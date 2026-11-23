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
	// StopRunner asks the runner to stop (it has no job), without waiting.
	StopRunner()
	// RunnerActive tells whether the runner's service has processes.
	RunnerActive() bool
	// Busy tells whether the runner runs a job.
	Busy() bool
	// TookTask tells whether the runner's journal shows it took a task, though no step
	// shows yet (an image still pulling).
	TookTask() bool
	// SaveJournal copies the runner's journal of this boot into the cycle's log.
	SaveJournal()
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
	// cpuAtJob is the container's CPU counters when the job was first seen.
	cpuAtJob [2]int64
	cpuOK    bool
}

// CycleOptions are the cycle's collaborators; nil ones are the real ones.
type CycleOptions struct {
	Log      *slog.Logger
	Now      func() time.Time
	Sleep    func(time.Duration)
	Stopping func() bool
}

// NewCycle makes the cycle of one slot.
func NewCycle(s *Settings, system System, o CycleOptions) *Cycle {
	c := &Cycle{settings: s, system: system, log: o.Log, now: o.Now, sleep: o.Sleep, stopping: o.Stopping,
		state: "starting", last: &provider.Finished{}}
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

// Run resets, serves one assignment, and starts over, until stopping.
func (c *Cycle) Run() {
	for !c.stopping() {
		if !c.Reset() {
			c.backOff()
			continue
		}
		if c.stopping() {
			break
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

// WaitAssignment is the ready slot: it returns the next assignment, or false when stopping
// or after idle_hours (the container is refreshed). One out of shape is dropped.
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
		c.finish(a, time.Time{}, c.now())
		c.setState(provider.Failed)
		return false
	}
	c.setState(provider.Assigned)
	started := c.now()
	var jobSince time.Time
	for {
		c.sleep(c.interval())
		if c.stopping() {
			// Whoever stops the cycle takes care of the runner.
			return true
		}
		if !c.system.RunnerActive() {
			break
		}
		now := c.now()
		if jobSince.IsZero() && c.system.Busy() {
			jobSince = now
			c.jobStarted()
		}
		if jobSince.IsZero() {
			why := ""
			if c.released(a) {
				why = "released by the broker"
			} else if idle := now.Sub(started); idle.Minutes() > a.UnusedMinutes {
				why = fmt.Sprintf("unused for %g minutes", a.UnusedMinutes)
			}
			if why != "" && c.system.TookTask() {
				// Its job is starting (an image pulled, say): not stopped.
				jobSince = now
				c.jobStarted()
			} else if why != "" {
				c.log.Info("stops its runner, which has no job", "why", why)
				c.system.StopRunner()
				break
			}
		} else if now.Sub(jobSince).Hours() > c.settings.JobHours {
			c.log.Warn("its job ran too long; reset", "hours", c.settings.JobHours)
			break
		}
	}
	c.system.SaveJournal()
	c.finish(a, jobSince, c.now())
	if !jobSince.IsZero() {
		c.jobs++
		c.failures = 0
		c.write()
		return true
	}
	if c.now().Sub(started).Seconds() < quickExit {
		c.failures++
		c.log.Error("the runner exited without a job", "in_a_row", c.failures)
		c.setState(provider.Failed)
		return false
	}
	c.failures = 0
	return true
}

// jobStarted notes the CPU counters at the job's start: the boot and the wait before it are
// not the job's.
func (c *Cycle) jobStarted() {
	usage, pressure, ok := c.system.CPU()
	c.cpuAtJob, c.cpuOK = [2]int64{usage, pressure}, ok
	c.setState(provider.Running)
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
	c.assignment = ""
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
	label := fmt.Sprintf("container=%q", s.Container)
	var b strings.Builder
	b.WriteString("# HELP forgejo_berth_cycle_state The slot's state in its reset cycle (1: the current one).\n")
	b.WriteString("# TYPE forgejo_berth_cycle_state gauge\n")
	for _, each := range []string{provider.Resetting, provider.Ready, provider.Assigned, provider.Running,
		provider.Failed, provider.Stopped} {
		fmt.Fprintf(&b, "forgejo_berth_cycle_state{%s,state=%q} %d\n", label, each, btoi(each == c.state))
	}
	fmt.Fprintf(&b, "# HELP forgejo_berth_cycle_jobs_total Jobs the slot ran since the cycle started.\n"+
		"# TYPE forgejo_berth_cycle_jobs_total counter\nforgejo_berth_cycle_jobs_total{%s} %d\n", label, c.jobs)
	fmt.Fprintf(&b, "# HELP forgejo_berth_cycle_failures Resets or runner starts that failed in a row.\n"+
		"# TYPE forgejo_berth_cycle_failures gauge\nforgejo_berth_cycle_failures{%s} %d\n", label, c.failures)
	fmt.Fprintf(&b, "# HELP forgejo_berth_cycle_last_reset_seconds How long the last reset took.\n"+
		"# TYPE forgejo_berth_cycle_last_reset_seconds gauge\nforgejo_berth_cycle_last_reset_seconds{%s} %.1f\n",
		label, c.lastResetSeconds)
	name := "forgejo_berth_cycle_" + strings.ReplaceAll(s.Container, "-", "_") + ".prom"
	if err := replace(filepath.Join(s.TextfileDir, name), []byte(b.String())); err != nil {
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

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
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
