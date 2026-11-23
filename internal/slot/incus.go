// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// writeFile writes standard input to $1 as root:$2, 0640, at once (the runner never reads
// half of it), from the shell's own standard input: reopening it (install /dev/stdin) is
// refused in an unprivileged container, where the pipe Incus passes in belongs to a host
// user the container does not map.
const writeFile = `umask 027 && cat > "$1.tmp" && chgrp "$2" "$1.tmp" && mv -f "$1.tmp" "$1"`

// Incus is the System of a container managed by Incus on this host; the runner is read
// through the container's cgroups, from the host.
type Incus struct {
	settings *Settings
	cgroup   string
	// Out gets the runner's journal (SaveJournal).
	Out io.Writer
	// run runs incus with its arguments; tests replace it.
	run func(ctx context.Context, stdin []byte, args ...string) (stdout []byte, stderr []byte, err error)
}

// NewIncus makes the container's system.
func NewIncus(s *Settings) *Incus {
	return &Incus{settings: s, cgroup: filepath.Join(s.CgroupRoot, "lxc.payload."+s.Container), Out: os.Stdout,
		run: runIncus}
}

func runIncus(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "incus", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (i *Incus) must(timeout time.Duration, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, errOut, err := i.run(ctx, stdin, args...)
	if err != nil {
		n := min(2, len(args))
		return nil, fmt.Errorf("incus %s: %v: %s", strings.Join(args[:n], " "), err, strings.TrimSpace(string(errOut)))
	}
	return out, nil
}

func (i *Incus) exec(stdin []byte, args ...string) ([]byte, error) {
	return i.must(2*time.Minute, stdin, append([]string{"exec", i.settings.Container, "--"}, args...)...)
}

func (i *Incus) running() (bool, error) {
	out, err := i.must(time.Minute, nil, "query", "/1.0/instances/"+i.settings.Container+"/state")
	if err != nil {
		return false, err
	}
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return false, err
	}
	return state.Status == "Running", nil
}

// Reset stops the container, restores its snapshot, starts it and waits for its boot.
func (i *Incus) Reset() error {
	name := i.settings.Container
	up, err := i.running()
	if err != nil {
		return err
	}
	if up {
		if _, err := i.must(2*time.Minute, nil, "stop", "--force", name); err != nil {
			return err
		}
	}
	if _, err := i.must(15*time.Minute, nil, "snapshot", "restore", name, i.settings.Snapshot); err != nil {
		return err
	}
	if _, err := i.must(2*time.Minute, nil, "start", name); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Duration(i.settings.BootTimeout * float64(time.Second)))
	state, lastErr := "", ""
	for {
		left := max(1, int(time.Until(deadline).Seconds()))
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(left+30)*time.Second)
		out, errOut, _ := i.run(ctx, nil, "exec", name, "--", "timeout", strconv.Itoa(left), "systemctl",
			"is-system-running", "--wait")
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
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, _, _ := i.run(ctx, nil, "exec", i.settings.Container, "--", "systemctl", "--failed", "--no-legend", "--plain")
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	out, _, err := i.run(ctx, nil, "info", i.settings.Container, "--show-log")
	cancel()
	if err == nil {
		var log []string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) != "" {
				log = append(log, line)
			}
		}
		for n, line := range log {
			if line == "Log:" {
				log = log[n+1:]
				break
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
	s, name := i.settings, i.settings.Container
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, _, _ = i.run(ctx, nil, "config", "device", "remove", name, "cache")
	cancel()
	if volume := s.CacheVolumes[a.Class]; volume != "" {
		if _, err := i.must(time.Minute, nil, "config", "device", "add", name, "cache", "disk",
			"pool="+s.CachePool, "source="+volume, "path="+s.CachePath); err != nil {
			return err
		}
		// The volume's root hides the runner's directory there: the runner's, as that was.
		if _, err := i.exec(nil, "install", "-d", "-o", s.RunnerUser, "-g", s.RunnerUser, "-m", "0700",
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
	if _, err := i.exec([]byte(config), "sh", "-c", writeFile, "write", s.ConfigPath, s.RunnerUser); err != nil {
		return err
	}
	token := []byte(a.Token)
	_, err = i.exec(token, "sh", "-c", writeFile, "write", s.TokenPath, s.RunnerUser)
	clear(token)
	return err
}

// StartRunner starts the runner's service in the container.
func (i *Incus) StartRunner() error {
	_, err := i.exec(nil, "systemctl", "start", i.settings.RunnerService+".service")
	return err
}

// StopRunner asks the runner's service to stop, without waiting.
func (i *Incus) StopRunner() {
	_, _ = i.exec(nil, "systemctl", "stop", "--no-block", i.settings.RunnerService+".service")
}

// runnerCgroup is the runner service's cgroup in the container: a system service, or one
// of a user's.
func (i *Incus) runnerCgroup() string {
	unit := i.settings.RunnerService + ".service"
	candidates := []string{filepath.Join(i.cgroup, "system.slice", unit)}
	found, _ := filepath.Glob(filepath.Join(i.cgroup, "user.slice", "user-*.slice", unit))
	candidates = append(candidates, found...)
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

func (i *Incus) pids() []string {
	cg := i.runnerCgroup()
	if cg == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

// RunnerActive tells whether the runner's service has processes (read from the host).
func (i *Incus) RunnerActive() bool {
	return len(i.pids()) > 0
}

// Busy sees a job: a step (a child of the runner), a host-mode job's directory in the work
// directory, or a job's Docker container. The container is new for every job, so any such
// directory is this job's.
func (i *Incus) Busy() bool {
	pids := i.pids()
	if len(pids) > 1 {
		return true
	}
	if len(pids) == 1 {
		work := filepath.Join("/proc", pids[0], "root", strings.TrimPrefix(i.settings.WorkDir, "/"))
		if found, _ := filepath.Glob(filepath.Join(work, "*", "hostexecutor")); len(found) > 0 {
			return true
		}
	}
	cg := i.runnerCgroup()
	if cg == "" {
		return false
	}
	busy := false
	_ = filepath.WalkDir(filepath.Dir(cg), func(path string, d os.DirEntry, err error) error {
		if err != nil || busy || !d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), "docker-") && strings.HasSuffix(d.Name(), ".scope") {
			if events, err := os.ReadFile(filepath.Join(path, "cgroup.events")); err == nil &&
				strings.Contains(string(events), "populated 1") {
				busy = true
			}
		}
		return nil
	})
	return busy
}

func (i *Incus) journal(format string, lines int) ([]byte, error) {
	args := []string{"journalctl", "--boot", "--no-pager", "--output=" + format,
		"--unit=" + i.settings.RunnerService + ".service"}
	if lines > 0 {
		args = append(args, "--lines="+strconv.Itoa(lines))
	}
	return i.exec(nil, args...)
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
// swap from its cgroup (it was started for this job, so the peak is the job's), in MiB
// rounded up.
func (i *Incus) Measure() (int64, int) {
	return i.task(), PeakMiB(i.cgroup)
}

// CPU reads the container's cgroup: cpu.stat's usage_usec and cpu.pressure's "some" total.
func (i *Incus) CPU() (int64, int64, bool) {
	return CPUCounters(i.cgroup)
}

// CPUCounters returns a cgroup's CPU time (cpu.stat usage_usec) and the time some of its
// tasks waited for a CPU (cpu.pressure, some total), in microseconds. Without pressure
// accounting (no cpu.pressure) the pressure is 0.
func CPUCounters(cgroup string) (int64, int64, bool) {
	stat, err := os.ReadFile(filepath.Join(cgroup, "cpu.stat"))
	if err != nil {
		return 0, 0, false
	}
	usage, ok := field(string(stat), "usage_usec ")
	if !ok {
		return 0, 0, false
	}
	var pressure int64
	if data, err := os.ReadFile(filepath.Join(cgroup, "cpu.pressure")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "some ") {
				pressure, _ = field(strings.ReplaceAll(line, " ", "\n"), "total=")
			}
		}
	}
	return usage, pressure, true
}

// field reads the number after prefix at a line's start.
func field(text, prefix string) (int64, bool) {
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// PeakMiB is memory.peak plus memory.swap.peak of the cgroup, in MiB rounded up.
func PeakMiB(cgroup string) int {
	var bytes int64
	for _, counter := range []string{"memory.peak", "memory.swap.peak"} {
		data, err := os.ReadFile(filepath.Join(cgroup, counter))
		if err != nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			bytes += n
		}
	}
	return int((bytes + 1<<20 - 1) >> 20)
}
