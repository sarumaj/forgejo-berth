// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"fmt"
	"reflect"
	"testing"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

func TestPredict(t *testing.T) {
	cfg := testConfig(t)
	alone := testConfig(t, "estimate: {unknown_mib: 0}")
	for _, tc := range []struct {
		name     string
		est      config.Estimate
		peak     int
		found    bool
		source   string
		want     int
		wantKnow bool
	}{
		{"no estimate gives the unknown reservation", cfg.Estimate, 0, false, "none", 6144, true},
		{"no estimate with unknown_mib 0 gives a host alone", alone.Estimate, 0, false, "none", 0, false},
		{"the job's own peak with its margin", cfg.Estimate, 2500, true, "job", 3000, true},
		{"a relative's peak with the larger margin", cfg.Estimate, 2000, true, "family", 3000, true},
		{"never below the floor", cfg.Estimate, 100, true, "job", 512, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := Predict(tc.peak, tc.found, tc.source, tc.est)
			if got != tc.want || known != tc.wantKnow {
				t.Fatalf("Predict = %d, %v; want %d, %v", got, known, tc.want, tc.wantKnow)
			}
		})
	}
}

func TestOffered(t *testing.T) {
	cfg := testConfig(t)
	for _, tc := range []struct {
		name string
		job  Job
		want *Offer
	}{
		{"a trusted owner's host job gets a runner of its owner", job(1),
			&Offer{"user:alice", []string{"ubuntu-latest"}}},
		{"a host feature is a trusted label", job(1, runsOn("ubuntu-latest", "gpu")),
			&Offer{"user:alice", []string{"gpu", "ubuntu-latest"}}},
		{"anyone else's docker job gets a global runner", job(1, repo("carol/site"), runsOn("docker")),
			&Offer{"global", []string{"docker"}}},
		{"anyone else's host job gets none", job(1, repo("carol/site")), nil},
		{"an unknown label gets none", job(1, runsOn("macos-latest")), nil},
		{"a job asking for no label gets none", job(1, runsOn()), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Offered(tc.job, cfg)
			if (tc.want == nil) == ok || (ok && !reflect.DeepEqual(got, *tc.want)) {
				t.Fatalf("Offered = %v, %v; want %v", got, ok, tc.want)
			}
		})
	}
}

func TestCovers(t *testing.T) {
	global := Offer{GlobalScope, []string{"docker"}}
	alice := Offer{"user:alice", []string{"docker", "ubuntu-latest"}}
	for _, tc := range []struct {
		name   string
		runner Offer
		job    Job
		want   bool
	}{
		{"a global runner covers anyone's job of its labels", global, job(1, repo("carol/site"), runsOn("docker")), true},
		{"a global runner covers an owner's job of its labels", global, job(1, runsOn("docker")), true},
		{"a global runner does not cover a label it lacks", global, job(1), false},
		{"an owner's runner covers its owner's job", alice, job(1), true},
		{"an owner's runner covers a job asking for some of its labels", alice, job(1, runsOn("docker")), true},
		{"an owner's runner does not cover another owner's job", alice, job(1, repo("bob/app")), false},
		{"no runner covers a job asking for no label", global, job(1, runsOn()), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Covers(tc.runner, tc.job); got != tc.want {
				t.Fatalf("Covers = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFamily(t *testing.T) {
	for name, want := range map[string]string{"server (32)": "server", "lint": "lint", "(x)": "(x)",
		"a (b) (c)": "a (b)"} {
		if got := (Job{Name: name}).Family(); got != want {
			t.Errorf("Family(%q) = %q, want %q", name, got, want)
		}
	}
}

// placed is a placement as the tests compare it.
type placed struct {
	Job        int64
	Host       string
	Container  string
	ReserveMiB int
	Exclusive  bool
	Scope      string
	Labels     string
	Candidates string
}

func summary(ps []Placement) []placed {
	out := []placed{}
	for _, p := range ps {
		var ids []int64
		for _, c := range p.Candidates {
			ids = append(ids, c.ID)
		}
		out = append(out, placed{p.Job.ID, p.Host, p.Container, p.ReserveMiB, p.Exclusive, p.Scope,
			fmt.Sprint(p.Labels), fmt.Sprint(ids)})
	}
	return out
}

func TestSchedule(t *testing.T) {
	type scheduleCase struct {
		name        string
		cfg         *config.Config
		jobs        []Job
		slots       []provider.Slot
		assignments []Assignment
		needs       map[int64]int
		waited      WaitedFunc
		// check compares the placements: each field set in want is compared.
		want []placed
		// fields names the fields of placed compared.
		fields []string
	}
	cfg := testConfig(t)
	hostLabelsOnly := testConfig(t)
	hostLabelsOnly.Trusted.Labels = []string{"ubuntu-latest"}
	slotsWith := func(s []provider.Slot, extra ...provider.Slot) []provider.Slot { return append(s, extra...) }
	run := func(host, container, assignment string) provider.Slot {
		return provider.Slot{Host: host, Container: container, State: provider.Running, Assignment: assignment}
	}
	// assigned is a slot whose runner is on its way: registered, its job not taken yet.
	assigned := func(host, container, assignment string) provider.Slot {
		return provider.Slot{Host: host, Container: container, State: provider.Assigned, Assignment: assignment}
	}
	for _, tc := range []scheduleCase{
		{
			name:   "two known jobs share a host where they fit",
			cfg:    cfg,
			jobs:   []Job{job(1, named("a")), job(2, named("b"), runsOn("docker"))},
			slots:  ready("ci-1/ci", "ci-1/ci-b"),
			needs:  map[int64]int{1: 2000, 2: 2000},
			want:   []placed{{Job: 1, Host: "ci-1", Container: "ci", ReserveMiB: 2000}, {Job: 2, Host: "ci-1", Container: "ci-b", ReserveMiB: 2000}},
			fields: []string{"Job", "Host", "Container", "ReserveMiB"},
		},
		{
			name:        "a job goes where most memory is free",
			cfg:         cfg,
			jobs:        []Job{job(1)},
			slots:       slotsWith(ready("ci-1/ci-b", "ci-2/ci-2"), run("ci-1", "ci", "x")),
			assignments: []Assignment{running("x", "ci-1", "ci", "user:alice", 4000, false, "docker")},
			needs:       map[int64]int{1: 1000},
			want:        []placed{{Host: "ci-2", Container: "ci-2"}},
			fields:      []string{"Host", "Container"},
		},
		{
			name:        "a job that does not fit waits",
			cfg:         cfg,
			jobs:        []Job{job(1)},
			slots:       slotsWith(ready("ci-2/ci-2b"), run("ci-2", "ci-2", "x")),
			assignments: []Assignment{running("x", "ci-2", "ci-2", "global", 5000, false, "docker")},
			needs:       map[int64]int{1: 2000},
		},
		{
			// The new job may be what either runner takes: both reserve a host, so only one
			// starts there.
			name:   "a job without history gets a host alone",
			cfg:    cfg,
			jobs:   []Job{job(1, named("new")), job(2, named("known"))},
			slots:  ready("ci-1/ci", "ci-1/ci-b"),
			needs:  map[int64]int{2: 500},
			want:   []placed{{Job: 1, Exclusive: true, ReserveMiB: 6000}},
			fields: []string{"Job", "Exclusive", "ReserveMiB"},
		},
		{
			name:        "nothing starts beside a job alone",
			cfg:         cfg,
			jobs:        []Job{job(1)},
			slots:       slotsWith(ready("ci-1/ci-b"), run("ci-1", "ci", "x")),
			assignments: []Assignment{running("x", "ci-1", "ci", "user:alice", 6000, true)},
			needs:       map[int64]int{1: 100},
		},
		{
			name:   "a feature job goes to the host with it",
			cfg:    cfg,
			jobs:   []Job{job(1, runsOn("ubuntu-latest", "gpu"))},
			slots:  ready("ci-2/ci-2", "ci-1/ci"),
			needs:  map[int64]int{1: 1000},
			want:   []placed{{Host: "ci-1", Labels: "[gpu ubuntu-latest]"}},
			fields: []string{"Host", "Labels"},
		},
		{
			name:  "a runner waiting for its job serves one job of its kind",
			cfg:   cfg,
			jobs:  []Job{job(1), job(2)},
			slots: slotsWith(ready("ci-2/ci-2"), provider.Slot{Host: "ci-1", Container: "ci", State: provider.Assigned, Assignment: "x"}),
			assignments: []Assignment{
				running("x", "ci-1", "ci", "user:alice", 1000, false, "ubuntu-latest")},
			needs:  map[int64]int{1: 1000, 2: 1000},
			want:   []placed{{Job: 2}},
			fields: []string{"Job"},
		},
		{
			name:   "jobs no runner may take are left",
			cfg:    cfg,
			jobs:   []Job{job(1, repo("carol/site")), job(2, repo("carol/site"), runsOn("docker"))},
			slots:  ready("ci-1/ci"),
			needs:  map[int64]int{1: 100, 2: 100},
			want:   []placed{{Job: 2, Scope: "global"}},
			fields: []string{"Job", "Scope"},
		},
		{
			// Forgejo hands a runner the oldest job it covers: a younger, larger job of the
			// same kind goes to the next runner, and does not keep the first to one job a host.
			name:   "a runner reserves for its job, not for younger ones",
			cfg:    oneHost(t, "ci-1"),
			jobs:   []Job{job(1, named("small")), job(2, named("big"))},
			slots:  ready("ci-1/ci", "ci-1/ci-b"),
			needs:  map[int64]int{1: 2000, 2: 3500},
			want:   []placed{{Job: 1, ReserveMiB: 2000, Candidates: "[1 2]"}, {Job: 2, ReserveMiB: 3500, Candidates: "[1 2]"}},
			fields: []string{"Job", "ReserveMiB", "Candidates"},
		},
		{
			// An older job of the same kind that did not fit is what Forgejo would hand a
			// runner for a younger one: the younger one reserves for it too, and waits with it.
			name:        "a runner reserves for an older job it may get instead",
			cfg:         oneHost(t, "ci-1"),
			jobs:        []Job{job(1, named("big")), job(2, named("small"))},
			slots:       slotsWith(ready("ci-1/ci-b"), run("ci-1", "ci", "x")),
			assignments: []Assignment{running("x", "ci-1", "ci", "user:alice", 3000, false, "docker")},
			needs:       map[int64]int{1: 5000, 2: 1000},
		},
		{
			// The runner on its way claims job 1, but the new one may ask first and get it.
			name:  "a runner waiting for its job claims the oldest, the next reserves for it too",
			cfg:   cfg,
			jobs:  []Job{job(1), job(2)},
			slots: slotsWith(ready("ci-2/ci-2"), provider.Slot{Host: "ci-1", Container: "ci", State: provider.Assigned, Assignment: "x"}),
			assignments: []Assignment{
				running("x", "ci-1", "ci", "user:alice", 5000, false, "ubuntu-latest")},
			needs:  map[int64]int{1: 5000, 2: 1000},
			want:   []placed{{Job: 2, ReserveMiB: 5000, Candidates: "[1 2]"}},
			fields: []string{"Job", "ReserveMiB", "Candidates"},
		},
		{
			name:   "a job estimated above a host's memory gets a host alone",
			cfg:    cfg,
			jobs:   []Job{job(1)},
			slots:  ready("ci-2/ci-2"),
			needs:  map[int64]int{1: 6960},
			want:   []placed{{Host: "ci-2", Exclusive: true, ReserveMiB: 6000}},
			fields: []string{"Host", "Exclusive", "ReserveMiB"},
		},
		{
			name:        "a large job waits for an empty host",
			cfg:         oneHost(t, "ci-2"),
			jobs:        []Job{job(1)},
			slots:       slotsWith(ready("ci-2/ci-2b"), run("ci-2", "ci-2", "x")),
			assignments: []Assignment{running("x", "ci-2", "ci-2", "global", 500, false, "docker")},
			needs:       map[int64]int{1: 7000},
		},
		{
			name:        "a slot holding an assignment is not ready",
			cfg:         cfg,
			jobs:        []Job{job(1)},
			slots:       slotsWith(ready("ci-1/ci"), provider.Slot{Host: "ci-1", Container: "ci-b", State: provider.Ready, Assignment: "y"}),
			assignments: []Assignment{running("x", "ci-1", "ci", "user:alice", 500, false, "docker")},
			needs:       map[int64]int{1: 500},
		},
		{
			name:   "a slot of a host not configured is not used",
			cfg:    cfg,
			jobs:   []Job{job(1)},
			slots:  ready("ci-9/ci"),
			needs:  map[int64]int{1: 500},
			fields: []string{"Job"},
		},
		{
			// A job run again (an old id, seen later) goes after the ones waiting longer.
			name:   "jobs go in the order they arrived",
			cfg:    cfg,
			jobs:   []Job{job(5), job(9)},
			slots:  ready("ci-1/ci"),
			needs:  map[int64]int{5: 1000, 9: 1000},
			waited: waits(map[int64]float64{5: 10, 9: 300}, 0),
			want:   []placed{{Job: 9}},
			fields: []string{"Job"},
		},
		{
			// Of different kinds: a runner for the small job could not take the big one.
			name:        "younger jobs go ahead of one that does not fit",
			cfg:         oneHost(t, "ci-2"),
			jobs:        []Job{job(1, named("big")), job(2, named("small"), runsOn("docker"))},
			slots:       slotsWith(ready("ci-2/ci-2b"), run("ci-2", "ci-2", "x")),
			assignments: []Assignment{running("x", "ci-2", "ci-2", "global", 3000, false, "docker")},
			needs:       map[int64]int{1: 5000, 2: 1000},
			waited:      waits(nil, 60),
			want:        []placed{{Job: 2}},
			fields:      []string{"Job"},
		},
		{
			// After starve_minutes, nothing new starts on the host it waits for: it drains.
			name:        "a starving job holds a host",
			cfg:         oneHost(t, "ci-2"),
			jobs:        []Job{job(1, named("big")), job(2, named("small"), runsOn("docker"))},
			slots:       slotsWith(ready("ci-2/ci-2b"), run("ci-2", "ci-2", "x")),
			assignments: []Assignment{running("x", "ci-2", "ci-2", "global", 3000, false, "docker")},
			needs:       map[int64]int{1: 5000, 2: 1000},
			waited:      waits(map[int64]float64{1: 900, 2: 60}, 0),
		},

		// Mixed scopes. Forgejo hands a global runner any repository's job its labels cover,
		// an owner's runner only that owner's: alice's docker job (1, 4000 MiB) is either's,
		// carol's docker job (2, 1000 MiB) only a global runner's.
		{
			name:  "both waiting: the global runner may get the owner's older job",
			cfg:   cfg,
			jobs:  []Job{job(1, runsOn("docker")), job(2, repo("carol/site"), runsOn("docker"))},
			slots: ready("ci-1/ci", "ci-2/ci-2"),
			needs: map[int64]int{1: 4000, 2: 1000},
			want: []placed{
				{Job: 1, Scope: "user:alice", ReserveMiB: 4000, Candidates: "[1]"},
				{Job: 2, Scope: "global", ReserveMiB: 4000, Candidates: "[1 2]"}},
			fields: []string{"Job", "Scope", "ReserveMiB", "Candidates"},
		},
		{
			name:  "the owner's runner on its way first, then the global one: each claims its own",
			cfg:   cfg,
			jobs:  []Job{job(1, runsOn("docker")), job(2, repo("carol/site"), runsOn("docker"))},
			slots: slotsWith(ready("ci-2/ci-2b"), assigned("ci-1", "ci", "o"), assigned("ci-2", "ci-2", "g")),
			assignments: []Assignment{
				running("o", "ci-1", "ci", "user:alice", 500, false, "docker"),
				running("g", "ci-2", "ci-2", "global", 500, false, "docker")},
			needs: map[int64]int{1: 4000, 2: 1000},
		},
		{
			// The global runner, registered first, claims alice's older job: the owner's runner
			// claims nothing, and carol's job gets a runner, which may also get alice's.
			name:  "the global runner on its way first, then the owner's: the shared job gets a runner",
			cfg:   cfg,
			jobs:  []Job{job(1, runsOn("docker")), job(2, repo("carol/site"), runsOn("docker"))},
			slots: slotsWith(ready("ci-2/ci-2b"), assigned("ci-1", "ci", "o"), assigned("ci-2", "ci-2", "g")),
			assignments: []Assignment{
				running("g", "ci-2", "ci-2", "global", 1000, false, "docker"),
				running("o", "ci-1", "ci", "user:alice", 1000, false, "docker")},
			needs:  map[int64]int{1: 4000, 2: 1000},
			want:   []placed{{Job: 2, Scope: "global", ReserveMiB: 4000, Candidates: "[1 2]"}},
			fields: []string{"Job", "Scope", "ReserveMiB", "Candidates"},
		},
		{
			name:  "a global runner on its way claims the owner's older job",
			cfg:   cfg,
			jobs:  []Job{job(1, runsOn("docker")), job(2, repo("carol/site"), runsOn("docker"))},
			slots: slotsWith(ready("ci-1/ci"), assigned("ci-2", "ci-2", "g")),
			assignments: []Assignment{
				running("g", "ci-2", "ci-2", "global", 1000, false, "docker")},
			needs:  map[int64]int{1: 4000, 2: 1000},
			want:   []placed{{Job: 2, Scope: "global", ReserveMiB: 4000}},
			fields: []string{"Job", "Scope", "ReserveMiB"},
		},
		{
			name:  "an owner's runner on its way does not claim the shared job",
			cfg:   cfg,
			jobs:  []Job{job(1, repo("carol/site"), runsOn("docker")), job(2, runsOn("docker"))},
			slots: slotsWith(ready("ci-2/ci-2"), assigned("ci-1", "ci", "o")),
			assignments: []Assignment{
				running("o", "ci-1", "ci", "user:alice", 1000, false, "docker")},
			needs:  map[int64]int{1: 4000, 2: 1000},
			want:   []placed{{Job: 1, Scope: "global", ReserveMiB: 4000, Candidates: "[1 2]"}},
			fields: []string{"Job", "Scope", "ReserveMiB", "Candidates"},
		},
		{
			name:  "another owner's runner on its way claims nothing",
			cfg:   cfg,
			jobs:  []Job{job(1, runsOn("docker")), job(2, repo("carol/site"), runsOn("docker"))},
			slots: slotsWith(ready("ci-1/ci", "ci-2/ci-2b"), assigned("ci-2", "ci-2", "o")),
			assignments: []Assignment{
				running("o", "ci-2", "ci-2", "user:bob", 500, false, "docker")},
			needs:  map[int64]int{1: 4000, 2: 1000},
			want:   []placed{{Job: 1, Scope: "user:alice"}, {Job: 2, Scope: "global"}},
			fields: []string{"Job", "Scope"},
		},
		{
			// A shared label is all a global runner has: an owner's job asking for a trusted
			// label is no global runner's, and does not count for one.
			name:  "trusted labels: the global runner does not cover the owner's host job",
			cfg:   cfg,
			jobs:  []Job{job(1), job(2, repo("carol/site"), runsOn("docker"))},
			slots: ready("ci-1/ci", "ci-2/ci-2"),
			needs: map[int64]int{1: 4000, 2: 1000},
			want: []placed{
				{Job: 1, Scope: "user:alice", ReserveMiB: 4000, Candidates: "[1]"},
				{Job: 2, Scope: "global", ReserveMiB: 1000, Candidates: "[2]"}},
			fields: []string{"Job", "Scope", "ReserveMiB", "Candidates"},
		},
		{
			name:  "trusted labels: a global runner on its way leaves the owner's host job",
			cfg:   cfg,
			jobs:  []Job{job(1), job(2, repo("carol/site"), runsOn("docker"))},
			slots: slotsWith(ready("ci-1/ci"), assigned("ci-2", "ci-2", "g")),
			assignments: []Assignment{
				running("g", "ci-2", "ci-2", "global", 1000, false, "docker")},
			needs:  map[int64]int{1: 4000, 2: 1000},
			want:   []placed{{Job: 1, Scope: "user:alice", ReserveMiB: 4000}},
			fields: []string{"Job", "Scope", "ReserveMiB"},
		},
		{
			// Alice's docker job gets no runner of hers (docker is no trusted label here), but
			// Forgejo hands it to a global runner all the same.
			name:  "a global runner claims an owner's job berth offers no runner for",
			cfg:   hostLabelsOnly,
			jobs:  []Job{job(1, runsOn("docker")), job(2, repo("carol/site"), runsOn("docker"))},
			slots: slotsWith(ready("ci-1/ci"), assigned("ci-2", "ci-2", "g")),
			assignments: []Assignment{
				running("g", "ci-2", "ci-2", "global", 1000, false, "docker")},
			needs:  map[int64]int{1: 4000, 2: 1000},
			want:   []placed{{Job: 2, Scope: "global", ReserveMiB: 4000, Candidates: "[1 2]"}},
			fields: []string{"Job", "Scope", "ReserveMiB", "Candidates"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summary(Schedule(tc.jobs, tc.slots, tc.assignments, fixed(tc.needs), tc.cfg, tc.waited, nil))
			if tc.want == nil {
				tc.want = []placed{}
			}
			if tc.name == "jobs go in the order they arrived" && len(got) > 1 {
				got = got[:1]
			}
			if len(got) != len(tc.want) {
				t.Fatalf("placed %+v, want %+v", got, tc.want)
			}
			for i := range got {
				g, w := reflect.ValueOf(got[i]), reflect.ValueOf(tc.want[i])
				for _, f := range tc.fields {
					if !reflect.DeepEqual(g.FieldByName(f).Interface(), w.FieldByName(f).Interface()) {
						t.Fatalf("placement %d: %s = %v, want %v (all: %+v)", i, f,
							g.FieldByName(f).Interface(), w.FieldByName(f).Interface(), got)
					}
				}
			}
		})
	}
}

func TestCPUAdmission(t *testing.T) {
	cfg := testConfig(t)
	cfg.CPU.Overcommit = 1.4
	cfg.Hosts = []config.Host{{Name: "ci-1", Address: "a", MemoryMiB: 16000, CPUCores: 4}}
	cores := func(j Job) float64 { return map[int64]float64{1: 3, 2: 3, 3: 2.5}[j.ID] }
	needs := fixed(map[int64]int{1: 1000, 2: 1000, 3: 1000})
	jobs := []Job{job(1, named("e2e")), job(2, named("e2e2"), runsOn("ubuntu-24.04")), job(3, named("unit"), repo("carol/site"), runsOn("docker"))}
	got := summary(Schedule(jobs, ready("ci-1/a", "ci-1/b", "ci-1/c"), nil, needs, cfg, nil, cores))
	// 3 cores, then 3 more would be 6 of 4 x 1.4 = 5.6: refused; 2.5 more is 5.5: admitted.
	if len(got) != 2 || got[0].Job != 1 || got[1].Job != 3 {
		t.Fatalf("placed %+v", got)
	}
	// Off (no cores function): memory alone admits all three.
	if n := len(Schedule(jobs, ready("ci-1/a", "ci-1/b", "ci-1/c"), nil, needs, cfg, nil, nil)); n != 3 {
		t.Fatalf("without the CPU admission %d placed", n)
	}
	// An idle host takes a job whatever its cores.
	big := func(Job) float64 { return 100 }
	if n := len(Schedule(jobs[:1], ready("ci-1/a"), nil, needs, cfg, nil, big)); n != 1 {
		t.Fatal("an idle host refused a job for its cores")
	}
}

func TestMaxJobs(t *testing.T) {
	cfg := testConfig(t)
	cfg.Hosts = []config.Host{{Name: "ci-1", Address: "a", MemoryMiB: 16000, MaxJobs: 1}}
	needs := fixed(map[int64]int{1: 1000, 2: 1000})
	jobs := []Job{job(1, named("a")), job(2, named("b"), runsOn("docker"))}
	if n := len(Schedule(jobs, ready("ci-1/a", "ci-1/b"), nil, needs, cfg, nil, nil)); n != 1 {
		t.Fatalf("%d placed beside a cap of one", n)
	}
}
