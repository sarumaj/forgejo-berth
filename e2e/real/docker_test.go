// SPDX-License-Identifier: GPL-3.0-or-later

//go:build realforgejo

package real

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
	dockerprovider "git.ciepiela.org/dawid/forgejo-berth/internal/provider/docker"
)

// TestRealForgejoDocker runs the scenario through the docker provider on the engine the
// test's Forgejo runs on: each job gets a runner container and a Docker of its own, whose
// containers run the job (scripts/real-forgejo.sh docker).
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
	scenario(t, fmt.Sprintf(`hosts: [{name: real-e2e, address: %q, memory_mib: 4000, slots: [a, b, c]}]
provider:
  type: docker
  docker:
    image: %s
    forgejo_url: %s
    network: %s
    state_dir: %s
    boot_timeout: 120
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
		})
}
