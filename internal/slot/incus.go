// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// TaskLine is the runner's own line when it takes its task, at its message's start, in
// logrus' text or JSON format, as forgejo-runner 13 writes it:
//
//	msg="task 1 repo is alice/app https://data.forgejo.org https://forgejo.example.org"
//
// A job's output never reaches the runner's journal (it goes to Forgejo), and only the
// first such line counts: a runner takes one task.
var TaskLine = regexp.MustCompile(`(?:\bmsg="|"msg":\s*")task (\d+) repo is `)

// The configuration's placeholders.
const (
	UUIDPlaceholder   = "@@RUNNER_UUID@@"
	LabelsPlaceholder = "@@RUNNER_LABELS@@"
)

// Instance is an Incus container as the reset cycle drives it: through the incus command on
// its own host (the cycle of the SSH provider's slot side), or through the Incus API (the
// incus provider, from the broker). What the cycle does with it is the same for both.
type Instance interface {
	// Running tells whether the container runs.
	Running(ctx context.Context) (bool, error)
	// Stop stops the container at once.
	Stop(ctx context.Context) error
	// Restore restores the stopped container's snapshot.
	Restore(ctx context.Context, snapshot string) error
	// Start starts the container.
	Start(ctx context.Context) error
	// Exec runs a command in the container as root; an exit code other than 0 is an error,
	// and the output is returned either way.
	Exec(ctx context.Context, args ...string) (stdout, stderr []byte, err error)
	// WriteFile writes a file in the container as root:group, 0640, at once: the runner
	// never reads half of it.
	WriteFile(ctx context.Context, path, group string, data []byte) error
	// SetCache removes the container's device "cache" and, for a volume, attaches that
	// volume of the pool at path.
	SetCache(ctx context.Context, pool, volume, path string) error
	// Log returns the container's own log (LXC's and its console's).
	Log(ctx context.Context) (string, error)
	// Runner tells whether the runner's unit has processes, and whether it runs a job: a
	// step (a child of the runner), a host-mode job's directory in the work directory, or a
	// populated Docker scope beside it.
	Runner(unit, workDir string) (active, busy bool)
	// PeakMiB is the most memory the container used since its start, swap included.
	PeakMiB() int
	// CPU is the container's CPU time and CPU pressure "some" total (microseconds).
	CPU() (usage, pressure int64, ok bool)
}

// Incus is the System of a container managed by Incus.
type Incus struct {
	settings *Settings
	instance Instance
	// Out gets the runner's journal (SaveJournal).
	Out io.Writer
}

// NewIncus makes the system of a container on this host, through the incus command.
func NewIncus(s *Settings) *Incus {
	return NewIncusOn(s, NewIncusCommand(s))
}

// NewIncusOn makes the system of the container that the instance reaches.
func NewIncusOn(s *Settings, instance Instance) *Incus {
	return &Incus{settings: s, instance: instance, Out: os.Stdout}
}

func timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func (i *Incus) exec(args ...string) ([]byte, error) {
	ctx, cancel := timeout(2 * time.Minute)
	defer cancel()
	out, errOut, err := i.instance.Exec(ctx, args...)
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", args[0], err, strings.TrimSpace(string(errOut)))
	}
	return out, nil
}

func (i *Incus) running() (bool, error) {
	ctx, cancel := timeout(time.Minute)
	defer cancel()
	return i.instance.Running(ctx)
}

// Reset stops the container, restores its snapshot, starts it and waits for its boot.
func (i *Incus) Reset() error {
	up, err := i.running()
	if err != nil {
		return err
	}
	steps := []struct {
		do      func(context.Context) error
		timeout time.Duration
	}{
		{i.instance.Stop, 2 * time.Minute},
		{func(ctx context.Context) error { return i.instance.Restore(ctx, i.settings.Snapshot) }, 15 * time.Minute},
		{i.instance.Start, 2 * time.Minute},
	}
	if !up {
		steps = steps[1:]
	}
	for _, step := range steps {
		ctx, cancel := timeout(step.timeout)
		err := step.do(ctx)
		cancel()
		if err != nil {
			return err
		}
	}
	deadline := time.Now().Add(time.Duration(i.settings.BootTimeout * float64(time.Second)))
	state, lastErr := "", ""
	for {
		left := max(1, int(time.Until(deadline).Seconds()))
		ctx, cancel := timeout(time.Duration(left+30) * time.Second)
		out, errOut, _ := i.instance.Exec(ctx, "timeout", strconv.Itoa(left), "systemctl", "is-system-running", "--wait")
		cancel()
		state, lastErr = strings.TrimSpace(string(out)), strings.TrimSpace(string(errOut))
		// Right after the start, systemd in the container does not answer yet: --wait then
		// returns at once, offline or without its bus, instead of waiting. Asked again until
		// the boot timeout, while the container runs.
		up, _ := i.running()
		if (state != "" && state != "offline") || time.Now().After(deadline) || !up {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if state != "running" && state != "degraded" {
		return fmt.Errorf("the container did not boot: %q%s", state, i.bootDiagnosis(lastErr))
	}
	return nil
}

// bootLogLines is how much of the container's own log a failed boot reports.
const bootLogLines = 20

// bootDiagnosis says why a boot failed, for the journal (the next reset loses it): what
// Incus said, the units that failed while the container still runs, and the end of its own
// log (its console and LXC's). A part that cannot be read is left out.
func (i *Incus) bootDiagnosis(incusErr string) string {
	var parts []string
	if incusErr != "" {
		parts = append(parts, "incus: "+incusErr)
	}
	if up, err := i.running(); err == nil && up {
		ctx, cancel := timeout(time.Minute)
		out, _, _ := i.instance.Exec(ctx, "systemctl", "--failed", "--no-legend", "--plain")
		cancel()
		var units []string
		for _, line := range strings.Split(string(out), "\n") {
			if fields := strings.Fields(line); len(fields) > 0 {
				units = append(units, fields[0])
			}
		}
		parts = append(parts, "failed units: "+orNone(strings.Join(units, ", ")))
	} else if err == nil {
		parts = append(parts, "the container is not running")
	}
	ctx, cancel := timeout(time.Minute)
	text, err := i.instance.Log(ctx)
	cancel()
	if err == nil {
		var log []string
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) != "" {
				log = append(log, line)
			}
		}
		if len(log) > bootLogLines {
			log = log[len(log)-bootLogLines:]
		}
		if len(log) > 0 {
			parts = append(parts, "its log ends:\n  "+strings.Join(log, "\n  "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Apply attaches the class's cache volume (in place of any the snapshot had) and writes the
// runner's configuration, with its UUID and labels, and its token.
func (i *Incus) Apply(a provider.Parsed) error {
	s := i.settings
	volume := s.CacheVolumes[a.Class]
	ctx, cancel := timeout(time.Minute)
	err := i.instance.SetCache(ctx, s.CachePool, volume, s.CachePath)
	cancel()
	if err != nil {
		return err
	}
	if volume != "" {
		// The volume's root hides the runner's directory there: the runner's, as that was.
		if _, err := i.exec("install", "-d", "-o", s.RunnerUser, "-g", s.RunnerUser, "-m", "0700",
			s.CachePath); err != nil {
			return err
		}
	}
	template, err := os.ReadFile(s.ConfigTemplate)
	if err != nil {
		return err
	}
	labels, _ := json.Marshal(a.Labels)
	config := strings.ReplaceAll(strings.ReplaceAll(string(template), UUIDPlaceholder, a.UUID),
		LabelsPlaceholder, string(labels))
	ctx, cancel = timeout(2 * time.Minute)
	defer cancel()
	if err := i.instance.WriteFile(ctx, s.ConfigPath, s.RunnerUser, []byte(config)); err != nil {
		return err
	}
	token := []byte(a.Token)
	err = i.instance.WriteFile(ctx, s.TokenPath, s.RunnerUser, token)
	clear(token)
	return err
}

// StartRunner starts the runner's service in the container.
func (i *Incus) StartRunner() error {
	_, err := i.exec("systemctl", "start", i.settings.RunnerService+".service")
	return err
}

// StopRunner asks the runner's service to stop, without waiting.
func (i *Incus) StopRunner() {
	_, _ = i.exec("systemctl", "stop", "--no-block", i.settings.RunnerService+".service")
}

// RunnerActive tells whether the runner's service has processes.
func (i *Incus) RunnerActive() bool {
	active, _ := i.instance.Runner(i.settings.RunnerService+".service", i.settings.WorkDir)
	return active
}

// Busy sees a job: a step (a child of the runner), a host-mode job's directory in the work
// directory, or a job's Docker container. The container is new for every job, so any such
// directory is this job's.
func (i *Incus) Busy() bool {
	_, busy := i.instance.Runner(i.settings.RunnerService+".service", i.settings.WorkDir)
	return busy
}

func (i *Incus) journal(format string, lines int) ([]byte, error) {
	args := []string{"journalctl", "--boot", "--no-pager", "--output=" + format,
		"--unit=" + i.settings.RunnerService + ".service"}
	if lines > 0 {
		args = append(args, "--lines="+strconv.Itoa(lines))
	}
	return i.exec(args...)
}

// task is the task the runner took, from its journal of this boot (0: none).
func (i *Incus) task() int64 {
	out, err := i.journal("cat", 0)
	if err != nil {
		return 0
	}
	return ParseTask(string(out))
}

// ParseTask returns the task of the journal's first task line (0: none).
func ParseTask(journal string) int64 {
	m := TaskLine.FindStringSubmatch(journal)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// TookTask tells whether the runner's journal shows it took a task.
func (i *Incus) TookTask() bool {
	return i.task() != 0
}

// SaveJournal prints the runner's journal of this boot into the cycle's.
func (i *Incus) SaveJournal() {
	if up, err := i.running(); err != nil || !up {
		return
	}
	out, err := i.journal("short-iso", i.settings.JournalLines)
	if err != nil {
		fmt.Fprintf(i.Out, "the runner's journal could not be read: %v\n", err)
		return
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fmt.Fprintf(i.Out, "runner: %s\n", line)
	}
}

// Measure returns the task from the runner's journal, and the container's peak memory and
// swap (it was started for this job, so the peak is the job's), in MiB rounded up.
func (i *Incus) Measure() (int64, int) {
	return i.task(), i.instance.PeakMiB()
}

// CPU reads the container's CPU time and pressure.
func (i *Incus) CPU() (int64, int64, bool) {
	return i.instance.CPU()
}
