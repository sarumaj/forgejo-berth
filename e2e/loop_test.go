// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/broker"
	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/forgejo"
	"git.ciepiela.org/dawid/forgejo-berth/internal/history"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// world is a broker over a fake Forgejo and in-memory slots, on a clock the test moves.
type world struct {
	t    *testing.T
	b    *broker.Broker
	f    *forge
	sl   *slots
	hist *history.History
	at   *time.Time
	logs *bytes.Buffer
	cfg  *config.Config
	ctx  context.Context
}

// newWorld has one host ci-1 of memoryMiB with the containers named, alice trusted.
func newWorld(t *testing.T, memoryMiB int, containers []string, extra string) *world {
	t.Helper()
	dir := t.TempDir()
	f := &forge{t: t, repos: map[int64][2]string{1: {"alice", "alice/app"}, 2: {"carol", "carol/site"}},
		runners: map[int64]*fakeRunner{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := broker.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
forgejo: {url: %s, admin_token_file: %s}
trusted: {owners: {alice: %s}, labels: [ubuntu-latest, docker]}
shared: {labels: [docker]}
hosts: [{name: ci-1, address: ci-1.example.org, memory_mib: %d}]
%s
`, server.URL, write("admin.token", adminToken), write("alice.token", aliceToken), memoryMiB, extra)))
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.Open(filepath.Join(dir, "history.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hist.Close() })
	sl := &slots{slots: map[string]*slotState{}}
	for _, c := range containers {
		sl.slots["ci-1/"+c] = &slotState{Slot: provider.Slot{Host: "ci-1", Container: c, State: provider.Ready}}
	}
	at := time.Unix(1_700_000_000, 0)
	w := &world{t: t, f: f, sl: sl, hist: hist, at: &at, logs: &bytes.Buffer{}, cfg: cfg, ctx: context.Background()}
	w.b = w.broker()
	return w
}

func (w *world) broker() *broker.Broker {
	b, err := broker.New(w.cfg, broker.Options{
		Forge:    forgejo.New(w.cfg.Forgejo.URL, adminToken, map[string]string{"alice": aliceToken}, nil),
		Provider: w.sl, History: w.hist, Log: slog.New(slog.NewTextHandler(w.logs, nil)),
		Now: func() time.Time { return *w.at },
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return b
}

func (w *world) tick() {
	w.t.Helper()
	if err := w.b.Tick(w.ctx); err != nil {
		w.t.Fatal(err)
	}
	*w.at = w.at.Add(10 * time.Second)
}

func (w *world) add(jobs ...*fakeJob) {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	for _, j := range jobs {
		j.Status = "waiting"
		w.f.jobs = append(w.f.jobs, j)
	}
}

func (w *world) status(id int64, status string) {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	for _, j := range w.f.jobs {
		if j.ID == id {
			j.Status = status
		}
	}
}

func (w *world) reserves() []string {
	var out []string
	for _, a := range w.b.Assignments() {
		out = append(out, fmt.Sprintf("%s %d", a.Container, a.ReserveMiB))
	}
	slices.Sort(out)
	return out
}

func job(id int64, repo int64, name string, labels ...string) *fakeJob {
	return &fakeJob{ID: id, RunID: id * 10, RepoID: repo, Name: name, RunsOn: labels}
}

func TestLeftoverRunnersAreSweptAtTheStart(t *testing.T) {
	w := newWorld(t, 8000, []string{"a"}, "")
	w.f.runners[90] = &fakeRunner{ID: 90, Name: "berth-ci-1-a-1-1", Scope: broker.GlobalScope}
	w.f.runners[91] = &fakeRunner{ID: 91, Name: "berth-ci-1-a-1-2", Scope: "user:alice"}
	w.f.runners[92] = &fakeRunner{ID: 92, Name: "static", Scope: broker.GlobalScope}
	w.f.nextID = 100
	if err := w.b.Sweep(w.ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.f.runners) != 1 || w.f.runners[92] == nil {
		t.Fatalf("runners left %v", w.f.runners)
	}
}

func TestARestartKeepsItsRunnersFromTheSweep(t *testing.T) {
	w := newWorld(t, 8000, []string{"a"}, "")
	w.add(job(1, 1, "build", "ubuntu-latest"))
	w.tick()
	w.b = w.broker() // a restart on the same history
	if err := w.b.Sweep(w.ctx); err != nil || len(w.f.runners) != 1 || len(w.b.Assignments()) != 1 {
		t.Fatalf("runners %v, assignments %v, %v", w.f.runners, w.b.Assignments(), err)
	}
}

func TestACancelledWaitingJobsRunnerIsReleasedAndDeleted(t *testing.T) {
	w := newWorld(t, 8000, []string{"a"}, "")
	w.add(job(1, 2, "test", "docker"))
	w.tick()
	if len(w.f.runners) != 1 {
		t.Fatalf("runners %v", w.f.runners)
	}
	w.status(1, "cancelled")
	for range 8 { // a minute and two looks, then the slot stops it
		w.tick()
		w.sl.step(w.f, nil)
	}
	w.tick()
	if len(w.sl.released) != 1 || len(w.f.runners) != 0 || len(w.b.Assignments()) != 0 {
		t.Fatalf("released %v, runners %v, assignments %v", w.sl.released, w.f.runners, w.b.Assignments())
	}
	if c := w.b.Counts(); c["released"] != 1 || c["unused"] != 1 || c["learned"] != 0 {
		t.Fatalf("counts %v", c)
	}
}

func TestACancelledRunningJobIsNotLearned(t *testing.T) {
	w := newWorld(t, 8000, []string{"a"}, "")
	w.add(job(1, 2, "test", "docker"))
	w.tick()
	w.sl.step(w.f, map[string]int{"test": 900}) // takes it
	w.tick()
	w.status(1, "cancelled")
	w.sl.step(w.f, nil) // ends
	w.tick()
	if n, _ := w.hist.Known(); n != 0 || w.b.Counts()["learned"] != 0 || len(w.b.Assignments()) != 0 {
		t.Fatalf("known %d, counts %v", n, w.b.Counts())
	}
	if !strings.Contains(w.logs.String(), "ended=cancelled") {
		t.Fatalf("log:\n%s", w.logs.String())
	}
}

func TestClaimsAndReservations(t *testing.T) {
	// One host of 6000 MiB, three slots. alice/app's build needed 2000 MiB, its e2e 3000.
	w := newWorld(t, 6000, []string{"a", "b", "c"}, "")
	for _, r := range []history.Run{
		{Repo: "alice/app", Name: "build", Family: "build", At: 1_699_999_000, PeakMiB: 2000},
		{Repo: "alice/app", Name: "e2e", Family: "e2e", At: 1_699_999_000, PeakMiB: 3000},
	} {
		if err := w.hist.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	// The same kind (scope and labels): Forgejo hands the first runner job 1, the next job 2.
	w.add(job(1, 1, "build", "ubuntu-latest"), job(2, 1, "e2e", "ubuntu-latest"))
	w.tick()
	// 2400 for the build; the e2e's runner reserves 3600, as Forgejo hands it the e2e.
	if got := w.reserves(); !slices.Equal(got, []string{"a 2400", "b 3600"}) {
		t.Fatalf("reserves %v", got)
	}
	// Another build: the host has no room left; it waits, and its runners' claims hold.
	w.add(job(3, 1, "build", "ubuntu-latest"))
	w.tick()
	if got := w.reserves(); len(got) != 2 {
		t.Fatalf("reserves %v", got)
	}
	// The runners take their jobs (the oldest first), run and end; the third build follows.
	w.sl.step(w.f, map[string]int{"build": 2100, "e2e": 2900})
	w.tick()
	w.sl.step(w.f, nil)
	w.tick()
	if got := w.reserves(); !slices.Equal(got, []string{"a 2520"}) {
		t.Fatalf("after the first two: %v", got)
	}
	w.f.mu.Lock()
	tasks := map[int64]int64{}
	for _, j := range w.f.jobs {
		tasks[j.ID] = j.Task
	}
	w.f.mu.Unlock()
	if tasks[1] == 0 || tasks[2] == 0 || tasks[1] > tasks[2] {
		t.Fatalf("tasks %v", tasks)
	}
}

func TestOldRunsArePruned(t *testing.T) {
	w := newWorld(t, 8000, []string{"a"}, "estimate: {retention_days: 30}")
	if err := w.hist.Record(history.Run{Repo: "alice/app", Name: "build", Family: "build",
		At: float64(w.at.Unix()) - 40*86400, PeakMiB: 100}); err != nil {
		t.Fatal(err)
	}
	w.add(job(1, 1, "build", "ubuntu-latest"))
	w.tick()
	// Forgotten before the estimate: the job is unknown again, 6144 MiB.
	if got := w.reserves(); !slices.Equal(got, []string{"a 6144"}) {
		t.Fatalf("reserves %v", got)
	}
	if n, _ := w.hist.Known(); n != 0 {
		t.Fatalf("known %d", n)
	}
}

func TestShadowBesideALiveInstance(t *testing.T) {
	w := newWorld(t, 16000, []string{"a", "b"}, "")
	w.add(job(1, 1, "build", "ubuntu-latest"))
	w.tick()
	ro, err := history.Open(filepath.Join(filepath.Dir(w.hist.Path()), "history.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var logs bytes.Buffer
	shadow, err := broker.New(w.cfg, broker.Options{
		Forge:    forgejo.New(w.cfg.Forgejo.URL, adminToken, map[string]string{"alice": aliceToken}, nil),
		Provider: w.sl, History: ro, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Now: func() time.Time { return *w.at },
	})
	if err != nil {
		t.Fatal(err)
	}
	w.add(job(2, 2, "test", "docker"))
	runners := len(w.f.runners)
	if err := shadow.Tick(w.ctx); err != nil {
		t.Fatal(err)
	}
	// Job 1 is the live runner's; the shadow would start one for job 2 only, and starts none.
	if c := shadow.Counts(); c["would_assign"] != 1 || len(w.f.runners) != runners {
		t.Fatalf("counts %v, runners %d", c, len(w.f.runners))
	}
	if !strings.Contains(logs.String(), "would assign") || !strings.Contains(logs.String(), "repo=carol/site") {
		t.Fatalf("log:\n%s", logs.String())
	}
}
