// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"git.ciepiela.org/dawid/forgejo-berth/internal/history"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// unusedRound is one runner's life for the waiting jobs on ci-1/ci: the slot is ready, the
// broker starts a runner (when it does), and after minutes the slot reports it unused while
// the jobs still wait. It returns the assignment, "" when no runner was started.
func unusedRound(t *testing.T, h *harness, minutes float64) string {
	t.Helper()
	h.slots.slots = map[string][]provider.Slot{"ci-1": ready("ci-1/ci"), "ci-2": {}}
	before := len(h.slots.assigned)
	h.tick(t)
	if len(h.slots.assigned) == before {
		return ""
	}
	id := h.slots.assigned[len(h.slots.assigned)-1].Assignment
	h.advance(minutes * 60)
	h.slots.slots["ci-1"] = []provider.Slot{{Host: "ci-1", Container: "ci", State: provider.Resetting,
		Finished: id, Unused: true}}
	h.tick(t)
	h.advance(10)
	return id
}

func TestTheBackoffGrowsAndCaps(t *testing.T) {
	h := newHarness(t, "backoff: {unused_runs: 3, base_minutes: 15, max_minutes: 60}")
	for _, tc := range []struct {
		unused  int
		minutes float64
	}{{3, 15}, {4, 30}, {5, 60}, {6, 60}, {64, 60}, {5000, 60}} {
		if got := h.backoffSeconds(tc.unused) / 60; got != tc.minutes {
			t.Errorf("%d unused: %g minutes, want %g", tc.unused, got, tc.minutes)
		}
	}
}

func TestBackoff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		// rounds are the minutes each runner lived unused.
		rounds []float64
		// unused is the job's count after them, backedOff whether it is held back, minutes
		// how long.
		unused    int
		backedOff bool
		minutes   float64
	}{
		{name: "fewer than unused_runs runners: no back-off", rounds: []float64{16, 16},
			unused: 2},
		{name: "unused_runs runners in a row: backed off for base_minutes", rounds: []float64{16, 16, 16},
			unused: 3, backedOff: true, minutes: 15},
		{name: "unused_runs of 2", extra: []string{"backoff: {unused_runs: 2}"}, rounds: []float64{16, 16},
			unused: 2, backedOff: true, minutes: 15},
		{name: "base_minutes follows unused_minutes", extra: []string{"unused_minutes: 5"},
			rounds: []float64{6, 6, 6}, unused: 3, backedOff: true, minutes: 5},
		{name: "a runner stopped before unused_minutes does not count (released, paused)",
			rounds: []float64{16, 3, 16}, unused: 2},
		{name: "turned off", extra: []string{"backoff: {unused_runs: 0}"}, rounds: []float64{16, 16, 16, 16}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.extra...)
			h.forge.jobs = []Job{job(7)}
			for i, m := range tc.rounds {
				if unusedRound(t, h, m) == "" {
					t.Fatalf("round %d started no runner", i+1)
				}
			}
			e, kept := h.Backoffs()[7]
			if e.Unused != tc.unused || e.active(h.clock()) != tc.backedOff || kept != (tc.unused > 0) {
				t.Fatalf("back-off %+v (kept %v), want %d unused, backed off %v", e, kept, tc.unused, tc.backedOff)
			}
			// The look after the last round's came 10 seconds after it.
			if got := (e.Until - h.clock() + 10) / 60; tc.backedOff && got != tc.minutes {
				t.Errorf("backed off for %g minutes, want %g", got, tc.minutes)
			}
			want := 0
			if tc.backedOff {
				want = 1
				if !strings.Contains(h.logs.String(), "backed off: its runners go unused") {
					t.Error("the back-off is not logged")
				}
			}
			if h.Counts()["backed_off"] != want || h.Snapshot().BackedOff() != want {
				t.Errorf("counts %v, state %+v", h.Counts(), h.Snapshot().Backoffs)
			}
			if !strings.Contains(h.Snapshot().Metrics(), fmt.Sprintf("forgejo_berth_backed_off_jobs %d\n", want)) {
				t.Errorf("the gauge:\n%s", h.Snapshot().Metrics())
			}
			// The next look starts a runner unless the job is held back.
			registered := len(h.forge.registered)
			if unusedRound(t, h, 16); tc.backedOff != (len(h.forge.registered) == registered) {
				t.Fatalf("backed off %v, but registered %d after %d", tc.backedOff, len(h.forge.registered), registered)
			}
		})
	}
}

// backedOff7 is job 7 backed off after three unused runners (base 15, cap 60 minutes).
func backedOff7(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, "backoff: {unused_runs: 3, base_minutes: 15, max_minutes: 60}")
	h.forge.jobs = []Job{job(7)}
	for range 3 {
		unusedRound(t, h, 16)
	}
	if !h.backedOff(7, h.clock()) {
		t.Fatalf("not backed off: %+v", h.Backoffs())
	}
	return h
}

func TestTheBackoffDoublesEachTimeTheJobIsTriedAgainInVain(t *testing.T) {
	h := backedOff7(t)
	var got []float64
	for range 4 {
		e := h.Backoffs()[7]
		h.advance(e.Until - h.clock() + 1)
		at := h.clock()
		if unusedRound(t, h, 16) == "" {
			t.Fatal("not tried again after the back-off")
		}
		got = append(got, (h.Backoffs()[7].Until-at-16*60)/60)
	}
	if want := []float64{30, 60, 60, 60}; !reflect.DeepEqual(got, want) {
		t.Fatalf("back-offs %v minutes, want %v", got, want)
	}
	if !strings.Contains(h.logs.String(), "back-off over; the job is tried again") {
		t.Fatal("the retry is not logged")
	}
	if h.Counts()["backed_off"] != 5 || h.Backoffs()[7].Unused != 7 {
		t.Fatalf("counts %v, back-off %+v", h.Counts(), h.Backoffs()[7])
	}
}

func TestTheBackoffIsReset(t *testing.T) {
	for _, tc := range []struct {
		name string
		// then is what happens once the job is tried again.
		then func(t *testing.T, h *harness)
		log  string
	}{
		{name: "a runner ran the job", log: "a backed-off job ran; back-off reset",
			then: func(t *testing.T, h *harness) {
				h.slots.slots["ci-1"] = ready("ci-1/ci")
				h.tick(t)
				id := h.slots.assigned[len(h.slots.assigned)-1].Assignment
				h.forge.tasks = map[int64]int64{555: 7}
				h.forge.ends = map[int64]string{555: "cancelled"}
				h.slots.slots["ci-1"] = []provider.Slot{finished("ci-1", "ci", id, 555, 100, 5)}
				h.advance(10)
				h.tick(t)
			}},
		{name: "the job waits no more", log: "a backed-off job waits no more",
			then: func(t *testing.T, h *harness) {
				h.forge.jobs = nil
				h.tick(t)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := backedOff7(t)
			if tc.name == "a runner ran the job" {
				h.advance(h.Backoffs()[7].Until - h.clock() + 1)
			}
			tc.then(t, h)
			if len(h.Backoffs()) != 0 {
				t.Fatalf("back-offs %+v", h.Backoffs())
			}
			if kept, _ := h.history.Backoffs(); len(kept) != 0 {
				t.Fatalf("kept %v", kept)
			}
			if !strings.Contains(h.logs.String(), tc.log) {
				t.Fatalf("the log lacks %q", tc.log)
			}
			// It counts anew: a runner at once, and three more before the next back-off.
			h.forge.jobs = []Job{job(7)}
			for i := range 2 {
				if unusedRound(t, h, 16) == "" {
					t.Fatalf("round %d: no runner", i+1)
				}
			}
			if e := h.Backoffs()[7]; e.Unused != 2 || e.active(h.clock()) {
				t.Fatalf("back-off %+v", e)
			}
		})
	}
}

func TestARestartedBrokerKeepsTheBackoff(t *testing.T) {
	h := backedOff7(t)
	again, err := New(h.cfg, Options{Forge: h.forge, Provider: h.slots, History: h.history, Log: h.log, Now: h.now})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Backoffs(), h.Backoffs()) {
		t.Fatalf("kept %+v, want %+v", again.Backoffs(), h.Backoffs())
	}
	h.Broker = again
	if unusedRound(t, h, 16) != "" {
		t.Fatal("the restarted broker started a runner for the backed-off job")
	}
	// A kept back-off that cannot be read is forgotten.
	if err := h.history.KeepBackoff(9, []byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(h.cfg, Options{Forge: h.forge, Provider: h.slots, History: h.history, Log: h.log, Now: h.now}); err != nil {
		t.Fatal(err)
	}
	if kept, _ := h.history.Backoffs(); len(kept) != 1 {
		t.Fatalf("kept %v", kept)
	}
}

func TestABackedOffJobHoldsNoReservationAndDoesNotStarve(t *testing.T) {
	for _, backedOff := range []bool{false, true} {
		h := newHarness(t)
		h.cfg.Hosts = h.cfg.Hosts[:1]
		// Job 7 has no history: it needs the host alone, which runs another job, and has
		// waited past starve_minutes. Job 8 is small.
		if err := h.history.Record(history.Run{Repo: "carol/site", Name: "test", Family: "test", At: 999_000,
			PeakMiB: 1000, Host: "ci-1"}); err != nil {
			t.Fatal(err)
		}
		h.assigned["busy"] = Assignment{ID: "busy", Host: "ci-1", Container: "a", Scope: "global",
			Labels: []string{"docker"}, ReserveMiB: 3000, Since: h.clock()}
		h.forge.jobs = []Job{job(7), job(8, repo("carol/site"), runsOn("docker"))}
		h.firstSeen[7] = h.clock() - 3600
		if backedOff {
			h.keepBackoff(Backoff{Job: job(7), Unused: 3, Until: h.clock() + 600})
		}
		h.slots.slots = map[string][]provider.Slot{"ci-1": {
			{Host: "ci-1", Container: "a", State: provider.Running, Assignment: "busy"},
			{Host: "ci-1", Container: "b", State: provider.Ready}}}
		h.tick(t)
		var got []string
		for _, a := range h.Assignments() {
			got = append(got, a.Container)
		}
		want := []string{"a"}
		if backedOff {
			want = []string{"a", "b"}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("backed off %v: assignments %v, want %v", backedOff, got, want)
		}
	}
}

func TestABackedOffJobIsNotClaimedButStaysACandidate(t *testing.T) {
	// Alice's job 7 asks for shared labels only: a global runner covers it (Forgejo may hand
	// it alice's job). Backed off, it is neither the runner's to claim nor reserved for, but
	// if Forgejo hands it out after all, the task is found among the runner's candidates.
	h := newHarness(t)
	h.forge.jobs = []Job{job(7, runsOn("docker")), job(8, repo("carol/site"), runsOn("docker"))}
	h.keepBackoff(Backoff{Job: h.forge.jobs[0], Unused: 3, Until: h.clock() + 600})
	h.slots.slots = map[string][]provider.Slot{"ci-1": ready("ci-1/a", "ci-1/b"), "ci-2": {}}
	h.tick(t)
	a := h.Assignments()
	if len(a) != 1 || a[0].Scope != "global" || a[0].ReserveMiB != 6000 {
		t.Fatalf("assignments %+v", a)
	}
	var candidates []int64
	for _, c := range a[0].Candidates {
		candidates = append(candidates, c.ID)
	}
	if !reflect.DeepEqual(candidates, []int64{8, 7}) {
		t.Fatalf("candidates %v", candidates)
	}
	// Carol's job went to another runner: the runner is released, the backed-off job does
	// not keep it.
	h.forge.jobs = h.forge.jobs[:1]
	h.slots.slots["ci-1"] = []provider.Slot{{Host: "ci-1", Container: a[0].Container, State: provider.Assigned,
		Assignment: a[0].ID}}
	for range 3 {
		h.advance(40)
		h.tick(t)
	}
	if len(h.slots.released) != 1 {
		t.Fatalf("released %v", h.slots.released)
	}
	// Forgejo handed it the backed-off job after all: found, and the back-off is reset.
	h.forge.tasks = map[int64]int64{555: 7}
	h.slots.slots["ci-1"] = []provider.Slot{finished("ci-1", a[0].Container, a[0].ID, 555, 800, 30)}
	h.tick(t)
	if len(h.Backoffs()) != 0 || h.Counts()["learned"] != 1 {
		t.Fatalf("back-offs %+v, counts %v", h.Backoffs(), h.Counts())
	}
}

func TestAGlobalRunnerGoneUnusedCountsForEveryJobItCovered(t *testing.T) {
	h := newHarness(t)
	h.forge.jobs = []Job{job(5, repo("carol/site"), runsOn("docker")), job(7, runsOn("docker"))}
	for range 3 {
		unusedRound(t, h, 16)
	}
	for _, id := range []int64{5, 7} {
		if e := h.Backoffs()[id]; e.Unused != 3 || !e.active(h.clock()) {
			t.Fatalf("job %d: %+v", id, e)
		}
	}
	state, _ := json.Marshal(h.Snapshot())
	if !strings.Contains(string(state), `"backoffs":[{"job_id":5,"run_id":50,"repo":"carol/site","name":"test",`+
		`"unused":3,"backed_off":true,`) {
		t.Fatalf("state %s", state)
	}
}

func TestShadowModeComputesTheBackoffAndKeepsNothing(t *testing.T) {
	live := newHarness(t)
	ro, err := history.Open(live.path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	shadowForge := &fakeForge{jobs: []Job{job(7)}}
	logs := &strings.Builder{}
	shadow, err := New(live.cfg, Options{Forge: shadowForge, Provider: live.slots, History: ro,
		Log: slog.New(slog.NewTextHandler(logs, nil)), Now: live.now})
	if err != nil {
		t.Fatal(err)
	}
	tick := func() {
		t.Helper()
		if err := shadow.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	live.forge.jobs = []Job{job(7)}
	for range 3 {
		live.slots.slots = map[string][]provider.Slot{"ci-1": ready("ci-1/ci"), "ci-2": {}}
		live.tick(t)
		id := live.slots.assigned[len(live.slots.assigned)-1].Assignment
		tick()
		live.advance(16 * 60)
		live.slots.slots["ci-1"] = []provider.Slot{{Host: "ci-1", Container: "ci", State: provider.Resetting,
			Finished: id, Unused: true}}
		// The shadow looks twice before the live instance settles: it counts the runner once.
		tick()
		tick()
		live.tick(t)
		live.advance(10)
	}
	if e := shadow.Backoffs()[7]; e.Unused != 3 || !e.active(shadow.clock()) {
		t.Fatalf("shadow back-off %+v", e)
	}
	if !reflect.DeepEqual(shadow.Backoffs(), live.Backoffs()) {
		t.Fatalf("shadow %+v, live %+v", shadow.Backoffs(), live.Backoffs())
	}
	if !strings.Contains(logs.String(), "would back off") || shadow.Counts()["backed_off"] != 1 {
		t.Fatalf("counts %v, log %s", shadow.Counts(), logs.String())
	}
	// It would start no runner for the job while it is held back.
	live.slots.slots["ci-1"] = ready("ci-1/ci")
	tick()
	if shadow.Counts()["would_assign"] != 0 {
		t.Fatalf("counts %v", shadow.Counts())
	}
	// Kept by the live instance only: the shadow wrote nothing.
	shadowForge.jobs = nil
	tick()
	if kept, _ := live.history.Backoffs(); len(kept) != 1 || len(shadow.Backoffs()) != 0 {
		t.Fatalf("kept %v, shadow %+v", kept, shadow.Backoffs())
	}
}
