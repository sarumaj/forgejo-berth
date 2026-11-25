// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

const (
	// quickExit: a runner that exits without a job sooner than that (seconds) failed (it
	// could not reach Forgejo, its configuration is broken): the next start waits.
	quickExit = 60.0
	// maxBackoff is the longest wait after failures (seconds); it doubles from 30 on.
	maxBackoff = 600.0
)

// System is the container as the cycle reads and changes it.
type System interface {
	// Reset stops the container, restores its snapshot, starts it and waits for its boot.
	Reset() error
	// Apply gives the container the assigned runner: its class's cache, its configuration
	// and its token.
	Apply(a provider.Parsed) error
	// StartRunner starts the runner's service.
	StartRunner() error
	// StopRunner asks the runner to stop, without waiting: a signal that lets it cancel its
	// job and report the job's end to Forgejo before it exits.
	StopRunner()
	// RunnerActive tells whether the runner's service has processes.
	RunnerActive() bool
	// Busy tells whether the runner runs a job.
	Busy() bool
	// TookTask tells whether the runner's journal shows it took a task, though no step
	// shows yet (an image still pulling).
	TookTask() bool
	// SaveJournal copies the runner's journal of this boot into the cycle's log; failed
	// says that the runner failed (it did not start, or exited at once without a job).
	SaveJournal(failed bool)
	// Measure returns the task the runner ran (0: none) and the most memory the container
	// used, swap included (MiB).
	Measure() (int64, int)
	// CPU returns the container's CPU time so far and the time some of its tasks waited for
	// a CPU (microseconds; false when the cgroup does not say).
	CPU() (usage, pressure int64, ok bool)
}

// Cycle resets a container, serves one assignment, and starts over.
type Cycle struct {
	settings *Settings
	system   System
	log      *slog.Logger
	now      func() time.Time
	sleep    func(time.Duration)
	stopping func() bool

	state            string
	stateSince       time.Time
	jobs, resets     int
	failures         int
	lastResetSeconds float64
	lastResetAt      time.Time
	assignment       string
	last             *provider.Finished
	// servedSince is when the assignment's runner started, jobSince when its job was first
	// seen, unusedMinutes how long it may wait for one: kept in the state, so that a cycle
	// that resumes takes the assignment up where it was.
	servedSince, jobSince time.Time
	unusedMinutes         float64
	// cpuAtJob is the container's CPU counters when the job was first seen.
	cpuAtJob [2]int64
	cpuOK    bool
	resume   bool
	// held is set while the cycle holds the slot paused, the container reset: a cycle that
	// starts while the slot is still paused leaves the container as it is.
	held bool
}

// CycleOptions are the cycle's collaborators; nil ones are the real ones.
type CycleOptions struct {
	Log      *slog.Logger
	Now      func() time.Time
	Sleep    func(time.Duration)
	Stopping func() bool
	// Resume takes up, at the start, the assignment the slot's last cycle left (a cycle in
	// the broker's process, which a restart of the broker stops), instead of resetting the
	// container under its job.
	Resume bool
}

// NewCycle makes the cycle of one slot.
func NewCycle(s *Settings, system System, o CycleOptions) *Cycle {
	c := &Cycle{settings: s, system: system, log: o.Log, now: o.Now, sleep: o.Sleep, stopping: o.Stopping,
		state: "starting", last: &provider.Finished{}, resume: o.Resume}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = time.Sleep
	}
	if c.stopping == nil {
		c.stopping = func() bool { return false }
	}
	c.stateSince = c.now()
	c.log = c.log.With("container", s.Container)
	return c
}

func (c *Cycle) interval() time.Duration {
	return time.Duration(c.settings.Interval * float64(time.Second))
}

// Run resets, serves one assignment, and starts over, until stopping. A paused slot is
// reset and then held until it is resumed; a slot the last cycle held paused, and still
// paused, is held at once, its container not reset.
func (c *Cycle) Run() {
	saved := c.saved()
	held := saved.Held
	if c.resume && !c.Resume() {
		c.backOff()
	} else if !c.resume && provider.AssignmentID.MatchString(saved.Assignment) && c.system.RunnerActive() {
		// The last cycle's runner, its job perhaps running: stopped before the reset, so
		// that its job's end reaches Forgejo.
		c.log.Warn("the last cycle's runner is still active; stopped before the reset",
			"assignment", saved.Assignment)
		c.stopRunner()
	}
	if paused, _ := c.pauseRequest(); held && paused {
		c.holdPaused()
	}
	for !c.stopping() {
		if !c.Reset() {
			c.backOff()
			continue
		}
		if c.stopping() {
			break
		}
		if paused, _ := c.pauseRequest(); paused {
			c.holdPaused()
			continue
		}
		a, ok := c.WaitAssignment()
		if !ok {
			continue
		}
		if !c.Serve(a) {
			c.backOff()
		}
	}
	last := c.state
	c.setState(provider.Stopped)
	c.log.Info("stopped", "in_state", last)
}

// Reset restores the container; it tells whether that worked.
func (c *Cycle) Reset() bool {
	c.setState(provider.Resetting)
	// An assignment the cycle stopped before taking: the broker has deleted its runner
	// since (the slot takes assignments only while ready, after a reset).
	_ = os.Remove(c.settings.path(".assignment"))
	started := c.now()
	if err := c.system.Reset(); err != nil {
		c.failures++
		c.log.Error("the reset failed", "in_a_row", c.failures, "error", err)
		c.setState(provider.Failed)
		return false
	}
	c.resets++
	c.lastResetSeconds = c.now().Sub(started).Seconds()
	c.lastResetAt = c.now()
	c.log.Info("reset", "snapshot", c.settings.Snapshot, "seconds", math.Round(c.lastResetSeconds))
	c.write()
	return true
}

// pauseRequest tells whether the slot is paused, and whether its job is to be stopped for
// that rather than run to its end.
func (c *Cycle) pauseRequest() (paused, stop bool) {
	data, err := os.ReadFile(c.settings.path(".pause"))
	if err != nil {
		return false, false
	}
	return true, strings.TrimSpace(string(data)) == PauseStop
}

// holdPaused holds the reset slot paused: it takes no assignment, and its container is left
// as it is, until the slot is resumed or the cycle stops.
func (c *Cycle) holdPaused() {
	c.held = true
	c.setState(provider.Paused)
	c.log.Info("paused: takes no assignment until it is resumed")
	for !c.stopping() {
		if paused, _ := c.pauseRequest(); !paused {
			c.held = false
			c.write()
			c.log.Info("resumed")
			return
		}
		c.sleep(c.interval())
	}
}

// WaitAssignment is the ready slot: it returns the next assignment, or false when stopping
// or after idle_hours (the container is refreshed), or after a pause (held until it is
// resumed: the container is then reset). One out of shape is dropped. An assignment taken
// before a pause is served.
func (c *Cycle) WaitAssignment() (provider.Parsed, bool) {
	path := c.settings.path(".assignment")
	// A release that came too late for the last assignment is of no use to the next.
	_ = os.Remove(c.settings.path(".release"))
	c.setState(provider.Ready)
	since := c.now()
	for !c.stopping() {
		if data, err := os.ReadFile(path); err == nil {
			a, err := provider.ParseHandoff(data, c.settings)
			clear(data)
			if err == nil {
				// Held before the file goes: the broker never sees the slot ready without it.
				c.assignment = a.ID
				c.setState(provider.Assigned)
				_ = os.Remove(path)
				return a, true
			}
			c.log.Warn("the assignment was dropped", "error", err)
			_ = os.Remove(path)
		}
		if paused, _ := c.pauseRequest(); paused {
			c.holdPaused()
			return provider.Parsed{}, false
		}
		if c.now().Sub(since).Hours() > c.settings.IdleHours {
			c.log.Info("ready too long, refreshed", "hours", c.settings.IdleHours)
			return provider.Parsed{}, false
		}
		c.sleep(c.interval())
	}
	return provider.Parsed{}, false
}

// released tells whether the broker released the assignment (its file is taken either way).
func (c *Cycle) released(a provider.Parsed) bool {
	path := c.settings.path(".release")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
	return strings.TrimSpace(string(data)) == a.ID
}

// Serve starts the assigned runner and waits for its job to end; false when the runner
// failed.
func (c *Cycle) Serve(a provider.Parsed) bool {
	c.assignment = a.ID
	err := c.system.Apply(a)
	a.Token = ""
	if err == nil {
		err = c.system.StartRunner()
	}
	if err != nil {
		c.failures++
		c.log.Error("the runner did not start", "in_a_row", c.failures, "error", err)
		c.system.SaveJournal(true)
		c.finish(a, time.Time{}, c.now())
		c.setState(provider.Failed)
		return false
	}
	c.setState(provider.Assigned)
	return c.watch(a, c.now(), time.Time{})
}

// watch waits for the runner's job to end, from the runner's start (and its job's, when it
// was seen already); false when the runner failed.
func (c *Cycle) watch(a provider.Parsed, started, jobSince time.Time) bool {
	c.assignment, c.servedSince, c.jobSince, c.unusedMinutes = a.ID, started, jobSince, a.UnusedMinutes
	c.write()
	lastLook := started
	// pausedOut: the runner was stopped for a pause, not failed; cut: the runner is to be
	// stopped (its job cut short, or no job came).
	pausedOut, cut := false, false
	for {
		c.sleep(c.interval())
		if c.stopping() {
			// Whoever stops the cycle takes care of the runner.
			return true
		}
		if !c.system.RunnerActive() {
			if jobSince.IsZero() && c.system.TookTask() {
				// A job that ran between two looks: its CPU use at the start is unknown.
				jobSince, c.jobSince, c.cpuOK = lastLook, lastLook, false
			}
			break
		}
		lastLook = c.now()
		now := c.now()
		if jobSince.IsZero() && c.system.Busy() {
			jobSince = now
			c.jobStarted()
		}
		if jobSince.IsZero() {
			why := ""
			if c.released(a) {
				why = "released by the broker"
			} else if paused, _ := c.pauseRequest(); paused {
				why, pausedOut = "the slot is paused", true
			} else if idle := now.Sub(started); idle.Minutes() > a.UnusedMinutes {
				why = fmt.Sprintf("unused for %g minutes", a.UnusedMinutes)
			}
			if why != "" && c.system.TookTask() {
				// Its job is starting (an image pulled, say): not stopped.
				jobSince = now
				c.jobStarted()
			} else if why != "" {
				c.log.Info("stops its runner, which has no job", "why", why)
				cut = true
				break
			}
		} else if now.Sub(jobSince).Hours() > c.settings.JobHours {
			c.log.Warn("its job ran too long; stopped", "hours", c.settings.JobHours)
			cut = true
			break
		} else if paused, stop := c.pauseRequest(); paused && stop {
			c.log.Warn("its job is stopped: the slot is paused at once")
			cut = true
			break
		}
	}
	ended := c.now()
	if cut && !c.stopRunner() {
		// Stopping: the runner ends on its own, and the next cycle finishes the assignment.
		return true
	}
	failed := jobSince.IsZero() && ended.Sub(started).Seconds() < quickExit && !pausedOut
	c.system.SaveJournal(failed)
	c.finish(a, jobSince, ended)
	if !jobSince.IsZero() {
		c.jobs++
		c.failures = 0
		c.write()
		return true
	}
	if failed {
		c.failures++
		c.log.Error("the runner exited without a job", "in_a_row", c.failures)
		c.setState(provider.Failed)
		return false
	}
	c.failures = 0
	return true
}

// stopRunner stops the runner and waits, up to stop_timeout, for it to exit. Stopped so, a
// runner cancels its job and reports the job's end to Forgejo itself; a container reset
// under a running job leaves the task to Forgejo's reaper of tasks whose runner went, which
// does not resolve the jobs that need it. A runner still active after stop_timeout is left
// to the reset, which kills it. False when the cycle is stopping meanwhile.
func (c *Cycle) stopRunner() bool {
	c.system.StopRunner()
	since := c.now()
	grace := time.Duration(c.settings.StopTimeout * float64(time.Second))
	for c.system.RunnerActive() {
		if c.stopping() {
			return false
		}
		waited := c.now().Sub(since)
		if waited >= grace {
			c.log.Warn("the runner did not exit within stop_timeout; the reset kills it",
				"stop_timeout", c.settings.StopTimeout)
			return true
		}
		c.sleep(min(c.interval(), grace-waited))
	}
	c.log.Info("the runner exited when stopped", "seconds", math.Round(c.now().Sub(since).Seconds()))
	return true
}

// jobStarted notes the CPU counters at the job's start: the boot and the wait before it are
// not the job's.
func (c *Cycle) jobStarted() {
	usage, pressure, ok := c.system.CPU()
	c.cpuAtJob, c.cpuOK, c.jobSince = [2]int64{usage, pressure}, ok, c.now()
	c.setState(provider.Running)
}

// savedState is what a cycle keeps of itself in its state file to resume.
type savedState struct {
	Assignment    string             `json:"assignment"`
	Last          *provider.Finished `json:"last"`
	Jobs          int                `json:"jobs"`
	Resets        int                `json:"resets"`
	ServedSince   int64              `json:"served_since"`
	JobSince      int64              `json:"job_since"`
	UnusedMinutes float64            `json:"unused_minutes"`
	CPUAtJob      [2]int64           `json:"cpu_at_job"`
	CPUOK         bool               `json:"cpu_ok"`
	Held          bool               `json:"held"`
}

// saved reads what the slot's last cycle left in its state file (nothing: zero).
func (c *Cycle) saved() savedState {
	var old savedState
	if data, err := os.ReadFile(c.settings.path(".json")); err == nil {
		_ = json.Unmarshal(data, &old)
	}
	return old
}

// Resume reads what the slot's last cycle left in its state file: the last finished
// assignment, which the broker may not have read yet, and the assignment it served. A runner
// still active is watched to its job's end; one that ended while no cycle watched is
// finished as it was (its job's time then includes the time without a cycle). False when the
// runner failed.
func (c *Cycle) Resume() bool {
	var old savedState
	data, err := os.ReadFile(c.settings.path(".json"))
	if err != nil || json.Unmarshal(data, &old) != nil {
		return true
	}
	if old.Last != nil {
		c.last = old.Last
	}
	c.jobs, c.resets = old.Jobs, old.Resets
	if !provider.AssignmentID.MatchString(old.Assignment) {
		return true
	}
	a := provider.Parsed{ID: old.Assignment, UnusedMinutes: old.UnusedMinutes}
	if a.UnusedMinutes <= 0 {
		a.UnusedMinutes = 15
	}
	served, jobSince := c.now(), time.Time{}
	if old.ServedSince > 0 {
		served = time.Unix(old.ServedSince, 0)
	}
	if old.JobSince > 0 {
		jobSince = time.Unix(old.JobSince, 0)
	}
	c.cpuAtJob, c.cpuOK = old.CPUAtJob, old.CPUOK
	c.assignment = a.ID
	if c.system.RunnerActive() {
		c.log.Info("took its runner up again", "assignment", a.ID, "running", !jobSince.IsZero())
		if jobSince.IsZero() {
			c.setState(provider.Assigned)
		} else {
			c.setState(provider.Running)
		}
		return c.watch(a, served, jobSince)
	}
	if jobSince.IsZero() && c.system.TookTask() {
		jobSince = served
	}
	c.log.Info("its runner ended while no cycle watched it", "assignment", a.ID)
	c.system.SaveJournal(false)
	c.finish(a, jobSince, c.now())
	if !jobSince.IsZero() {
		c.jobs++
		c.write()
	}
	return true
}

// finish notes for the broker what the assignment's runner did: its task and the memory
// it needed.
func (c *Cycle) finish(a provider.Parsed, jobSince, ended time.Time) {
	last := &provider.Finished{Assignment: a.ID, Unused: jobSince.IsZero()}
	if !jobSince.IsZero() {
		last.Task, last.PeakMiB = c.system.Measure()
		seconds := ended.Sub(jobSince).Seconds()
		last.Seconds = math.Round(seconds*10) / 10
		if usage, pressure, ok := c.system.CPU(); ok && c.cpuOK && seconds > 0 {
			last.CPUCores = round3(float64(usage-c.cpuAtJob[0]) / 1e6 / seconds)
			last.CPUPressure = round3(min(1, float64(pressure-c.cpuAtJob[1])/1e6/seconds))
		}
		c.log.Info("finished", "assignment", a.ID, "task", last.Task, "peak_mib", last.PeakMiB,
			"cpu_cores", last.CPUCores, "cpu_pressure", last.CPUPressure)
	} else {
		c.log.Info("finished without a job", "assignment", a.ID)
	}
	c.last = last
	c.assignment, c.servedSince, c.jobSince = "", time.Time{}, time.Time{}
	c.write()
}

func (c *Cycle) backOff() {
	delay := min(30*math.Pow(2, float64(max(c.failures-1, 0))), maxBackoff)
	until := c.now().Add(time.Duration(delay * float64(time.Second)))
	for !c.stopping() && c.now().Before(until) {
		c.sleep(min(c.interval(), until.Sub(c.now())))
	}
}

func (c *Cycle) setState(state string) {
	if state != c.state {
		c.stateSince = c.now()
	}
	c.state = state
	c.write()
}

// State returns the cycle's state.
func (c *Cycle) State() string { return c.state }

// Last returns the last finished assignment.
func (c *Cycle) Last() provider.Finished { return *c.last }

// write publishes the state (state_dir/<container>.json) and, with a textfile directory,
// the metrics for node_exporter's textfile collector.
func (c *Cycle) write() {
	s := c.settings
	state := map[string]any{
		"state":              c.state,
		"state_since":        c.stateSince.Unix(),
		"jobs":               c.jobs,
		"resets":             c.resets,
		"failures":           c.failures,
		"last_reset_seconds": math.Round(c.lastResetSeconds*10) / 10,
		"last_reset_at":      unix(c.lastResetAt),
		"assignment":         c.assignment,
		"last":               c.last,
		"served_since":       unix(c.servedSince),
		"job_since":          unix(c.jobSince),
		"unused_minutes":     c.unusedMinutes,
		"cpu_at_job":         c.cpuAtJob,
		"cpu_ok":             c.cpuOK,
		"held":               c.held,
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := replace(s.path(".json"), append(data, '\n')); err != nil {
		c.log.Error("the state was not written", "error", err)
	}
	if s.TextfileDir == "" {
		return
	}
	if st, err := os.Stat(s.TextfileDir); err != nil || !st.IsDir() {
		return
	}
	metrics := provider.CycleMetrics([]provider.CycleStats{{Container: s.Container, State: c.state,
		Jobs: c.jobs, Failures: c.failures, LastResetSeconds: c.lastResetSeconds}})
	name := "forgejo_berth_cycle_" + strings.ReplaceAll(s.Container, "-", "_") + ".prom"
	if err := replace(filepath.Join(s.TextfileDir, name), []byte(metrics)); err != nil {
		c.log.Error("the metrics were not written", "error", err)
	}
}

func round3(v float64) float64 {
	return math.Round(max(v, 0)*1000) / 1000
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// replace writes a file at once: readers never see half of it.
func replace(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
