// SPDX-License-Identifier: GPL-3.0-or-later

//go:build realforgejo

package real

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
	dockerprovider "git.ciepiela.org/dawid/forgejo-berth/internal/provider/docker"
	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
)

// TestRealForgejoDocker runs the scenario through the docker provider on the engine the
// test's Forgejo runs on: each job gets a runner container and a Docker of its own, whose
// containers run the job (scripts/real-forgejo.sh docker). Each slot has a cache volume of
// its own, and slot c is paused throughout: it runs none of the jobs, and is ready once
// resumed.
func TestRealForgejoDocker(t *testing.T) {
	address := env(t, "BERTH_REAL_DOCKER_HOST")
	network := env(t, "BERTH_REAL_NETWORK")
	inner := env(t, "BERTH_REAL_INNER_URL")
	image := env(t, "BERTH_REAL_RUNNER_IMAGE")
	state := t.TempDir()
	var hosted interface{ Stop() }
	defer func() {
		if hosted != nil {
			hosted.Stop()
		}
		out, _ := docker("ps", "-aq", "--filter", "label="+dockerprovider.LabelHost+"=real-e2e")
		if ids := strings.Fields(out); len(ids) > 0 {
			_, _ = docker(append([]string{"rm", "-f", "-v"}, ids...)...)
		}
		out, _ = docker("volume", "ls", "-q", "--filter", "label="+dockerprovider.LabelHost+"=real-e2e")
		if ids := strings.Fields(out); len(ids) > 0 {
			_, _ = docker(append([]string{"volume", "rm", "-f"}, ids...)...)
		}
	}()
	jobImage := "docker://" + os.Getenv("BERTH_REAL_JOB_IMAGE")
	if jobImage == "docker://" {
		jobImage = "docker://docker.io/library/alpine:3"
	}
	// Paused before the broker starts, as a pause survives its restarts.
	if err := os.MkdirAll(filepath.Join(state, "real-e2e"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "real-e2e", "c.pause"), []byte("finish\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, logs := scenario(t, fmt.Sprintf(`hosts: [{name: real-e2e, address: %q, memory_mib: 4000, slots: [a, b, c]}]
provider:
  type: docker
  docker:
    image: %s
    forgejo_url: %s
    network: %s
    state_dir: %s
    boot_timeout: 120
    cache_volumes: {trusted: "berth-e2e-cache-{host}-{slot}-trusted", shared: "berth-e2e-cache-{host}-{slot}-shared"}
    labels:
      trusted: {ubuntu-latest: "ubuntu-latest:%s", docker: "docker:%s"}
      shared: {docker: "docker:%s"}
`, address, image, inner, network, state, jobImage, jobImage, jobImage),
		func(cfg *config.Config, log *slog.Logger) provider.Provider {
			h, err := dockerprovider.New(cfg, log.With("test", "docker"))
			if err != nil {
				t.Fatal(err)
			}
			hosted = h
			return h
		}, nil)

	cycles := map[string]provider.CycleStats{}
	jobs := 0
	for _, c := range hosted.(slot.Hosted).Cycles() {
		cycles[c.Container] = c
		jobs += c.Jobs
	}
	if c := cycles["c"]; c.State != provider.Paused || c.Jobs != 0 || jobs < 7 {
		t.Fatalf("cycles %+v", cycles)
	}
	out, err := docker("volume", "ls", "-q", "--filter", "label="+dockerprovider.LabelCache)
	if err != nil {
		t.Fatal(err)
	}
	volumes := strings.Fields(out)
	for _, v := range volumes {
		if strings.Contains(v, "-real-e2e-c-") {
			t.Fatalf("the paused slot got a cache volume: %v", volumes)
		}
	}
	if !slices.Contains(volumes, "berth-e2e-cache-real-e2e-a-trusted") &&
		!slices.Contains(volumes, "berth-e2e-cache-real-e2e-b-trusted") {
		t.Fatalf("no slot's own cache volume: %v", volumes)
	}
	t.Logf("cycles %+v, cache volumes %v", cycles, volumes)

	// Resumed: the slot is reset and ready.
	slots := hosted.(slot.Hosted)
	host := config.Host{Name: "real-e2e"}
	resume := func(container string) {
		if _, err := slots.Resume(context.Background(), host, container); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Minute)
		for {
			state := ""
			for _, c := range slots.Cycles() {
				if c.Container == container {
					state = c.State
				}
			}
			if state == provider.Ready {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the resumed slot is %s", state)
			}
			time.Sleep(time.Second)
		}
	}
	resume("c")

	// A job that a pause at once cuts short ends as its runner reports it.
	cutShort(t, b, logs, slots.Cycles, func(container string) {
		if _, err := slots.Pause(context.Background(), host, container, true); err != nil {
			t.Fatal(err)
		}
	}, resume)
}
