// SPDX-License-Identifier: GPL-3.0-or-later

//go:build realforgejo

// Package real runs the broker against a real Forgejo and forgejo-runner, with slots that
// are Docker containers: each assignment starts a runner container for one job (host mode
// inside it), and the slot reports its task (the runner's log), its memory peak and CPU use
// (the container's cgroup). scripts/real-forgejo.sh sets the instance up and runs this:
//
//	go test -tags realforgejo -run TestRealForgejo -v ./e2e/real/
package real

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
)

// syncBuffer is a log buffer the cycles' goroutines write to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set: run scripts/real-forgejo.sh", name)
	}
	return v
}

// runnerLog is the runner container's log: the runner writes to its standard error.
func runnerLog(container string) string {
	out, _ := exec.Command("docker", "logs", container).CombinedOutput()
	return string(out)
}

func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// labels maps a class's label names to the runner's labels: host mode, as the runner
// container has no Docker of its own.
var labels = map[string]map[string]string{
	"trusted": {"ubuntu-latest": "ubuntu-latest:host", "docker": "docker:host"},
	"shared":  {"docker": "docker:host"},
}

type classes struct{}

func (classes) Labels(class string) (map[string]string, bool) {
	l, ok := labels[class]
	return l, ok
}

// dockerSlots are slots whose runner is a container of its own per job.
type dockerSlots struct {
	t        *testing.T
	mu       sync.Mutex
	image    string
	network  string
	url      string
	dir      string
	names    []string
	assigned map[string]string // container -> assignment
	last     map[string]provider.Slot
}

func (d *dockerSlots) container(name string) string { return "berth-slot-" + name }

func (d *dockerSlots) Status(_ context.Context, h config.Host) ([]provider.Slot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []provider.Slot
	for _, name := range d.names {
		s := d.last[name]
		s.Host, s.Container, s.State, s.Assignment = h.Name, name, provider.Ready, ""
		if id := d.assigned[name]; id != "" {
			c := d.container(name)
			task := slot.ParseTask(runnerLog(c))
			if _, err := docker("exec", c, "test", "-f", "/tmp/done"); err == nil {
				s = d.finish(name, id, task)
			} else if task != 0 {
				s.State, s.Assignment = provider.Running, id
			} else {
				s.State, s.Assignment = provider.Assigned, id
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// finish reads the job's figures from the container's cgroup, removes it and keeps them.
func (d *dockerSlots) finish(name, id string, task int64) provider.Slot {
	c := d.container(name)
	read := func(file string) string {
		out, _ := docker("exec", c, "cat", "/sys/fs/cgroup/"+file)
		return out
	}
	var peak int64
	for _, f := range []string{"memory.peak", "memory.swap.peak"} {
		n, _ := strconv.ParseInt(strings.TrimSpace(read(f)), 10, 64)
		peak += n
	}
	started, _ := docker("exec", c, "cat", "/tmp/started")
	startedAt, _ := strconv.ParseInt(strings.TrimSpace(started), 10, 64)
	seconds := float64(time.Now().Unix() - startedAt)
	tmp := filepath.Join(d.dir, name+".cgroup")
	_ = os.MkdirAll(tmp, 0o700)
	_ = os.WriteFile(filepath.Join(tmp, "cpu.stat"), []byte(read("cpu.stat")), 0o600)
	_ = os.WriteFile(filepath.Join(tmp, "cpu.pressure"), []byte(read("cpu.pressure")), 0o600)
	usage, pressure, ok := slot.CPUCounters(tmp)
	d.t.Logf("slot %s finished %s, task %d; runner log:\n%s", name, id, task, runnerLog(c))
	_, _ = docker("rm", "-f", c)
	s := provider.Slot{Host: "ci-1", Container: name, State: provider.Ready, Finished: id, Task: task,
		PeakMiB: int((peak + 1<<20 - 1) >> 20), Seconds: seconds, Unused: task == 0}
	if ok && seconds > 0 {
		s.CPUCores = float64(usage) / 1e6 / seconds
		s.CPUPressure = min(1, float64(pressure)/1e6/seconds)
	}
	delete(d.assigned, name)
	d.last[name] = s
	return s
}

// Assign checks the handoff as a slot does and starts a runner container for one job; the
// token goes in through a file that is removed at once.
func (d *dockerSlots) Assign(_ context.Context, h config.Host, name string, ho provider.Handoff) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.assigned[name] != "" {
		return fmt.Errorf("%s is not ready", name)
	}
	data, _ := json.Marshal(ho)
	parsed, err := provider.ParseHandoff(data, classes{})
	if err != nil {
		return err
	}
	c := d.container(name)
	args := []string{"create", "--name", c, "--network", d.network, "--memory", "1g",
		"--entrypoint", "sh", d.image, "-c",
		// $2 is the label flags, split into words on purpose (labels have no spaces).
		`date +%s > /tmp/started; forgejo-runner one-job --wait --url "$0" --uuid "$1" --token-url file:///tmp/token $2; touch /tmp/done; sleep 3600`,
		d.url, parsed.UUID, labelFlags(parsed.Labels)}
	if _, err := docker(args...); err != nil {
		return err
	}
	token := filepath.Join(d.dir, name+".token")
	if err := os.WriteFile(token, []byte(parsed.Token), 0o644); err != nil {
		return err
	}
	_, err = docker("cp", token, c+":/tmp/token")
	_ = os.Remove(token)
	if err == nil {
		_, err = docker("start", c)
	}
	if err != nil {
		_, _ = docker("rm", "-f", c)
		return err
	}
	d.assigned[name] = parsed.ID
	return nil
}

func labelFlags(labels []string) string {
	var flags []string
	for _, l := range labels {
		flags = append(flags, "--label="+l)
	}
	return strings.Join(flags, " ")
}

// Release stops a runner that has no job yet.
func (d *dockerSlots) Release(_ context.Context, _ config.Host, name, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.assigned[name] != id {
		return fmt.Errorf("%s does not hold %s", name, id)
	}
	if slot.ParseTask(runnerLog(d.container(name))) == 0 {
		_, _ = docker("exec", d.container(name), "touch", "/tmp/done")
	}
	return nil
}

// api calls the instance as the token's owner.
func api(t *testing.T, base, token, method, path string, body any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, base+"/api/v1"+path, bytes.NewReader(data))
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, out)
	}
	return out
}

func push(t *testing.T, base, token, repo, path, content string) {
	t.Helper()
	api(t, base, token, "POST", "/repos/"+repo+"/contents/"+path, map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte(content)), "message": "ci: " + path,
	})
}

// The workflows: a build that holds about 300 MiB, a matrix on docker, and another owner's
// job (a global runner).
const appWorkflow = `on: [push]
defaults:
  run:
    shell: sh
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: head -c 300000000 /dev/zero | tail > /dev/null; i=0; while [ $i -lt 2000000 ]; do i=$((i+1)); done
  test:
    runs-on: docker
    strategy:
      matrix:
        n: [1, 2]
    steps:
      - run: echo test ${{ matrix.n }}
`

const siteWorkflow = `on: [push]
defaults:
  run:
    shell: sh
jobs:
  lint:
    runs-on: docker
    steps:
      - run: echo lint
`

func TestRealForgejo(t *testing.T) {
	inner := env(t, "BERTH_REAL_INNER_URL")
	dir := t.TempDir()
	slots := &dockerSlots{t: t, image: env(t, "BERTH_REAL_RUNNER_IMAGE"), network: env(t, "BERTH_REAL_NETWORK"),
		url: inner, dir: dir, names: []string{"a", "b", "c"}, assigned: map[string]string{},
		last: map[string]provider.Slot{}}
	defer func() {
		for _, n := range slots.names {
			_, _ = docker("rm", "-f", slots.container(n))
		}
	}()
	scenario(t, "hosts: [{name: ci-1, address: localhost, memory_mib: 4000}]\n",
		func(*config.Config, *slog.Logger) provider.Provider { return slots })
}

// scenario runs two owners' workflows through the broker with the provider: the broker
// learns their peaks and CPU use, and the next build reserves its own peak with the margin.
func scenario(t *testing.T, hosts string, slots func(*config.Config, *slog.Logger) provider.Provider) {
	base := env(t, "BERTH_REAL_URL")
	adminFile := env(t, "BERTH_REAL_ADMIN_TOKEN_FILE")
	aliceFile := env(t, "BERTH_REAL_ALICE_TOKEN_FILE")
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
forgejo: {url: %s, admin_token_file: %s}
trusted: {owners: {alice: %s}, labels: [ubuntu-latest, docker]}
shared: {labels: [docker]}
estimate: {unknown_mib: 1500}
interval_seconds: 2
history: {path: %s}
`, base, adminFile, aliceFile, filepath.Join(dir, "history.sqlite")) + hosts))
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := config.ReadSecret(adminFile)
	alice, _ := config.ReadSecret(aliceFile)
	hist, err := history.Open(cfg.History.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	var logs syncBuffer
	var out io.Writer = &logs
	if os.Getenv("BERTH_REAL_VERBOSE") != "" {
		out = io.MultiWriter(&logs, os.Stderr)
	}
	log := slog.New(slog.NewTextHandler(out, nil))
	b, err := broker.New(cfg, broker.Options{
		Forge:    forgejo.New(base, admin, map[string]string{"alice": alice}, nil),
		Provider: slots(cfg, log), History: hist, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := func(round string, learned int) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Minute)
		for time.Now().Before(deadline) {
			if err := b.Tick(ctx); err != nil {
				t.Logf("tick: %v", err)
			}
			// Every job is learned: a global runner that takes an older job of a trusted owner
			// has it among its candidates.
			c := b.Counts()
			if c["learned"] >= learned &&
				len(b.Assignments()) == 0 && b.Snapshot().Waiting == 0 {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("%s: not done: counts %v, state %+v\nlog:\n%s", round, b.Counts(), b.Snapshot(), logs.String())
	}

	push(t, base, alice, "alice/app", ".forgejo/workflows/ci.yml", appWorkflow)
	push(t, base, admin, "carol/site", ".forgejo/workflows/ci.yml", siteWorkflow)
	run("first round", 4)
	peak, source, found, err := hist.Estimate("alice/app", "build", "build", 0, 5)
	if err != nil || !found || source != "job" || peak < 250 {
		t.Fatalf("the build's estimate: %d %s %v %v\nlog:\n%s", peak, source, found, err, logs.String())
	}
	cores, _, found, _ := hist.EstimateCores("alice/app", "build", "build", 0, 5)
	t.Logf("learned: build %d MiB, %.2f cores (found %v)", peak, cores, found)

	// Again: the build now reserves its own peak with the margin.
	push(t, base, alice, "alice/app", "again.txt", "again\n")
	var reserved []string
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) && len(reserved) == 0 {
		if err := b.Tick(ctx); err != nil {
			t.Logf("tick: %v", err)
		}
		for _, a := range b.Assignments() {
			for _, c := range a.Candidates {
				if c.Name == "build" {
					reserved = append(reserved, fmt.Sprint(a.ReserveMiB))
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	want := fmt.Sprint(max(512, int(float64(peak)*1.2+0.5)))
	if len(reserved) == 0 || reserved[0] != want {
		t.Fatalf("the second build reserved %v, want %s\nlog:\n%s", reserved, want, logs.String())
	}
	run("second round", 7)
	if err := b.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("counts %v\nlog:\n%s", b.Counts(), logs.String())
	for _, secret := range []string{admin, alice} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("a token is in the log")
		}
	}
}
