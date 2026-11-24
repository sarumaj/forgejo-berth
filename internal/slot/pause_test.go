// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

func pause(t *testing.T, dir, mode string) {
	t.Helper()
	write(t, filepath.Join(dir, "run/ci.pause"), mode+"\n")
}

// A paused slot is reset and held: it takes no assignment, and its container is left alone
// until it is resumed; a cycle that starts while it is still held does not reset it.
func TestAPausedSlotIsResetThenHeld(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock()}
	c := newCycle(t, dir, sys, sys.clock.stop(20))
	pause(t, dir, PauseFinish)
	assignedLate := false
	var metrics []byte
	c.sleep = func(d time.Duration) {
		sys.clock.Sleep(d)
		if !assignedLate {
			metrics, _ = os.ReadFile(filepath.Join(dir, "textfile/forgejo_berth_cycle_ci.prom"))
			// An assignment written while it is held (no broker sends one): never taken.
			assign(t, dir, nil)
			assignedLate = true
		}
	}
	c.Run()
	if !slices.Equal(sys.calls, []string{"reset"}) {
		t.Fatalf("calls %v", sys.calls)
	}
	if st := readCycleState(t, dir); st["held"] != true || st["state"] != "stopped" {
		t.Fatalf("state %v", st)
	}
	if !strings.Contains(string(metrics), `forgejo_berth_cycle_state{container="ci",state="paused"} 1`) {
		t.Fatalf("metrics while held:\n%s", metrics)
	}
	if !exists(filepath.Join(dir, "run/ci.assignment")) {
		t.Fatal("the paused slot took an assignment")
	}
	_ = os.Remove(filepath.Join(dir, "run/ci.assignment"))

	// The broker restarts: the slot stays as it is, paused.
	sys.calls = nil
	again := newCycle(t, dir, sys, sys.clock.stop(5))
	again.resume = true
	again.Run()
	if len(sys.calls) != 0 {
		t.Fatalf("a held slot was touched at the start: %v", sys.calls)
	}

	// Resumed: reset, then ready.
	_ = os.Remove(filepath.Join(dir, "run/ci.pause"))
	third := newCycle(t, dir, sys, sys.clock.stop(8))
	third.resume = true
	third.Run()
	if !slices.Equal(sys.calls, []string{"reset"}) {
		t.Fatalf("calls after the resume %v", sys.calls)
	}
	if st := readCycleState(t, dir); st["held"] != false {
		t.Fatalf("state %v", st)
	}
}

// A ready slot paused leaves its wait and is held; resumed, it is reset.
func TestAReadySlotPausedIsHeldThenReset(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock()}
	c := newCycle(t, dir, sys, sys.clock.stop(1000))
	looks := 0
	c.sleep = func(d time.Duration) {
		sys.clock.Sleep(d)
		looks++
		switch looks {
		case 2:
			pause(t, dir, PauseFinish)
		case 4:
			if c.State() != provider.Paused {
				t.Errorf("state %s while paused", c.State())
			}
			_ = os.Remove(filepath.Join(dir, "run/ci.pause"))
		}
	}
	if _, ok := c.WaitAssignment(); ok || c.held {
		t.Fatal("a paused slot took an assignment, or is still held")
	}
}

// A job runs to its end when its slot is paused; a runner without a job is stopped, which
// is no failure of the slot.
func TestAPauseLetsTheJobEnd(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: append(repeat(look{true, true}, 50), look{}), task: 5, peak: 700}
	c := newCycle(t, dir, sys, sys.clock.stop(1000))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	pause(t, dir, PauseFinish)
	if !c.Serve(a) || slices.Contains(sys.calls, "stop") || c.Last().Unused || c.Last().Task != 5 {
		t.Fatalf("calls %v, last %+v", sys.calls, c.Last())
	}

	dir = t.TempDir()
	sys = &fakeSystem{clock: newClock(), looks: repeat(look{true, false}, 50)}
	c = newCycle(t, dir, sys, sys.clock.stop(1000))
	assign(t, dir, nil)
	a, _ = c.WaitAssignment()
	pause(t, dir, PauseFinish)
	if !c.Serve(a) || !slices.Contains(sys.calls, "stop") || !c.Last().Unused || c.State() == provider.Failed ||
		c.failures != 0 {
		t.Fatalf("calls %v, last %+v, state %s", sys.calls, c.Last(), c.State())
	}
}

// Paused at once, a running job is cut: the cycle notes it and goes on to the reset.
func TestAPauseAtOnceStopsTheJob(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: repeat(look{true, true}, 5000), task: 5, peak: 700}
	c := newCycle(t, dir, sys, sys.clock.stop(100000))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	start := sys.clock.now
	c.sleep = func(d time.Duration) {
		sys.clock.Sleep(d)
		if sys.clock.now.Sub(start) > time.Minute {
			pause(t, dir, PauseStop)
		}
	}
	if !c.Serve(a) || sys.clock.now.Sub(start) > 2*time.Minute || c.Last().Task != 5 {
		t.Fatalf("ran %v, last %+v", sys.clock.now.Sub(start), c.Last())
	}
}
