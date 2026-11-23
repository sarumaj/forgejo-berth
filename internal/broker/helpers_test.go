// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"strings"
	"testing"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// baseConfig is two hosts of 6000 MiB each, ci-1 with a GPU; alice and bob trusted.
const baseConfig = `
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
trusted:
  owners: {alice: alice.token, bob: bob.token}
  labels: [ubuntu-latest, ubuntu-24.04, docker]
shared:
  labels: [docker]
hosts:
  - {name: ci-1, address: ci-1.example.org, memory_mib: 6000, features: [gpu]}
  - {name: ci-2, address: ci-2.example.org, memory_mib: 6000}
`

// testConfig is baseConfig with the YAML lines in extra appended (they override its keys).
func testConfig(t *testing.T, extra ...string) *config.Config {
	t.Helper()
	text := baseConfig + strings.Join(extra, "\n")
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// oneHost is a configuration with the single host name, of 6000 MiB.
func oneHost(t *testing.T, name string) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Hosts = []config.Host{{Name: name, Address: "a", MemoryMiB: 6000}}
	return cfg
}

// job is a waiting job of the repository (alice/app by default) asking for ubuntu-latest.
func job(id int64, opts ...func(*Job)) Job {
	j := Job{ID: id, RunID: id * 10, RepoID: 1, Owner: "alice", Repo: "alice/app", Name: "test",
		RunsOn: []string{"ubuntu-latest"}}
	for _, o := range opts {
		o(&j)
	}
	return j
}

func named(name string) func(*Job) { return func(j *Job) { j.Name = name } }

func repo(r string) func(*Job) {
	return func(j *Job) { j.Repo, j.Owner = r, strings.Split(r, "/")[0] }
}

func runsOn(labels ...string) func(*Job) { return func(j *Job) { j.RunsOn = labels } }

// ready are ready slots, as host/container.
func ready(names ...string) []provider.Slot {
	var slots []provider.Slot
	for _, n := range names {
		host, container, _ := strings.Cut(n, "/")
		slots = append(slots, provider.Slot{Host: host, Container: container, State: provider.Ready})
	}
	return slots
}

// fixed is a reserve function from job ids (missing: no history, a host alone).
func fixed(needs map[int64]int) ReserveFunc {
	return func(j Job) (int, bool) {
		n, ok := needs[j.ID]
		return n, ok
	}
}

// waits is a waited function from job ids, with a default.
func waits(seconds map[int64]float64, def float64) WaitedFunc {
	return func(j Job) float64 {
		if s, ok := seconds[j.ID]; ok {
			return s
		}
		return def
	}
}

func running(id, host, container, scope string, reserve int, exclusive bool, labels ...string) Assignment {
	return Assignment{ID: id, Host: host, Container: container, Scope: scope, RunnerID: 9,
		Labels: labels, ReserveMiB: reserve, Exclusive: exclusive}
}
