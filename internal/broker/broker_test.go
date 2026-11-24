// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/history"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// fakeForge is Forgejo's side: waiting jobs, the runners registered and deleted, which job
// a task was.
type fakeForge struct {
	jobs       []Job
	registered [][2]string
	deleted    []string
	tasks      map[int64]int64
	ends       map[int64]string
	listed     []Runner
	taskErr    error
}

func (f *fakeForge) WaitingJobs(context.Context) ([]Job, error) { return slices.Clone(f.jobs), nil }

func (f *fakeForge) Register(_ context.Context, scope, name string) (Registered, error) {
	f.registered = append(f.registered, [2]string{scope, name})
	n := len(f.registered)
	return Registered{ID: int64(n), UUID: fmt.Sprintf("uuid-%d", n), Token: fmt.Sprintf("secret-token-%d", n)}, nil
}

func (f *fakeForge) Delete(_ context.Context, scope string, id int64) error {
	f.deleted = append(f.deleted, fmt.Sprintf("%s/%d", scope, id))
	return nil
}

func (f *fakeForge) Runners(context.Context) ([]Runner, error) { return f.listed, nil }

func (f *fakeForge) TaskJob(_ context.Context, task int64, candidates []Job) (Job, string, bool, error) {
	if f.taskErr != nil {
		return Job{}, "", false, f.taskErr
	}
	for _, c := range candidates {
		if f.tasks[task] == c.ID {
			end := f.ends[task]
			if end == "" {
				end = "success"
			}
			return c, end, true, nil
		}
	}
	return Job{}, "", false, nil
}

// fakeSlots is the hosts' slots: their status as set, the handoffs given to them.
type fakeSlots struct {
	slots    map[string][]provider.Slot
	assigned []provider.Handoff
	where    []string
	down     map[string]bool
	released []string
	refuse   error
	// cycles are the slots' cycles, as a provider that runs them reports them.
	cycles []provider.CycleStats
}

func (f *fakeSlots) Cycles() []provider.CycleStats { return f.cycles }

func (f *fakeSlots) Status(_ context.Context, h config.Host) ([]provider.Slot, error) {
	if f.down[h.Name] {
		return nil, errors.New("connection timed out")
	}
	return slices.Clone(f.slots[h.Name]), nil
}

// Assign records the handoff; the slot shows it as assigned at the next status.
func (f *fakeSlots) Assign(_ context.Context, h config.Host, container string, ho provider.Handoff) error {
	if f.refuse != nil {
		return f.refuse
	}
	f.assigned = append(f.assigned, ho)
	f.where = append(f.where, h.Name+"/"+container)
	for i, s := range f.slots[h.Name] {
		if s.Container == container {
			f.slots[h.Name][i] = provider.Slot{Host: h.Name, Container: container, State: provider.Assigned,
				Assignment: ho.Assignment}
		}
	}
	return nil
}

func (f *fakeSlots) Release(_ context.Context, h config.Host, container, assignment string) error {
	f.released = append(f.released, h.Name+"/"+container+" "+assignment)
	return nil
}

type harness struct {
	*Broker
	forge *fakeForge
	slots *fakeSlots
	at    time.Time
	logs  *bytes.Buffer
	path  string
}

func (h *harness) advance(seconds float64) {
	h.at = h.at.Add(time.Duration(seconds * float64(time.Second)))
}

func (h *harness) tick(t *testing.T) {
	t.Helper()
	if err := h.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newHarness(t *testing.T, extra ...string) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	hist, err := history.Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hist.Close() })
	h := &harness{forge: &fakeForge{}, slots: &fakeSlots{slots: map[string][]provider.Slot{}},
		at: time.Unix(1_000_000, 0), logs: &bytes.Buffer{}, path: path}
	b, err := New(testConfig(t, extra...), Options{Forge: h.forge, Provider: h.slots, History: hist,
		Log: slog.New(slog.NewTextHandler(h.logs, nil)), Now: func() time.Time { return h.at }})
	if err != nil {
		t.Fatal(err)
	}
	h.Broker = b
	return h
}

// assigned7 is job 7 assigned to ci-1/ci.
func assigned7(t *testing.T, h *harness) {
	t.Helper()
	h.forge.jobs = []Job{job(7)}
	h.slots.slots = map[string][]provider.Slot{"ci-1": ready("ci-1/ci"), "ci-2": {}}
	h.tick(t)
}

func finished(host, container, assignment string, task int64, peak int, seconds float64) provider.Slot {
	return provider.Slot{Host: host, Container: container, State: provider.Resetting, Finished: assignment,
		Task: task, PeakMiB: peak, Seconds: seconds}
}

func TestANewJobGetsItsOwnersRunnerOnAHostAlone(t *testing.T) {
	h := newHarness(t)
	h.forge.jobs = []Job{job(7)}
	h.slots.slots = map[string][]provider.Slot{"ci-1": ready("ci-1/ci"), "ci-2": ready("ci-2/ci-2")}
	h.tick(t)
	if want := [][2]string{{"user:alice", "berth-ci-1-ci-1000000-7"}}; !reflect.DeepEqual(h.forge.registered, want) {
		t.Fatalf("registered %v, want %v", h.forge.registered, want)
	}
	want := provider.Handoff{Assignment: "ci-1-ci-1000000-7", UUID: "uuid-1", Token: "secret-token-1",
		Labels: []string{"ubuntu-latest"}, Class: "trusted", UnusedMinutes: 15}
	if h.slots.where[0] != "ci-1/ci" || !reflect.DeepEqual(h.slots.assigned[0], want) {
		t.Fatalf("assigned %v %#v", h.slots.where, h.slots.assigned)
	}
	state := h.Snapshot()
	if state.Hosts["ci-1"] != (HostState{Reachable: true, ReservedMiB: 6000}) {
		t.Fatalf("state %+v", state.Hosts)
	}
	data, _ := json.Marshal(state)
	if strings.Contains(string(data), "secret-token") || strings.Contains(h.logs.String(), "secret-token") {
		t.Fatal("a runner token is in the state or the log")
	}
	held, _ := h.history.Held()
	if strings.Contains(fmt.Sprint(held), "secret-token") {
		t.Fatal("a runner token is in the history")
	}
}

func TestANewRepositorysJobRunsBesideOthers(t *testing.T) {
	// unknown_mib, not a host alone: on a host larger than that, another job fits beside it.
	h := newHarness(t)
	h.cfg.Hosts = []config.Host{{Name: "ci-1", Address: "a", MemoryMiB: 10000}}
	h.forge.jobs = []Job{job(7), job(8, repo("carol/site"), runsOn("docker"))}
	if err := h.history.Record(history.Run{Repo: "carol/site", Name: "test", Family: "test", At: 999_000,
		PeakMiB: 2000, Host: "ci-1"}); err != nil {
		t.Fatal(err)
	}
	h.slots.slots = map[string][]provider.Slot{"ci-1": ready("ci-1/a", "ci-1/b")}
	h.tick(t)
	var got []string
	for _, a := range h.Assignments() {
		got = append(got, fmt.Sprintf("%s %d %v", a.Container, a.ReserveMiB, a.Exclusive))
	}
	if want := []string{"a 6144 false", "b 2400 false"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("assignments %v, want %v", got, want)
	}
}

func TestAFinishedJobIsLearned(t *testing.T) {
	h := newHarness(t)
	assigned7(t, h)
	h.forge.jobs = nil
	h.forge.tasks = map[int64]int64{555: 7}
	h.slots.slots["ci-1"] = []provider.Slot{finished("ci-1", "ci", "ci-1-ci-1000000-7", 555, 2100, 300)}
	h.tick(t)
	peak, source, found := h.estimate(job(8), h.clock())
	if peak != 2100 || source != "job" || !found {
		t.Fatalf("estimate %d %s %v", peak, source, found)
	}
	if len(h.Assignments()) != 0 || h.Counts()["learned"] != 1 {
		t.Fatalf("assignments %v, counts %v", h.Assignments(), h.Counts())
	}
	// Known now: the next run reserves its peak with the margin, beside others.
	if n, _ := Predict(peak, found, source, h.cfg.Estimate); n != 2520 {
		t.Fatalf("reserve %d, want 2520", n)
	}
}

func TestAJobThatNeededMoreThanReservedIsCounted(t *testing.T) {
	h := newHarness(t)
	if err := h.history.Record(history.Run{Repo: "alice/app", Name: "test", Family: "test", At: 999_000,
		PeakMiB: 1000, Seconds: 60, Host: "ci-1"}); err != nil {
		t.Fatal(err)
	}
	assigned7(t, h)
	if a := h.Assignments(); len(a) != 1 || a[0].ReserveMiB != 1200 {
		t.Fatalf("assignments %+v", a)
	}
	h.forge.jobs = nil
	h.forge.tasks = map[int64]int64{555: 7}
	h.slots.slots["ci-1"] = []provider.Slot{finished("ci-1", "ci", "ci-1-ci-1000000-7", 555, 1900, 60)}
	h.tick(t)
	if h.Counts()["underestimated"] != 1 {
		t.Fatalf("counts %v", h.Counts())
	}
	peak, source, found := h.estimate(job(8), h.clock())
	if n, _ := Predict(peak, found, source, h.cfg.Estimate); n != 2280 {
		t.Fatalf("reserve %d, want 2280", n)
	}
}

func TestSettling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		slot    provider.Slot
		ends    string
		taskErr error
		jobs    []Job
		counts  map[string]int
		deleted []string
		known   int
		log     string
		// left are the assignments after the look.
		left []string
	}{
		{name: "an unused runner is deleted",
			slot:    provider.Slot{Host: "ci-1", Container: "ci", State: provider.Resetting, Finished: "ci-1-ci-1000000-7", Unused: true},
			counts:  map[string]int{"unused": 1},
			deleted: []string{"user:alice/1"}},
		{name: "a job run without a task line is counted unlearned, not unused",
			slot:    provider.Slot{Host: "ci-1", Container: "ci", State: provider.Ready, Finished: "ci-1-ci-1000000-7", PeakMiB: 900},
			counts:  map[string]int{"unlearned": 1, "unused": 0, "learned": 0},
			deleted: []string{"user:alice/1"},
			log:     "no task line"},
		{name: "a cancelled job is not learned",
			slot:   finished("ci-1", "ci", "ci-1-ci-1000000-7", 555, 300, 5),
			ends:   "cancelled",
			counts: map[string]int{"learned": 0}},
		{name: "a run deleted during its job does not stop the look",
			slot:    provider.Slot{Host: "ci-1", Container: "ci", State: provider.Ready, Finished: "ci-1-ci-1000000-7", Task: 555, PeakMiB: 900, Seconds: 10},
			taskErr: errors.New("GET /repos/alice/app/actions/runs/70/jobs: HTTP 404"),
			jobs:    []Job{job(8)},
			counts:  map[string]int{"learned": 0, "unlearned": 1},
			left:    []string{"ci-1-ci-1000000-8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			assigned7(t, h)
			h.forge.jobs = tc.jobs
			h.forge.tasks = map[int64]int64{555: 7}
			h.forge.ends = map[int64]string{555: tc.ends}
			h.forge.taskErr = tc.taskErr
			h.slots.slots["ci-1"] = []provider.Slot{tc.slot}
			h.tick(t)
			for k, v := range tc.counts {
				if h.Counts()[k] != v {
					t.Errorf("count %s = %d, want %d", k, h.Counts()[k], v)
				}
			}
			if !reflect.DeepEqual(h.forge.deleted, tc.deleted) {
				t.Errorf("deleted %v, want %v", h.forge.deleted, tc.deleted)
			}
			if n, _ := h.history.Known(); n != tc.known {
				t.Errorf("known %d, want %d", n, tc.known)
			}
			if !strings.Contains(h.logs.String(), tc.log) {
				t.Errorf("the log lacks %q", tc.log)
			}
			var left []string
			for _, a := range h.Assignments() {
				left = append(left, a.ID)
			}
			if !reflect.DeepEqual(left, tc.left) {
				t.Errorf("assignments %v, want %v", left, tc.left)
			}
		})
	}
}

func TestARefusedAssignmentDeletesTheRunner(t *testing.T) {
	h := newHarness(t)
	h.forge.jobs = []Job{job(7, repo("carol/site"), runsOn("docker"))}
	h.slots.slots = map[string][]provider.Slot{"ci-1": {}, "ci-2": ready("ci-2/ci-2")}
	h.slots.refuse = errors.New("the slot is not ready")
	h.tick(t)
	if want := [][2]string{{"global", "berth-ci-2-ci-2-1000000-7"}}; !reflect.DeepEqual(h.forge.registered, want) {
		t.Fatalf("registered %v", h.forge.registered)
	}
	if !reflect.DeepEqual(h.forge.deleted, []string{"global/1"}) || len(h.Assignments()) != 0 {
		t.Fatalf("deleted %v, assignments %v", h.forge.deleted, h.Assignments())
	}
	if held, _ := h.history.Held(); len(held) != 0 {
		t.Fatalf("held %v", held)
	}
}

func TestAnUnreachableHostGetsNoRunnerAndKeepsItsAssignments(t *testing.T) {
	h := newHarness(t)
	assigned7(t, h)
	h.slots.down = map[string]bool{"ci-1": true}
	h.forge.jobs = []Job{job(8)}
	h.advance(LostAfter * 2)
	h.tick(t)
	if len(h.forge.registered) != 1 || len(h.Assignments()) != 1 || h.Assignments()[0].ID != "ci-1-ci-1000000-7" {
		t.Fatalf("registered %v, assignments %v", h.forge.registered, h.Assignments())
	}
	if !reflect.DeepEqual(h.reachable, map[string]bool{"ci-1": false, "ci-2": true}) {
		t.Fatalf("reachable %v", h.reachable)
	}
}

func TestAnAssignmentOnAHostNoLongerConfiguredIsForgotten(t *testing.T) {
	h := newHarness(t)
	h.assigned["old"] = Assignment{ID: "old", Host: "ci-3", Container: "ci", Scope: "user:alice", RunnerID: 5,
		Labels: []string{"docker"}, ReserveMiB: 1000}
	h.slots.slots = map[string][]provider.Slot{"ci-1": {}, "ci-2": {}}
	h.tick(t)
	if !reflect.DeepEqual(h.forge.deleted, []string{"user:alice/5"}) || len(h.Assignments()) != 0 {
		t.Fatalf("deleted %v, assignments %v", h.forge.deleted, h.Assignments())
	}
	if !strings.Contains(h.logs.String(), "lost its runner") {
		t.Fatal("not logged")
	}
}

func TestShadowModeDoesNotReportTheLiveInstancesLostRunners(t *testing.T) {
	h := newHarness(t, "shadow: true")
	h.assigned["old"] = Assignment{ID: "old", Host: "ci-3", Container: "ci", Scope: "user:alice", RunnerID: 5,
		Labels: []string{"docker"}, ReserveMiB: 1000}
	h.slots.slots = map[string][]provider.Slot{"ci-1": {}, "ci-2": {}}
	h.tick(t)
	if len(h.forge.deleted) != 0 || strings.Contains(h.logs.String(), "lost its runner") {
		t.Fatalf("deleted %v, logs %s", h.forge.deleted, h.logs.String())
	}
}

func TestARunnerNoSlotHoldsAnyMoreIsDeleted(t *testing.T) {
	h := newHarness(t)
	assigned7(t, h)
	// The cycle restarted: the slot is ready again without reporting the assignment.
	h.forge.jobs = nil
	h.slots.slots["ci-1"] = ready("ci-1/ci")
	h.advance(LostAfter)
	h.tick(t)
	if !reflect.DeepEqual(h.forge.deleted, []string{"user:alice/1"}) || len(h.Assignments()) != 0 {
		t.Fatalf("deleted %v, assignments %v", h.forge.deleted, h.Assignments())
	}
}

func TestASlotSlowToTakeItsAssignmentKeepsIt(t *testing.T) {
	h := newHarness(t)
	h.forge.jobs = []Job{job(7), job(8, named("other"))}
	h.slots.slots = map[string][]provider.Slot{"ci-1": ready("ci-1/ci"), "ci-2": {}}
	h.tick(t)
	h.slots.slots["ci-1"] = ready("ci-1/ci")
	h.advance(10)
	h.tick(t)
	if len(h.forge.deleted) != 0 || len(h.slots.assigned) != 1 {
		t.Fatalf("deleted %v, assigned %v", h.forge.deleted, h.slots.where)
	}
}

func TestARestartedBrokerKeepsItsAssignments(t *testing.T) {
	h := newHarness(t)
	assigned7(t, h)
	again, err := New(h.cfg, Options{Forge: h.forge, Provider: h.slots, History: h.history, Log: h.log, Now: h.now})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Assignments(), h.Assignments()) {
		t.Fatalf("kept %v, want %v", again.Assignments(), h.Assignments())
	}
	h.forge.listed = []Runner{{Scope: "user:alice", ID: 1, Name: "berth-" + h.Assignments()[0].ID}}
	if err := again.Sweep(context.Background()); err != nil || len(h.forge.deleted) != 0 {
		t.Fatalf("sweep deleted %v, %v", h.forge.deleted, err)
	}
}

func TestTheSweepDeletesOnlyTheBrokersLeftovers(t *testing.T) {
	h := newHarness(t)
	h.forge.listed = []Runner{{Scope: "global", ID: 3, Name: "berth-ci-1-ci-1-1"}, {Scope: "global", ID: 4, Name: "static-runner"}}
	if err := h.Sweep(context.Background()); err != nil || !reflect.DeepEqual(h.forge.deleted, []string{"global/3"}) {
		t.Fatalf("deleted %v, %v", h.forge.deleted, err)
	}
}

func TestMetrics(t *testing.T) {
	h := newHarness(t)
	h.forge.jobs = []Job{job(7, repo("carol/site"))}
	h.slots.slots = map[string][]provider.Slot{"ci-1": {}, "ci-2": {}}
	h.tick(t)
	text := h.Snapshot().Metrics()
	for _, line := range []string{`forgejo_berth_waiting_jobs{kind="unservable"} 1`,
		`forgejo_berth_host_reachable{host="ci-2"} 1`, `forgejo_berth_estimates{source="none"} 1`,
		`forgejo_berth_events_total{event="assigned"} 0`, "forgejo_berth_shadow 0"} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("metrics lack %s:\n%s", line, text)
		}
	}
	if strings.Contains(text, "forgejo_berth_cycle_") {
		t.Errorf("cycle metrics without cycles:\n%s", text)
	}

	// The cycles the provider runs: the metrics a cycle on its host writes, with the host.
	h.slots.cycles = []provider.CycleStats{
		{Host: "ci-1", Container: "a", State: provider.Running, Jobs: 3, LastResetSeconds: 4.25},
		{Host: "ci-2", Container: "a", State: provider.Failed, Failures: 2},
	}
	h.tick(t)
	text = h.Snapshot().Metrics()
	for _, line := range []string{`forgejo_berth_cycle_state{host="ci-1",container="a",state="running"} 1`,
		`forgejo_berth_cycle_state{host="ci-1",container="a",state="ready"} 0`,
		`forgejo_berth_cycle_state{host="ci-2",container="a",state="failed"} 1`,
		`forgejo_berth_cycle_jobs_total{host="ci-1",container="a"} 3`,
		`forgejo_berth_cycle_failures{host="ci-2",container="a"} 2`,
		`forgejo_berth_cycle_last_reset_seconds{host="ci-1",container="a"} 4.2`} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("metrics lack %s:\n%s", line, text)
		}
	}
	if n := strings.Count(text, "# TYPE forgejo_berth_cycle_state gauge"); n != 1 {
		t.Errorf("the cycle state's type %d times", n)
	}
	if data, _ := json.Marshal(h.Snapshot()); !strings.Contains(string(data), `"cycles":[{"host":"ci-1","container":"a"`) {
		t.Errorf("the state lacks the cycles: %s", data)
	}
}

func TestRelease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		want  []string
	}{
		// Its run deleted or cancelled: released after a minute and two looks, once.
		{"a runner whose jobs went is released", provider.Assigned, []string{"ci-1/ci ci-1-ci-1000000-7"}},
		// A slot running its job keeps it, whether or not the job still waits.
		{"a busy runner is not released", provider.Running, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			assigned7(t, h)
			h.forge.jobs = nil
			h.slots.slots["ci-1"] = []provider.Slot{{Host: "ci-1", Container: "ci", State: tc.state,
				Assignment: "ci-1-ci-1000000-7"}}
			for range 4 {
				h.advance(40)
				h.tick(t)
			}
			if !reflect.DeepEqual(h.slots.released, tc.want) || h.Counts()["released"] != len(tc.want) {
				t.Fatalf("released %v (%d), want %v", h.slots.released, h.Counts()["released"], tc.want)
			}
		})
	}
}

func TestShadowModeActsOnNothing(t *testing.T) {
	h := newHarness(t, "shadow: true")
	h.forge.jobs = []Job{job(7)}
	h.slots.slots = map[string][]provider.Slot{"ci-1": ready("ci-1/ci"), "ci-2": {}}
	h.tick(t)
	if len(h.forge.registered) != 0 || len(h.slots.assigned) != 0 || h.Counts()["would_assign"] != 1 {
		t.Fatalf("registered %v, assigned %v, counts %v", h.forge.registered, h.slots.where, h.Counts())
	}
	if !strings.Contains(h.logs.String(), "would assign") {
		t.Fatal("the decision is not logged")
	}
	h.forge.listed = []Runner{{Scope: "global", ID: 3, Name: "berth-x"}}
	if err := h.Sweep(context.Background()); err != nil || len(h.forge.deleted) != 0 {
		t.Fatalf("the shadow swept %v", h.forge.deleted)
	}
}

func TestShadowModeSeesTheLiveInstancesAssignments(t *testing.T) {
	live := newHarness(t)
	assigned7(t, live)
	ro, err := history.Open(live.path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	shadowForge := &fakeForge{jobs: []Job{job(7), job(8)}}
	shadow, err := New(live.cfg, Options{Forge: shadowForge, Provider: live.slots, History: ro,
		Log: slog.New(slog.DiscardHandler), Now: live.now})
	if err != nil {
		t.Fatal(err)
	}
	if !shadow.Shadow() {
		t.Fatal("a read-only history is not shadow mode")
	}
	live.slots.slots["ci-2"] = ready("ci-2/ci-2")
	if err := shadow.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Job 7 is claimed by the live runner; job 8 would go to ci-2.
	if shadow.Counts()["would_assign"] != 1 || len(shadow.Assignments()) != 1 {
		t.Fatalf("counts %v, assignments %v", shadow.Counts(), shadow.Assignments())
	}
}
