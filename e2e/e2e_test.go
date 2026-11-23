// SPDX-License-Identifier: GPL-3.0-or-later

// Package e2e runs the broker against a fake Forgejo API (httptest) and an in-memory slot
// provider that plays the slots' life: a runner takes the oldest waiting job it covers, as
// Forgejo hands it, runs it, and the slot reports the job's task and peak.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/broker"
	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/forgejo"
	"git.ciepiela.org/dawid/forgejo-berth/internal/history"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

const (
	adminToken = "admin-secret-0000000000"
	aliceToken = "alice-secret-0000000000"
)

type fakeJob struct {
	ID, RunID, RepoID int64
	Name              string
	RunsOn            []string
	Status            string // waiting, running, success
	Task              int64
}

type fakeRunner struct {
	ID          int64
	Name, Scope string
	UUID, Token string
}

// forge is a fake Forgejo: its jobs, its repositories and the runners registered.
type forge struct {
	mu       sync.Mutex
	jobs     []*fakeJob
	repos    map[int64][2]string // id -> owner, full name
	runners  map[int64]*fakeRunner
	nextID   int64
	nextTask int64
	t        *testing.T
}

func (f *forge) token(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
}

func (f *forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	token := f.token(r)
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	scope := ""
	switch {
	case strings.HasPrefix(path, "/admin/actions/runners") && token == adminToken:
		scope = broker.GlobalScope
	case strings.HasPrefix(path, "/user/actions/runners") && token == aliceToken:
		scope = "user:alice"
	}
	switch {
	case r.Method == http.MethodGet && path == "/admin/runners/jobs" && token == adminToken:
		var list []map[string]any
		if page <= 1 {
			for _, j := range f.jobs {
				list = append(list, map[string]any{"id": j.ID, "run_id": j.RunID, "repo_id": j.RepoID,
					"name": j.Name, "runs_on": j.RunsOn, "status": j.Status})
			}
		}
		reply(map[string]any{"jobs": list})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repositories/") && token == adminToken:
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/repositories/"), 10, 64)
		repo := f.repos[id]
		reply(map[string]any{"full_name": repo[1], "owner": map[string]any{"login": repo[0]}})
	case r.Method == http.MethodPost && scope != "":
		var body struct {
			Name      string `json:"name"`
			Ephemeral bool   `json:"ephemeral"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Ephemeral {
			http.Error(w, "not ephemeral", http.StatusBadRequest)
			return
		}
		f.nextID++
		run := &fakeRunner{ID: f.nextID, Name: body.Name, Scope: scope,
			UUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.nextID), Token: fmt.Sprintf("%040d", f.nextID)}
		f.runners[run.ID] = run
		reply(map[string]any{"id": run.ID, "uuid": run.UUID, "token": run.Token})
	case r.Method == http.MethodDelete && scope != "":
		id, _ := strconv.ParseInt(path[strings.LastIndex(path, "/")+1:], 10, 64)
		if run, ok := f.runners[id]; !ok || run.Scope != scope {
			http.NotFound(w, r)
			return
		}
		delete(f.runners, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && scope != "":
		var list []map[string]any
		if page <= 1 {
			for _, run := range f.runners {
				if run.Scope == scope {
					list = append(list, map[string]any{"id": run.ID, "name": run.Name})
				}
			}
		}
		reply(map[string]any{"runners": list})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/jobs") && token == adminToken:
		parts := strings.Split(path, "/")
		run, _ := strconv.ParseInt(parts[len(parts)-2], 10, 64)
		var list []map[string]any
		for _, j := range f.jobs {
			if j.RunID == run && page <= 1 {
				conclusion := ""
				if j.Status == "success" || j.Status == "failure" || j.Status == "cancelled" {
					conclusion = j.Status
				}
				list = append(list, map[string]any{"id": j.ID, "task_id": j.Task, "status": j.Status,
					"conclusion": conclusion})
			}
		}
		reply(map[string]any{"jobs": list})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
	}
}

// take hands the runner the oldest waiting job of its scope that its labels cover, as
// Forgejo does; it returns the job, or nil.
func (f *forge) take(runnerUUID string, labels []string) *fakeJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	var run *fakeRunner
	for _, r := range f.runners {
		if r.UUID == runnerUUID {
			run = r
		}
	}
	if run == nil {
		return nil
	}
	for _, j := range f.jobs {
		owner := f.repos[j.RepoID][0]
		ofScope := (run.Scope == "user:"+owner) || (run.Scope == broker.GlobalScope && owner != "alice")
		covered := true
		for _, l := range j.RunsOn {
			covered = covered && slices.Contains(labels, l)
		}
		if j.Status == "waiting" && ofScope && covered {
			f.nextTask++
			j.Status, j.Task = "running", 100+f.nextTask
			// Ephemeral: Forgejo removes the runner once its job is done; here at once.
			delete(f.runners, run.ID)
			return j
		}
	}
	return nil
}

// finish ends a running job with success; one cancelled meanwhile stays cancelled.
func (f *forge) finish(j *fakeJob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j.Status == "running" {
		j.Status = "success"
	}
}

// slots is an in-memory provider whose slots go ready, assigned, running, ready.
type slots struct {
	mu       sync.Mutex
	slots    map[string]*slotState
	released []string
}

type slotState struct {
	provider.Slot
	uuid   string
	labels []string
	job    *fakeJob
	peak   int
	// releasedFor is the assignment the broker released.
	releasedFor string
}

func (s *slots) Status(_ context.Context, h config.Host) ([]provider.Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []provider.Slot
	for _, st := range s.slots {
		if st.Host == h.Name {
			out = append(out, st.Slot)
		}
	}
	return out, nil
}

func (s *slots) Assign(_ context.Context, h config.Host, container string, ho provider.Handoff) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slots[h.Name+"/"+container]
	if st == nil || st.State != provider.Ready {
		return fmt.Errorf("%s is not ready", container)
	}
	if !provider.RunnerToken.MatchString(ho.Token) || ho.Class == "" {
		return fmt.Errorf("the assignment is refused")
	}
	st.State, st.Assignment, st.uuid, st.labels = provider.Assigned, ho.Assignment, ho.UUID, ho.Labels
	return nil
}

// Release marks the slot's runner released: at the next step, one without a job stops.
func (s *slots) Release(_ context.Context, h config.Host, container, assignment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slots[h.Name+"/"+container]
	if st == nil || st.Assignment != assignment {
		return fmt.Errorf("%s does not hold %s", container, assignment)
	}
	st.releasedFor = assignment
	s.released = append(s.released, assignment)
	return nil
}

// step lets every assigned runner take its job, and ends the running ones with their peak.
func (s *slots) step(f *forge, peaks map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.slots {
		switch st.State {
		case provider.Assigned:
			if j := f.take(st.uuid, st.labels); j != nil {
				st.State, st.job, st.peak = provider.Running, j, peaks[j.Name]
			} else if st.releasedFor == st.Assignment {
				// Stopped without a job: unused.
				st.Slot = provider.Slot{Host: st.Host, Container: st.Container, State: provider.Ready,
					Finished: st.Assignment, Unused: true}
			}
		case provider.Running:
			f.finish(st.job)
			st.Slot = provider.Slot{Host: st.Host, Container: st.Container, State: provider.Ready,
				Finished: st.Assignment, Task: st.job.Task, PeakMiB: st.peak, Seconds: 60}
		}
	}
}

func TestTheBrokerLearnsAndPacks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := broker.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	f := &forge{t: t, repos: map[int64][2]string{1: {"alice", "alice/app"}, 2: {"carol", "carol/site"}},
		runners: map[int64]*fakeRunner{}}
	f.jobs = []*fakeJob{
		{ID: 1, RunID: 10, RepoID: 1, Name: "build", RunsOn: []string{"ubuntu-latest"}, Status: "waiting"},
		{ID: 2, RunID: 20, RepoID: 2, Name: "test", RunsOn: []string{"docker"}, Status: "waiting"},
	}
	server := httptest.NewServer(f)
	defer server.Close()

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
forgejo: {url: %s, admin_token_file: %s}
trusted: {owners: {alice: %s}, labels: [ubuntu-latest, docker]}
shared: {labels: [docker]}
hosts: [{name: ci-1, address: ci-1.example.org, memory_mib: 8000}]
`, server.URL, write("admin.token", adminToken+"\n"), write("alice.token", aliceToken))))
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.Open(filepath.Join(dir, "history.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	sl := &slots{slots: map[string]*slotState{}}
	for _, c := range []string{"a", "b"} {
		sl.slots["ci-1/"+c] = &slotState{Slot: provider.Slot{Host: "ci-1", Container: c, State: provider.Ready}}
	}
	var logs bytes.Buffer
	at := time.Unix(1_700_000_000, 0)
	b, err := broker.New(cfg, broker.Options{
		Forge:    forgejo.New(cfg.Forgejo.URL, adminToken, map[string]string{"alice": aliceToken}, nil),
		Provider: sl, History: hist, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tick := func() {
		t.Helper()
		if err := b.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		at = at.Add(10 * time.Second)
	}
	reserves := func() []string {
		var out []string
		for _, a := range b.Assignments() {
			out = append(out, fmt.Sprintf("%s %s %d", a.Scope, a.Container, a.ReserveMiB))
		}
		slices.Sort(out)
		return out
	}
	peaks := map[string]int{"build": 1500, "test": 1000, "lint": 300}

	// Two new repositories: 6144 MiB each, so only one fits on the 8000 MiB host.
	tick()
	if got := reserves(); !slices.Equal(got, []string{"user:alice a 6144"}) {
		t.Fatalf("first look: %v", got)
	}
	sl.step(f, peaks) // the runner takes job 1
	tick()
	if got := reserves(); len(got) != 1 {
		t.Fatalf("while job 1 runs: %v", got)
	}
	sl.step(f, peaks) // job 1 ends, 1500 MiB
	tick()            // learned; job 2 gets the host
	if c := b.Counts(); c["learned"] != 1 || c["assigned"] != 2 {
		t.Fatalf("counts %v", c)
	}
	if got := reserves(); !slices.Equal(got, []string{"global a 6144"}) {
		t.Fatalf("after job 1: %v", got)
	}

	// Job 1's repository again: its own history (1500 * 1.2) and a relative's (1500 * 1.5)
	// fit beside job 2 now.
	f.mu.Lock()
	f.jobs = append(f.jobs,
		&fakeJob{ID: 3, RunID: 30, RepoID: 1, Name: "build", RunsOn: []string{"ubuntu-latest"}, Status: "waiting"},
		&fakeJob{ID: 4, RunID: 40, RepoID: 1, Name: "lint", RunsOn: []string{"docker"}, Status: "waiting"})
	f.mu.Unlock()
	sl.step(f, peaks) // job 2 starts
	sl.step(f, peaks) // job 2 ends, 1000 MiB
	tick()
	if got := reserves(); !slices.Equal(got, []string{"user:alice a 1800", "user:alice b 2250"}) {
		t.Fatalf("packed: %v", got)
	}
	for range 3 {
		sl.step(f, peaks)
		tick()
	}
	if c := b.Counts(); c["learned"] != 4 || c["underestimated"] != 0 || len(b.Assignments()) != 0 {
		t.Fatalf("counts %v, assignments %v", c, b.Assignments())
	}
	if n, _ := hist.Known(); n != 3 {
		t.Fatalf("jobs learned %d, want 3", n)
	}
	if err := b.Sweep(ctx); err != nil || len(f.runners) != 0 {
		t.Fatalf("runners left %v, %v", f.runners, err)
	}
	for _, secret := range []string{adminToken, aliceToken, "0000000000000000000000000000000000000001"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log holds a secret:\n%s", logs.String())
		}
	}
	state, _ := json.Marshal(b.Snapshot())
	if strings.Contains(string(state), "token") {
		t.Fatalf("the state holds a token: %s", state)
	}
}
