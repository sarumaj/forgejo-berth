// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// A cycle that stopped while its runner waited for its job takes that runner up again,
// without resetting the container under it, and finishes its job.
func TestResumeWatchesARunnerStillActive(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: []look{{true, false}, {true, false}, {true, true}, {false, false}},
		task: 42, peak: 900}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	stopped := false
	c.stopping = func() bool { return stopped }
	sys.looks = []look{{true, false}}
	_ = c.system.Apply(a)
	c.setState(provider.Assigned)
	c.servedSince, c.unusedMinutes, c.assignment = sys.clock.now, 15, a.ID
	stopped = true
	c.watch(a, sys.clock.now, time.Time{})
	c.setState(provider.Stopped)

	sys.calls = nil
	sys.looks = []look{{true, false}, {true, false}, {true, true}, {false, false}}
	again := newCycle(t, dir, sys, sys.clock.stop(100))
	if !again.Resume() {
		t.Fatal("the resumed runner failed")
	}
	if want := []string{"journal", "measure"}; !slices.Equal(sys.calls, want) {
		t.Fatalf("calls %v, want %v", sys.calls, want)
	}
	if last := again.Last(); last.Assignment != a.ID || last.Task != 42 || last.PeakMiB != 900 || last.Unused {
		t.Fatalf("last %+v", last)
	}
}

// A runner that ended while no cycle watched it is finished from the state left, and the
// last finished assignment the broker may not have read yet is kept.
func TestResumeFinishesARunnerThatEnded(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), task: 7, peak: 300}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	write(t, filepath.Join(dir, "run/ci.json"), `{"state": "running", "assignment": "a-2",
		"served_since": 999000, "job_since": 999500, "unused_minutes": 15, "jobs": 3,
		"cpu_at_job": [0, 0], "cpu_ok": true, "last": {"assignment": "a-1", "task": 5}}`)
	if !c.Resume() {
		t.Fatal("resume failed")
	}
	last := c.Last()
	if last.Assignment != "a-2" || last.Task != 7 || last.PeakMiB != 300 || last.Seconds != 500 {
		t.Fatalf("last %+v", last)
	}
	if st := readCycleState(t, dir); st["assignment"] != "" || st["jobs"] != 4.0 {
		t.Fatalf("state %v", st)
	}

	write(t, filepath.Join(dir, "run/ci.json"), `{"state": "stopped", "assignment": "",
		"last": {"assignment": "a-1", "task": 5, "peak_mib": 10}}`)
	c = newCycle(t, dir, sys, sys.clock.stop(100))
	sys.calls = nil
	c.Resume()
	if last := c.Last(); last.Assignment != "a-1" || last.Task != 5 || len(sys.calls) != 0 {
		t.Fatalf("last %+v, calls %v", last, sys.calls)
	}
}

func TestLocalServesTheSlotCommandInProcess(t *testing.T) {
	dir := t.TempDir()
	s := &Settings{Container: "ci", StateDir: filepath.Join(dir, "state"), ConfigTemplate: "/dev/null",
		LabelTables: map[string]map[string]string{"trusted": {"ubuntu-latest": "ubuntu-latest:host"}}}
	s.defaults()
	s.Interval = 0.01
	l, err := NewLocal(map[string]*Settings{"ci": s})
	if err != nil {
		t.Fatal(err)
	}
	sys := &syncSystem{}
	l.Start(func(*Settings) System { return sys }, slog.New(slog.DiscardHandler))
	defer l.Stop()
	h := Hosted{"ci-1": l}
	host := config.Host{Name: "ci-1"}
	wait := func(state string) []provider.Slot {
		t.Helper()
		for range 500 {
			slots, err := h.Status(context.Background(), host)
			if err == nil && len(slots) == 1 && (slots[0].State == state || state == "finished" && slots[0].Finished != "") {
				return slots
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("never %s", state)
		return nil
	}
	wait(provider.Ready)
	if cycles := h.Cycles(); len(cycles) != 1 || cycles[0].Host != "ci-1" || cycles[0].Container != "ci" ||
		cycles[0].State != provider.Ready {
		t.Fatalf("cycles %+v", cycles)
	}
	handoff := provider.Handoff{Assignment: "ci-1-ci-1-7", UUID: testUUID, Token: testToken,
		Labels: []string{"ubuntu-latest"}, Class: "trusted", UnusedMinutes: 15}
	if err := h.Assign(context.Background(), host, "ci", handoff); err != nil {
		t.Fatal(err)
	}
	if err := h.Assign(context.Background(), host, "ci", handoff); err == nil {
		t.Fatal("a second assignment was taken")
	}
	wait(provider.Assigned)
	if err := h.Release(context.Background(), host, "ci", "ci-1-ci-1-7"); err != nil {
		t.Fatal(err)
	}
	// Released before a minute is up, it counts as a runner that failed to start: the
	// slot backs off.
	slots := wait("finished")
	if slots[0].Finished != "ci-1-ci-1-7" || !slots[0].Unused {
		t.Fatalf("slots %+v", slots)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "state/ci.json"))
	if strings.Contains(string(data), testToken) {
		t.Fatal("the token is in the state")
	}
	for i := 0; h.Cycles()[0].Failures != 1; i++ {
		if i == 500 {
			t.Fatalf("a runner that failed to start is not a failure: %+v", h.Cycles())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if (&Local{slots: l.slots}).Cycles("ci-1") != nil {
		t.Fatal("cycles not started are reported")
	}
	if _, err := h.Status(context.Background(), config.Host{Name: "ci-2"}); err == nil {
		t.Fatal("an unknown host answered")
	}
	l.Ping = func(context.Context) error { return errors.New("down") }
	if _, err := h.Status(context.Background(), host); err == nil {
		t.Fatal("a host whose API is down answered")
	}
}

// syncSystem is a container whose runner waits until it is stopped.
type syncSystem struct {
	mu      sync.Mutex
	stopped bool
}

func (s *syncSystem) lock() func() {
	s.mu.Lock()
	return s.mu.Unlock
}

func (s *syncSystem) Reset() error {
	defer s.lock()()
	s.stopped = false
	return nil
}
func (s *syncSystem) Apply(provider.Parsed) error { return nil }
func (s *syncSystem) StartRunner() error          { return nil }
func (s *syncSystem) StopRunner() {
	defer s.lock()()
	s.stopped = true
}
func (s *syncSystem) RunnerActive() bool {
	defer s.lock()()
	return !s.stopped
}
func (s *syncSystem) Busy() bool                { return false }
func (s *syncSystem) TookTask() bool            { return false }
func (s *syncSystem) SaveJournal(bool)          {}
func (s *syncSystem) Measure() (int64, int)     { return 0, 0 }
func (s *syncSystem) CPU() (int64, int64, bool) { return 0, 0, false }

// A slot of the broker's own paused through the provider: its cycle holds it, the broker's
// status shows it, and resumed it is ready again.
func TestALocalSlotPausesAndResumes(t *testing.T) {
	dir := t.TempDir()
	s := &Settings{Container: "ci", StateDir: filepath.Join(dir, "state"), ConfigTemplate: "/dev/null",
		LabelTables: map[string]map[string]string{"trusted": {"ubuntu-latest": "ubuntu-latest:host"}}}
	s.defaults()
	s.Interval = 0.01
	l, err := NewLocal(map[string]*Settings{"ci": s})
	if err != nil {
		t.Fatal(err)
	}
	l.Start(func(*Settings) System { return &syncSystem{} }, slog.New(slog.DiscardHandler))
	defer l.Stop()
	h := Hosted{"ci-1": l}
	host := config.Host{Name: "ci-1"}
	wait := func(state string, paused bool) {
		t.Helper()
		for range 500 {
			slots, err := h.Status(context.Background(), host)
			if err == nil && len(slots) == 1 && slots[0].State == state && slots[0].Paused == paused {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("never %s (paused %v)", state, paused)
	}
	wait(provider.Ready, false)
	if _, err := h.Pause(context.Background(), host, "ci", false); err != nil {
		t.Fatal(err)
	}
	wait(provider.Paused, true)
	if cycles := h.Cycles(); cycles[0].State != provider.Paused {
		t.Fatalf("cycles %+v", cycles)
	}
	handoff := provider.Handoff{Assignment: "ci-1-ci-1-7", UUID: testUUID, Token: testToken,
		Labels: []string{"ubuntu-latest"}, Class: "trusted", UnusedMinutes: 15}
	if err := h.Assign(context.Background(), host, "ci", handoff); err == nil {
		t.Fatal("a paused slot took an assignment")
	}
	if state, err := h.Resume(context.Background(), host, "ci"); err != nil || state != provider.Paused {
		t.Fatalf("resume: %q %v", state, err)
	}
	wait(provider.Ready, false)
	if _, err := h.Pause(context.Background(), config.Host{Name: "ci-2"}, "ci", false); err == nil {
		t.Fatal("an unknown host was paused")
	}
}
