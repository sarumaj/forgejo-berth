// SPDX-License-Identifier: GPL-3.0-or-later

//go:build realforgejo

package real

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
	incusprovider "git.ciepiela.org/dawid/forgejo-berth/internal/provider/incus"
	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
)

// incusVolume is a custom volume of the Incus host's pool default.
const incusVolume = "berth-e2e-real-e2e-%s-%s"

// volumeBuilds is the builds' lines in a cache volume of the project on the Incus host (""
// when it has none): read from the host's dir pool, the slot's container being reset.
func volumeBuilds(host, project, slotName, class string) string {
	name := fmt.Sprintf(incusVolume, slotName, class)
	out, _ := docker("exec", host, "cat", "/var/lib/incus/storage-pools/default/custom/"+project+"_"+name+"/builds")
	return out
}

// TestRealForgejoIncus runs the scenario through the incus provider on a real Incus
// (scripts/real-forgejo.sh incus): its API over HTTPS with a client certificate restricted
// to the slots' project and the server's certificate pinned, slots a and b its containers
// with forgejo-runner as a systemd service and a snapshot clean. Each slot has cache volumes of its own; after the
// first round, forgejo-berth pause holds slot b out of service, and forgejo-berth resume
// gives it back at the end.
func TestRealForgejoIncus(t *testing.T) {
	address := env(t, "BERTH_REAL_INCUS_ADDRESS")
	dir := env(t, "BERTH_REAL_INCUS_DIR")
	host := env(t, "BERTH_REAL_INCUS_HOST")
	inner := env(t, "BERTH_REAL_INNER_URL")
	project := env(t, "BERTH_REAL_INCUS_PROJECT")
	state := t.TempDir()
	ctx := context.Background()

	// The local socket answers too, and trusts its user.
	local, err := incusprovider.NewClient("unix://"+filepath.Join(dir, "incus.socket"), config.TLS{}, project)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Ping(ctx); err != nil {
		t.Fatalf("the unix socket: %v", err)
	}
	if _, err := incusprovider.NewInstance(local, "a", time.Minute).Running(ctx); err != nil {
		t.Fatalf("slot a through the unix socket: %v", err)
	}

	berth := filepath.Join(state, "forgejo-berth")
	if out, err := exec.Command("go", "build", "-o", berth, "../../cmd/forgejo-berth").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	template := filepath.Join(state, "runner-config.yml")
	if err := os.WriteFile(template, []byte(fmt.Sprintf(`log: {level: info}
runner: {capacity: 1, labels: @@RUNNER_LABELS@@}
host: {workdir_parent: /var/lib/forgejo-runner/work}
server:
  connections:
    forgejo: {url: %s, uuid: @@RUNNER_UUID@@, token_url: "file:///etc/forgejo-runner/token"}
`, inner)), 0o644); err != nil {
		t.Fatal(err)
	}
	var hosted slot.Hosted
	defer func() {
		if hosted != nil {
			hosted.Stop()
		}
	}()
	cycle := func(name string) provider.CycleStats {
		for _, c := range hosted.Cycles() {
			if c.Container == name {
				return c
			}
		}
		return provider.CycleStats{}
	}
	waitFor := func(name, want string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for cycle(name).State != want {
			if time.Now().After(deadline) {
				t.Fatalf("slot %s is %+v, not %s", name, cycle(name), want)
			}
			time.Sleep(time.Second)
		}
	}
	berthCommand := func(args ...string) {
		t.Helper()
		out, err := exec.Command(berth, args...).CombinedOutput()
		t.Logf("forgejo-berth %s: %s", strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("forgejo-berth %s: %v", args[0], err)
		}
	}

	var pausedJobs int
	var configFile string
	firstBuilds := map[string]string{}
	between := func(file string) {
		configFile = file
		// The first round's build left its line in its slot's volume, the slot since reset.
		for _, s := range []string{"a", "b"} {
			if lines := volumeBuilds(host, project, s, "trusted"); lines != "" {
				firstBuilds[s] = lines
			}
		}
		if len(firstBuilds) != 1 || strings.Join(slices.Collect(maps.Values(firstBuilds)), "") != "build\n" {
			t.Fatalf("the first build's lines in the cache volumes: %q", firstBuilds)
		}
		berthCommand("pause", "--config", configFile, "real-e2e/b")
		waitFor("b", provider.Paused)
		pausedJobs = cycle("b").Jobs
	}
	b, logBuffer := scenario(t, fmt.Sprintf(`hosts: [{name: real-e2e, address: %q, memory_mib: 4000, slots: [a, b]}]
provider:
  type: incus
  incus:
    tls: {ca_file: %s, cert_file: %s, key_file: %s}
    project: %s
    snapshot: clean
    config_template: %s
    config_path: /etc/forgejo-runner/config.yml
    token_path: /etc/forgejo-runner/token
    runner_service: forgejo-runner
    runner_user: runner
    work_dir: /var/lib/forgejo-runner/work
    labels:
      trusted: {ubuntu-latest: "ubuntu-latest:host", docker: "docker:host"}
      shared: {docker: "docker:host"}
    cache_volumes: {trusted: "berth-e2e-{host}-{slot}-trusted", shared: "berth-e2e-{host}-{slot}-shared"}
    cache_pool: default
    cache_path: /var/cache/forgejo-runner
    state_dir: %s
    boot_timeout: 120
`, address, filepath.Join(dir, "server.crt"), filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"),
		project, template, filepath.Join(state, "slots")),
		func(cfg *config.Config, log *slog.Logger) provider.Provider {
			h, err := incusprovider.New(cfg, log.With("test", "incus"))
			if err != nil {
				t.Fatal(err)
			}
			hosted = h
			return h
		}, between)

	// The paused slot ran nothing more; the other ran the second round.
	if c := cycle("b"); c.State != provider.Paused || c.Jobs != pausedJobs {
		t.Fatalf("the paused slot: %+v, %d jobs when paused", c, pausedJobs)
	}
	if c := cycle("a"); c.Jobs < 3 || c.Failures != 0 {
		t.Fatalf("slot a: %+v", c)
	}
	// Each slot's own volumes, made at their first use; the first build's line is still there,
	// and the second build (on a) added its own.
	out, err := docker("exec", host, "incus", "storage", "volume", "list", "default", "--project", project, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var volumes []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(out), &volumes); err != nil {
		t.Fatal(err)
	}
	var custom []string
	for _, v := range volumes {
		if v.Type == "custom" {
			custom = append(custom, v.Name)
		}
	}
	t.Logf("cache volumes %v", custom)
	if !slices.Contains(custom, fmt.Sprintf(incusVolume, "a", "trusted")) {
		t.Fatalf("no trusted cache volume of slot a: %v", custom)
	}
	for _, v := range custom {
		if !slices.Contains([]string{"a", "b"}, strings.Split(strings.TrimPrefix(v, "berth-e2e-real-e2e-"), "-")[0]) {
			t.Fatalf("a cache volume that is no slot's: %v", custom)
		}
	}
	for s, lines := range firstBuilds {
		now := volumeBuilds(host, project, s, "trusted")
		want := lines
		if s == "a" {
			want += "build\n"
		}
		if now != want {
			t.Fatalf("slot %s's cache volume has %q, want %q", s, now, want)
		}
	}
	if _, ok := firstBuilds["a"]; !ok && volumeBuilds(host, project, "a", "trusted") != "build\n" {
		t.Fatalf("slot a's cache volume has %q", volumeBuilds(host, project, "a", "trusted"))
	}

	// The runner's journal is in the broker's log at info, its task line among it.
	logs := logBuffer.String()
	if !strings.Contains(logs, "level=INFO msg=runner") || !strings.Contains(logs, " repo is alice/app") {
		t.Fatal("no runner journal at info in the log")
	}
	// The cycle saw a build run and measured its CPU use.
	cores := regexp.MustCompile(`msg=learned .* job=build .* cpu_cores=([0-9.]+)`).FindAllStringSubmatch(logs, -1)
	if len(cores) == 0 || !slices.ContainsFunc(cores, func(m []string) bool { return m[1] != "0" }) {
		t.Fatalf("no build's CPU use learned: %q", cores)
	}
	// The cycles are in the broker's metrics.
	metrics := b.Snapshot().Metrics()
	for _, want := range []string{
		`forgejo_berth_cycle_state{host="real-e2e",container="b",state="paused"} 1`,
		fmt.Sprintf(`forgejo_berth_cycle_jobs_total{host="real-e2e",container="a"} %d`, cycle("a").Jobs),
		`forgejo_berth_cycle_failures{host="real-e2e",container="a"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("no %s in the metrics:\n%s", want, metrics)
		}
	}
	t.Logf("cycles %+v", hosted.Cycles())

	// Resumed: the slot is reset from its snapshot and ready.
	berthCommand("resume", "--config", configFile, "real-e2e/b")
	waitFor("b", provider.Ready)

	// A job that forgejo-berth pause --stop cuts short ends as its runner reports it.
	cutShort(t, b, logBuffer, hosted.Cycles,
		func(container string) { berthCommand("pause", "--config", configFile, "--stop", "real-e2e/"+container) },
		func(container string) {
			berthCommand("resume", "--config", configFile, "real-e2e/"+container)
			waitFor(container, provider.Ready)
		})
}
