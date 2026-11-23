// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// clock is a clock that sleeping moves on.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time        { return c.now }
func (c *clock) Sleep(d time.Duration) { c.now = c.now.Add(d) }
func (c *clock) advance(s float64)     { c.Sleep(time.Duration(s * float64(time.Second))) }
func newClock() *clock                 { return &clock{now: time.Unix(1_000_000, 0)} }
func (c *clock) stop(after int) func() bool {
	left := after
	return func() bool { left--; return left < 0 }
}

// look is what one look at the runner sees: active, busy.
type look struct{ active, busy bool }

// fakeSystem is a container whose runner follows a script of looks.
type fakeSystem struct {
	clock      *clock
	calls      []string
	looks      []look
	current    look
	resetFails int
	startFails int
	applyFails int
	took       bool
	task       int64
	peak       int
}

func (f *fakeSystem) Reset() error {
	f.calls = append(f.calls, "reset")
	f.clock.advance(3)
	if f.resetFails > 0 {
		f.resetFails--
		return errors.New("restore failed")
	}
	return nil
}

func (f *fakeSystem) Apply(a provider.Parsed) error {
	f.calls = append(f.calls, "apply "+a.Class+" "+strings.Join(a.Labels, " "))
	if f.applyFails > 0 {
		f.applyFails--
		return errors.New("the cache volume is missing")
	}
	return nil
}

func (f *fakeSystem) StartRunner() error {
	f.calls = append(f.calls, "start")
	if f.startFails > 0 {
		f.startFails--
		return errors.New("dependency failed")
	}
	return nil
}

func (f *fakeSystem) StopRunner() {
	f.calls = append(f.calls, "stop")
	f.looks = []look{{false, false}}
}

func (f *fakeSystem) RunnerActive() bool {
	f.current = look{}
	if len(f.looks) > 0 {
		f.current, f.looks = f.looks[0], f.looks[1:]
	}
	return f.current.active
}

func (f *fakeSystem) Busy() bool { return f.current.busy }

func (f *fakeSystem) TookTask() bool {
	f.calls = append(f.calls, "took")
	return f.took
}

func (f *fakeSystem) SaveJournal() { f.calls = append(f.calls, "journal") }

func (f *fakeSystem) CPU() (int64, int64, bool) {
	// One core busy, a tenth of the time waiting: the counters follow the clock.
	usec := f.clock.now.UnixMicro()
	return usec, usec / 10, true
}

func (f *fakeSystem) Measure() (int64, int) {
	f.calls = append(f.calls, "measure")
	return f.task, f.peak
}

func repeat(l look, n int) []look {
	out := make([]look, n)
	for i := range out {
		out[i] = l
	}
	return out
}

func newCycle(t *testing.T, dir string, sys *fakeSystem, stop func() bool) *Cycle {
	t.Helper()
	s := &Settings{Container: "ci", StateDir: filepath.Join(dir, "run"), TextfileDir: filepath.Join(dir, "textfile"),
		ConfigTemplate: "/dev/null",
		LabelTables: map[string]map[string]string{
			"trusted": {"ubuntu-latest": "ubuntu-latest:host", "docker": "docker:docker://node:24-trixie"},
			"shared":  {"docker": "docker:docker://node:24-trixie"},
		},
		CacheVolumes: map[string]string{"trusted": "ci-cache-trusted", "shared": "ci-cache-shared"}}
	s.defaults()
	_ = os.MkdirAll(s.TextfileDir, 0o755)
	_ = os.MkdirAll(s.StateDir, 0o755)
	return NewCycle(s, sys, CycleOptions{Log: slog.New(slog.DiscardHandler), Now: sys.clock.Now,
		Sleep: sys.clock.Sleep, Stopping: stop})
}

func assign(t *testing.T, dir string, changes map[string]any) {
	t.Helper()
	data := map[string]any{"assignment": "ci-1-ci-1-7", "uuid": testUUID, "token": testToken,
		"labels": []string{"ubuntu-latest"}, "class": "trusted", "unused_minutes": 15}
	maps.Copy(data, changes)
	text, _ := json.Marshal(data)
	write(t, filepath.Join(dir, "run/ci.assignment"), string(text))
}

func readCycleState(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "run/ci.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestServesAnAssignmentAndNotesWhatItNeeded(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: []look{{true, false}, {true, true}, {false, false}}, task: 8480, peak: 2345}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	if !c.Reset() {
		t.Fatal("reset failed")
	}
	assign(t, dir, nil)
	a, ok := c.WaitAssignment()
	if !ok || exists(filepath.Join(dir, "run/ci.assignment")) {
		t.Fatal("the assignment was not taken")
	}
	if !c.Serve(a) {
		t.Fatal("serve failed")
	}
	if want := []string{"reset", "apply trusted ubuntu-latest:host", "start", "journal", "measure"}; !slices.Equal(sys.calls, want) {
		t.Fatalf("calls %v, want %v", sys.calls, want)
	}
	st := readCycleState(t, dir)
	last := st["last"].(map[string]any)
	if st["assignment"] != "" || last["assignment"] != "ci-1-ci-1-7" || last["task"] != 8480.0 ||
		last["peak_mib"] != 2345.0 || last["unused"] != nil {
		t.Fatalf("state %v", st)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "run/ci.json"))
	if strings.Contains(string(data), testToken) {
		t.Fatal("the token is in the state")
	}
	metrics, _ := os.ReadFile(filepath.Join(dir, "textfile/forgejo_berth_cycle_ci.prom"))
	if !strings.Contains(string(metrics), "forgejo_berth_cycle_jobs_total{container=\"ci\"} 1") {
		t.Fatalf("metrics:\n%s", metrics)
	}
}

func TestHoldsAnAssignmentBeforeSettingItUp(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock()}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	assign(t, dir, nil)
	if _, ok := c.WaitAssignment(); !ok {
		t.Fatal("no assignment")
	}
	if st := readCycleState(t, dir); st["state"] != "assigned" || st["assignment"] != "ci-1-ci-1-7" {
		t.Fatalf("state %v", st)
	}
}

func TestDropsALeftoverAssignmentAtTheReset(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock()}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	assign(t, dir, nil)
	if !c.Reset() || exists(filepath.Join(dir, "run/ci.assignment")) {
		t.Fatal("the leftover assignment is still there")
	}
}

func TestKeepsARunnerThatTookItsTask(t *testing.T) {
	// A release that comes while the job pulls its image (no step yet) does not stop it.
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: append(repeat(look{true, false}, 30), look{}), took: true}
	c := newCycle(t, dir, sys, sys.clock.stop(1000))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	write(t, filepath.Join(dir, "run/ci.release"), "ci-1-ci-1-7")
	if !c.Serve(a) || slices.Contains(sys.calls, "stop") || c.Last().Unused {
		t.Fatalf("calls %v, last %+v", sys.calls, c.Last())
	}
}

func TestDropsAnAssignmentItsClassDoesNotAllow(t *testing.T) {
	// A shared runner handed a trusted label is dropped; the container stays ready.
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock()}
	c := newCycle(t, dir, sys, sys.clock.stop(3))
	assign(t, dir, map[string]any{"class": "shared"})
	if _, ok := c.WaitAssignment(); ok {
		t.Fatal("took a shared runner with a trusted label")
	}
	if exists(filepath.Join(dir, "run/ci.assignment")) || readCycleState(t, dir)["state"] != "ready" {
		t.Fatal("not dropped, or not ready")
	}
}

func TestStopsAnUnusedRunner(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: repeat(look{true, false}, 400)}
	c := newCycle(t, dir, sys, sys.clock.stop(1000))
	assign(t, dir, map[string]any{"unused_minutes": 2})
	a, _ := c.WaitAssignment()
	c.Serve(a)
	if !slices.Contains(sys.calls, "stop") || slices.Contains(sys.calls, "measure") || !c.Last().Unused ||
		c.Last().Assignment != "ci-1-ci-1-7" {
		t.Fatalf("calls %v, last %+v", sys.calls, c.Last())
	}
}

func TestStopsAReleasedRunner(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: repeat(look{true, false}, 400)}
	c := newCycle(t, dir, sys, sys.clock.stop(1000))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	write(t, filepath.Join(dir, "run/ci.release"), "ci-1-ci-1-7")
	c.Serve(a)
	if !slices.Contains(sys.calls, "stop") || !c.Last().Unused {
		t.Fatalf("calls %v", sys.calls)
	}
}

func TestReportsARunnerItCouldNotSetUpAsUnused(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), applyFails: 1}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	if c.Serve(a) || c.State() != "failed" || !c.Last().Unused || slices.Contains(sys.calls, "start") {
		t.Fatalf("state %s, last %+v, calls %v", c.State(), c.Last(), sys.calls)
	}
}

func TestALongJobIsCut(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: repeat(look{true, true}, 5000)}
	c := newCycle(t, dir, sys, sys.clock.stop(100000))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	start := sys.clock.now
	if !c.Serve(a) || sys.clock.now.Sub(start) > 5*time.Hour {
		t.Fatalf("ran %v", sys.clock.now.Sub(start))
	}
}

func TestRunLoopsAndStops(t *testing.T) {
	// Failures back off; a stop is reported as stopped.
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), resetFails: 2}
	c := newCycle(t, dir, sys, sys.clock.stop(40))
	c.Run()
	if n := strings.Count(strings.Join(sys.calls, ","), "reset"); n < 3 {
		t.Fatalf("resets %d (%v)", n, sys.calls)
	}
	if st := readCycleState(t, dir); st["state"] != "stopped" {
		t.Fatalf("state %v", st)
	}
}

func TestStateSinceIsWhenTheStateWasEntered(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock()}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	c.setState("ready")
	first := readCycleState(t, dir)["state_since"]
	sys.clock.advance(60)
	c.setState("ready")
	if readCycleState(t, dir)["state_since"] != first {
		t.Fatal("state_since moved within one state")
	}
}

func TestAnAssignmentOutOfShapeIsRefused(t *testing.T) {
	s := &Settings{Container: "ci", LabelTables: map[string]map[string]string{
		"trusted": {"docker": "docker:docker://x"}, "shared": {"docker": "docker:docker://x"}}}
	for _, change := range []map[string]any{
		{"token": "not-hex"}, {"uuid": "../../etc/passwd"}, {"assignment": "Ci; rm -rf /"},
		{"labels": []string{}}, {"labels": []string{"macos-latest"}}, {"class": "root"}, {"unused_minutes": 0},
	} {
		data := map[string]any{"assignment": "ci-1-ci-1-7", "uuid": testUUID, "token": testToken,
			"labels": []string{"docker"}, "class": "trusted", "unused_minutes": 15}
		maps.Copy(data, change)
		text, _ := json.Marshal(data)
		if _, err := provider.ParseHandoff(text, s); err == nil {
			t.Errorf("took %v", change)
		}
	}
	shared := `{"assignment": "a-1", "uuid": "` + testUUID + `", "token": "` + testToken +
		`", "labels": ["docker"], "class": "shared"}`
	p, err := provider.ParseHandoff([]byte(shared), s)
	if err != nil || !slices.Equal(p.Labels, []string{"docker:docker://x"}) || p.UnusedMinutes != 15 {
		t.Fatalf("parsed %+v, %v", p, err)
	}
}

// realJournal is forgejo-runner 13.2.2's lines when it takes a task, as a real runner wrote
// them, and a job's line that only looks like one.
const realJournal = `time="2026-11-20T20:43:22Z" level=info msg="single task poller successfully fetched one task from http://forgejo:3000"
time="2026-11-20T20:43:22Z" level=info msg="task 17 repo is alice/app https://data.forgejo.org http://forgejo:3000"
time="2026-11-20T20:43:30Z" level=info msg="task 999 repo is alice/app x y"
`

func TestTheTaskLine(t *testing.T) {
	for _, tc := range []struct {
		name, journal string
		task          int64
	}{
		{"text", realJournal, 17},
		{"json", `{"level":"info","msg":"task 23 repo is alice/app https://data.forgejo.org x","time":"t"}` + "\n", 23},
		{"bare", "received task 5\ntask 6 repo is alice/app\n", 0},
		{"inside", `level=info msg="echo task 7 repo is alice/app"` + "\n", 0},
		{"none", `level=info msg="single task poller launched"` + "\n", 0},
	} {
		if got := ParseTask(tc.journal); got != tc.task {
			t.Errorf("%s: task %d, want %d", tc.name, got, tc.task)
		}
	}
}

func TestPeakMiBAddsSwapAndRoundsUp(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "memory.peak"), "1048577\n")
	write(t, filepath.Join(dir, "memory.swap.peak"), "1048576\n")
	if got := PeakMiB(dir); got != 3 {
		t.Fatalf("PeakMiB = %d, want 3", got)
	}
}

func TestIncusReadsTheRunnerFromTheCgroups(t *testing.T) {
	root := t.TempDir()
	s := &Settings{Container: "ci", CgroupRoot: root}
	s.defaults()
	i := NewIncus(s)
	svc := filepath.Join(root, "lxc.payload.ci/system.slice/forgejo-runner.service")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(svc, "cgroup.procs"), "")
	if i.RunnerActive() {
		t.Fatal("active without processes")
	}
	write(t, filepath.Join(svc, "cgroup.procs"), "999999999\n")
	if !i.RunnerActive() || i.Busy() {
		t.Fatal("the runner alone is active, not busy")
	}
	write(t, filepath.Join(svc, "cgroup.procs"), "999999999\n999999998\n")
	if !i.Busy() {
		t.Fatal("a step is not a job")
	}
	write(t, filepath.Join(svc, "cgroup.procs"), "999999999\n")
	scope := filepath.Join(root, "lxc.payload.ci/system.slice/docker-abc.scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(scope, "cgroup.events"), "populated 1\nfrozen 0\n")
	if !i.Busy() {
		t.Fatal("a job's Docker container is not a job")
	}
}

func TestTheJobsCPUIsNoted(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: []look{{true, false}, {true, true}, {true, true}, {false, false}}}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	c.Serve(a)
	if last := c.Last(); last.CPUCores != 1 || last.CPUPressure != 0.1 {
		t.Fatalf("last %+v", last)
	}
}

func TestCPUCounters(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := CPUCounters(dir); ok {
		t.Fatal("counters without cpu.stat")
	}
	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec 5000000\nuser_usec 4000000\nsystem_usec 1000000\n")
	if u, p, ok := CPUCounters(dir); !ok || u != 5000000 || p != 0 {
		t.Fatalf("%d %d %v", u, p, ok)
	}
	write(t, filepath.Join(dir, "cpu.pressure"), "some avg10=1.00 avg60=0.50 avg300=0.10 total=250000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1000\n")
	if _, p, _ := CPUCounters(dir); p != 250000 {
		t.Fatalf("pressure %d", p)
	}
}

// scripted is an incus stand-in: each command's answer by its first words.
func scripted(answers map[string]string, calls *[]string) func(context.Context, []byte, ...string) ([]byte, []byte, error) {
	return func(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
		line := strings.Join(args, " ")
		*calls = append(*calls, line)
		for prefix, answer := range answers {
			if strings.HasPrefix(line, prefix) {
				if strings.HasPrefix(answer, "ERR:") {
					return nil, []byte(strings.TrimPrefix(answer, "ERR:")), errors.New("exit status 1")
				}
				return []byte(answer), nil, nil
			}
		}
		return nil, nil, nil
	}
}

func TestAFailedBootSaysWhy(t *testing.T) {
	s := &Settings{Container: "ci", BootTimeout: 1}
	s.defaults()
	s.BootTimeout = 1
	var calls []string
	i := NewIncus(s)
	i.instance.(*IncusCommand).run = scripted(map[string]string{
		"query /1.0/instances/ci/state": `{"status": "Running"}`,
		"exec ci -- timeout":            "maintenance\n",
		"exec ci -- systemctl --failed": "docker.service loaded failed failed Docker\nx.mount loaded failed failed X\n",
		"info ci --show-log":            "Name: ci\nLog:\n\nlxc ci: oops\nlast line\n",
	}, &calls)
	err := i.Reset()
	if err == nil {
		t.Fatal("the boot did not fail")
	}
	for _, want := range []string{`"maintenance"`, "failed units: docker.service, x.mount", "its log ends:\n  lxc ci: oops\n  last line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}
	if !slices.Contains(calls, "snapshot restore ci clean") {
		t.Fatalf("calls %v", calls)
	}
}

func TestApplyWritesTheConfigurationAndToken(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config.yml"), "uuid: @@RUNNER_UUID@@\nlabels: @@RUNNER_LABELS@@\n")
	s := &Settings{Container: "ci", ConfigTemplate: filepath.Join(dir, "config.yml"),
		CacheVolumes: map[string]string{"shared": "cache-shared"}}
	s.defaults()
	var calls []string
	var stdins []string
	i := NewIncus(s)
	i.instance.(*IncusCommand).run = func(_ context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
		calls = append(calls, strings.Join(args, " "))
		stdins = append(stdins, string(stdin))
		return nil, nil, nil
	}
	err := i.Apply(provider.Parsed{ID: "a-1", UUID: testUUID, Token: testToken, Class: "shared",
		Labels: []string{"docker:docker://node:24-trixie"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "config device add ci cache disk pool=default source=cache-shared path=/var/cache/forgejo-runner") {
		t.Fatalf("calls:\n%s", joined)
	}
	if !slices.Contains(stdins, "uuid: "+testUUID+"\nlabels: [\"docker:docker://node:24-trixie\"]\n") ||
		!slices.Contains(stdins, testToken) || strings.Contains(joined, testToken) {
		t.Fatalf("stdins %q", stdins)
	}
}

// A job short enough to start and end between two looks is still the runner's job: its
// runner took a task.
func TestAJobBetweenTwoLooksIsAJob(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystem{clock: newClock(), looks: []look{{true, false}, {false, false}}, took: true, task: 9, peak: 64}
	c := newCycle(t, dir, sys, sys.clock.stop(100))
	assign(t, dir, nil)
	a, _ := c.WaitAssignment()
	if !c.Serve(a) {
		t.Fatal("serve failed")
	}
	if last := c.Last(); last.Unused || last.Task != 9 || last.PeakMiB != 64 || last.CPUCores != 0 {
		t.Fatalf("last %+v", last)
	}
}

// A cache volume that is not there is made in the pool before it is attached; one that is
// there is attached as it is.
func TestACacheVolumeIsMadeAtItsFirstUse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer map[string]string
		create bool
	}{
		{"missing", map[string]string{"storage volume show": "ERR:Error: Storage pool volume not found"}, true},
		{"there", map[string]string{"storage volume show": "name: cache-ci\n"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Settings{Container: "ci", ConfigTemplate: "/dev/null",
				CacheVolumes: map[string]string{"shared": "cache-ci"}}
			s.defaults()
			var calls []string
			i := NewIncus(s)
			i.instance.(*IncusCommand).run = scripted(tc.answer, &calls)
			if err := i.Apply(provider.Parsed{ID: "a-1", UUID: testUUID, Token: testToken, Class: "shared",
				Labels: []string{"docker:docker://node:24-trixie"}}); err != nil {
				t.Fatal(err)
			}
			created := slices.Index(calls, "storage volume create default cache-ci")
			attached := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "config device add ci cache") })
			if (created >= 0) != tc.create || attached < 0 || (tc.create && created > attached) {
				t.Fatalf("calls %v", calls)
			}
		})
	}
}

func TestASettingsFilesCacheOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "ci-a.yml"), "container: ci-a\nconfig_template: /dev/null\n"+
		"labels: {shared: {docker: d}}\ncache_volumes: {shared: \"cache-{host}-{slot}\", trusted: plain}\n")
	s, err := LoadSettings(filepath.Join(dir, "ci-a.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "cache-" + shortHostname() + "-ci-a"; s.CacheVolumes["shared"] != want || s.CacheVolumes["trusted"] != "plain" {
		t.Fatalf("volumes %v, want %s", s.CacheVolumes, want)
	}
	write(t, filepath.Join(dir, "ci-b.yml"), "container: ci-b\nconfig_template: /dev/null\n"+
		"labels: {shared: {docker: d}}\ncache_volumes: {shared: \"cache-{class}\"}\n")
	if _, err := LoadSettings(filepath.Join(dir, "ci-b.yml")); err == nil || !strings.Contains(err.Error(), "placeholders") {
		t.Fatalf("an unknown placeholder: %v", err)
	}
}
