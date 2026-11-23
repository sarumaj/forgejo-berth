// SPDX-License-Identifier: GPL-3.0-or-later

package docker

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

const (
	testUUID  = "0f0e0d0c-0b0a-4908-8706-050403020100"
	testToken = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// container is a container of the fake engine.
type container struct {
	spec    Spec
	running bool
	exited  bool
	logs    string
	files   map[string]string
	cgroup  string
}

// fakeEngine serves the Engine API's shapes for the calls the provider makes.
type fakeEngine struct {
	mu         sync.Mutex
	containers map[string]*container
	volumes    map[string]bool
	// volumeLabels are each volume's labels, as it was made.
	volumeLabels map[string]map[string]string
	images       map[string]bool
	calls        []string
	execs        map[string][]string
	pullError    string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{containers: map[string]*container{}, volumes: map[string]bool{},
		volumeLabels: map[string]map[string]string{},
		images:       map[string]bool{}, execs: map[string][]string{}}
}

// frame is one frame of a multiplexed stream.
func frame(stream byte, text string) []byte {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
	return append(header, text...)
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "No such object"}`))
	}
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case parts[0] == "images" && r.Method == http.MethodGet:
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
		if !f.images[name] {
			notFound()
			return
		}
		reply(map[string]any{"Config": map[string]any{"User": "1000:1000"}})
	case r.URL.Path == "/images/create":
		ref := r.URL.Query().Get("fromImage")
		if f.pullError != "" {
			_, _ = w.Write([]byte(`{"status": "Pulling"}` + "\n" + `{"error": "` + f.pullError + `"}` + "\n"))
			return
		}
		f.images[ref] = true
		_, _ = w.Write([]byte(`{"status": "Downloaded"}` + "\n"))
	case r.URL.Path == "/containers/create":
		name := r.URL.Query().Get("name")
		if f.containers[name] != nil {
			w.WriteHeader(http.StatusConflict)
			reply(map[string]string{"message": "name in use"})
			return
		}
		c := &container{files: map[string]string{}}
		_ = json.NewDecoder(r.Body).Decode(&c.spec)
		f.containers[name] = c
		w.WriteHeader(http.StatusCreated)
		reply(map[string]string{"Id": name})
	case parts[0] == "containers" && len(parts) >= 2:
		c := f.containers[parts[1]]
		if c == nil {
			notFound()
			return
		}
		action := ""
		if len(parts) > 2 {
			action = parts[2]
		}
		switch {
		case r.Method == http.MethodDelete:
			delete(f.containers, parts[1])
			w.WriteHeader(http.StatusNoContent)
		case action == "start":
			c.running = true
			w.WriteHeader(http.StatusNoContent)
		case action == "kill":
			c.exited = true
			w.WriteHeader(http.StatusNoContent)
		case action == "json":
			reply(map[string]any{"State": map[string]any{"Running": c.running}})
		case action == "archive" && r.Method == http.MethodHead:
			if r.URL.Query().Get("path") == exitFile && c.exited {
				return
			}
			notFound()
		case action == "archive" && r.Method == http.MethodPut:
			tr := tar.NewReader(r.Body)
			for {
				h, err := tr.Next()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(tr)
				c.files[h.Name] = string(data)
				f.calls = append(f.calls, fmt.Sprintf("file %s %d %o", h.Name, h.Uid, h.Mode))
			}
			w.WriteHeader(http.StatusOK)
		case action == "logs":
			_, _ = w.Write(frame(2, c.logs))
		case action == "exec":
			var body struct {
				Cmd []string `json:"Cmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := "exec" + string(rune('a'+len(f.execs)))
			f.execs[id] = append([]string{parts[1]}, body.Cmd...)
			w.WriteHeader(http.StatusCreated)
			reply(map[string]string{"Id": id})
		case action == "stats":
			reply(map[string]any{"memory_stats": map[string]any{"max_usage": 0}})
		default:
			notFound()
		}
	case parts[0] == "exec" && len(parts) == 3:
		cmd := f.execs[parts[1]]
		if cmd == nil {
			notFound()
			return
		}
		if parts[2] == "json" {
			reply(map[string]any{"ExitCode": 0, "Running": false})
			return
		}
		c := f.containers[cmd[0]]
		out := ""
		if strings.Contains(strings.Join(cmd, " "), "/sys/fs/cgroup") {
			out = c.cgroup
		}
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		_, _ = w.Write(append(frame(1, out), frame(2, "a warning\n")...))
	case r.URL.Path == "/volumes/create":
		var body struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.volumes[body.Name] = true
		f.volumeLabels[body.Name] = body.Labels
		w.WriteHeader(http.StatusCreated)
		reply(map[string]string{"Name": body.Name})
	case parts[0] == "volumes" && len(parts) == 2 && r.Method == http.MethodGet:
		if !f.volumes[parts[1]] {
			notFound()
			return
		}
		reply(map[string]any{"Name": parts[1], "Labels": f.volumeLabels[parts[1]]})
	case parts[0] == "volumes" && r.Method == http.MethodDelete:
		if !f.volumes[parts[1]] {
			notFound()
			return
		}
		delete(f.volumes, parts[1])
		w.WriteHeader(http.StatusNoContent)
	default:
		notFound()
	}
}

// serve starts the fake engine on a unix socket and returns its address.
func serve(t *testing.T, f *fakeEngine) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(f)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return "unix://" + path
}

func testConfig(engine string) config.Docker {
	cfg, err := config.Parse([]byte(`
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
hosts: [{name: ci-1, address: "` + engine + `", memory_mib: 8000, slots: [a]}]
provider:
  type: docker
  docker:
    labels:
      trusted: {ubuntu-latest: "ubuntu-latest:docker://node:24"}
      shared: {docker: "docker:docker://node:24"}
    cache_volumes: {shared: "cache-{host}-{slot}"}
    dind_args: [--registry-mirror=https://mirror.example.org]
`))
	if err != nil {
		panic(err)
	}
	return cfg.Provider.Docker
}

func newTestSystem(t *testing.T, f *fakeEngine) *System {
	t.Helper()
	address := serve(t, f)
	engine, err := NewEngine(address, config.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(address)
	cfg.BootTimeout = 1
	s := Settings(cfg.Slots, "ci-1", "a")
	return NewSystem(engine, cfg, s, "ci-1", "berth-ci-1-a", slog.New(slog.DiscardHandler))
}

func TestASlotsLifeOnTheEngine(t *testing.T) {
	f := newFakeEngine()
	d := newTestSystem(t, f)
	if err := d.Reset(); err != nil {
		t.Fatal(err)
	}
	if !f.images["data.forgejo.org/forgejo/runner:13"] || !f.images["docker.io/library/docker:29-dind"] {
		t.Fatalf("images %v", f.images)
	}
	a := provider.Parsed{ID: "ci-1-a-7-42", UUID: testUUID, Token: testToken, Class: "shared",
		Labels: []string{"docker:docker://node:24"}, UnusedMinutes: 15}
	if err := d.Apply(a); err != nil {
		t.Fatal(err)
	}
	dind := f.containers["berth-ci-1-a-docker"]
	runner := f.containers["berth-ci-1-a"]
	if dind == nil || runner == nil || !f.volumes["berth-ci-1-a-run"] {
		t.Fatalf("containers %v volumes %v", f.containers, f.volumes)
	}
	if !dind.spec.HostConfig.Privileged || !dind.running ||
		!slices.Contains(dind.spec.Cmd, "--registry-mirror=https://mirror.example.org") {
		t.Fatalf("the slot's Docker %+v", dind)
	}
	if runner.spec.HostConfig.Privileged || runner.running || !runner.spec.HostConfig.Init ||
		!slices.Contains(runner.spec.Env, "DOCKER_HOST=unix:///run/forgejo-berth/docker.sock") {
		t.Fatalf("the runner %+v", runner.spec)
	}
	for _, m := range runner.spec.HostConfig.Mounts {
		if m.Type == "bind" {
			t.Fatalf("the runner has a bind mount %+v", m)
		}
	}
	cmd := strings.Join(runner.spec.Cmd, " ")
	if !strings.Contains(cmd, "one-job --wait --url https://forgejo.example.org --uuid "+testUUID) ||
		!strings.Contains(cmd, "--label docker:docker://node:24") || strings.Contains(cmd, testToken) {
		t.Fatalf("the runner's command %q", cmd)
	}
	if runner.files["etc/forgejo-berth/token"] != testToken || runner.spec.Labels[LabelAssignment] != a.ID {
		t.Fatalf("files %v labels %v", runner.files, runner.spec.Labels)
	}
	if !slices.Contains(f.calls, "file etc/forgejo-berth/token 1000 400") ||
		!slices.Contains(f.calls, "file var/cache/forgejo-runner/ 1000 700") {
		t.Fatalf("the token is not the runner's alone: %v", f.calls)
	}
	if err := d.StartRunner(); err != nil || !d.RunnerActive() {
		t.Fatal("the runner does not run")
	}
	if d.Busy() || d.TookTask() {
		t.Fatal("busy without a task line")
	}
	runner.logs = `time="x" level=info msg="task 8480 repo is alice/app https://data.forgejo.org https://forgejo.example.org"` + "\n"
	if !d.Busy() {
		t.Fatal("the task line is not a job")
	}
	runner.cgroup = "@memory.peak\n1048576\n@memory.swap.peak\n0\n@cpu.stat\nusage_usec 2000000\n@cpu.pressure\nsome avg10=0 total=1000\n"
	dind.cgroup = "@memory.peak\n3145729\n@memory.swap.peak\n1048576\n@cpu.stat\nusage_usec 1000000\n@cpu.pressure\n"
	if u, p, ok := d.CPU(); !ok || u != 3000000 || p != 1000 {
		t.Fatalf("CPU %d %d %v", u, p, ok)
	}
	d.StopRunner()
	if d.RunnerActive() {
		t.Fatal("active after its exit")
	}
	if task, peak := d.Measure(); task != 8480 || peak != 6 {
		t.Fatalf("measured %d %d", task, peak)
	}
	// The slot's cache volume is its own, labelled, and outlives the reset; nothing else does.
	if err := d.Reset(); err != nil || len(f.containers) != 0 || len(f.volumes) != 1 || !f.volumes["cache-ci-1-a"] {
		t.Fatalf("after the reset %v %v (%v)", f.containers, f.volumes, err)
	}
	if l := f.volumeLabels["cache-ci-1-a"]; l[LabelHost] != "ci-1" || l[LabelCache] != "shared" {
		t.Fatalf("the cache volume's labels %v", l)
	}
	if err := d.Apply(a); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(f.calls, ","), "POST /volumes/create"); n != 3 {
		t.Fatalf("volumes made %d times (the run volume twice, the cache once): %v", n, f.calls)
	}
	var mounted bool
	for _, m := range f.containers["berth-ci-1-a"].spec.HostConfig.Mounts {
		mounted = mounted || m.Source == "cache-ci-1-a" && m.Target == "/var/cache/forgejo-runner"
	}
	if !mounted {
		t.Fatalf("the cache is not mounted: %+v", f.containers["berth-ci-1-a"].spec.HostConfig.Mounts)
	}
}

func TestEngineModes(t *testing.T) {
	for _, mode := range []string{config.EngineNone, config.EngineHostSocket} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeEngine()
			d := newTestSystem(t, f)
			d.cfg.JobEngine = mode
			if err := d.Reset(); err != nil {
				t.Fatal(err)
			}
			a := provider.Parsed{ID: "ci-1-a-7-42", UUID: testUUID, Token: testToken, Class: "trusted",
				Labels: []string{"ubuntu-latest:host"}, UnusedMinutes: 15}
			if err := d.Apply(a); err != nil {
				t.Fatal(err)
			}
			if f.containers["berth-ci-1-a-docker"] != nil || f.images["docker.io/library/docker:29-dind"] {
				t.Fatal("a Docker of the slot's own without dind")
			}
			mounts := f.containers["berth-ci-1-a"].spec.HostConfig.Mounts
			bound := slices.ContainsFunc(mounts, func(m Mount) bool {
				return m.Type == "bind" && m.Source == "/var/run/docker.sock"
			})
			if bound != (mode == config.EngineHostSocket) {
				t.Fatalf("mounts %+v", mounts)
			}
		})
	}
}

func TestAFailedPullFailsTheReset(t *testing.T) {
	f := newFakeEngine()
	f.pullError = "manifest unknown"
	d := newTestSystem(t, f)
	if err := d.Reset(); err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("reset: %v", err)
	}
}

func TestEngineErrors(t *testing.T) {
	f := newFakeEngine()
	engine, _ := NewEngine(serve(t, f), config.TLS{})
	ctx := t.Context()
	if err := engine.Start(ctx, "nothing"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "No such object") {
		t.Fatalf("start: %v", err)
	}
	if err := engine.Remove(ctx, "nothing"); err != nil {
		t.Fatalf("remove of nothing: %v", err)
	}
	if up, err := engine.Running(ctx, "nothing"); up || err != nil {
		t.Fatalf("running: %v %v", up, err)
	}
	if _, err := NewEngine("ssh://host", config.TLS{}); err == nil {
		t.Fatal("an ssh engine was taken")
	}
	if _, err := NewEngine("tcp://host:2376", config.TLS{CAFile: "/nonexistent"}); err == nil {
		t.Fatal("a missing CA was taken")
	}
}

func TestDemux(t *testing.T) {
	var out, errOut bytes.Buffer
	stream := append(frame(1, "out\n"), frame(2, "err\n")...)
	if err := demux(bytes.NewReader(stream), &out, &errOut); err != nil || out.String() != "out\n" || errOut.String() != "err\n" {
		t.Fatalf("%q %q %v", out.String(), errOut.String(), err)
	}
	out.Reset()
	if err := demux(strings.NewReader("raw output\n"), &out, &errOut); err != nil || out.String() != "raw output\n" {
		t.Fatalf("raw: %q %v", out.String(), err)
	}
}

func TestOwner(t *testing.T) {
	for user, want := range map[string][3]int64{"1000:1000": {1000, 1000, 0o400}, "1001": {1001, 1001, 0o400},
		"runner": {0, 0, 0o444}, "": {0, 0, 0o444}} {
		uid, gid, mode := owner(user)
		if [3]int64{int64(uid), int64(gid), mode} != want {
			t.Errorf("owner(%q) = %d %d %o", user, uid, gid, mode)
		}
	}
}

func TestTheProviderServesItsHosts(t *testing.T) {
	f := newFakeEngine()
	address := serve(t, f)
	cfg, err := config.Parse([]byte(`
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
hosts: [{name: ci-1, address: "` + address + `", memory_mib: 8000, slots: [a, b]}]
history: {path: ` + filepath.Join(t.TempDir(), "history.sqlite") + `}
shadow: true
provider:
  type: docker
  docker:
    labels: {shared: {docker: "docker:docker://node:24"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer hosted.Stop()
	slots, err := hosted.Status(t.Context(), cfg.Hosts[0])
	if err != nil || len(slots) != 2 || slots[0].Container != "a" || slots[0].State != "unknown" {
		t.Fatalf("slots %+v, %v", slots, err)
	}
	if len(f.containers) != 0 {
		t.Fatal("shadow mode touched the engine")
	}
}

// A host's own label table is its slots': a label it alone maps is taken there only.
func TestAHostsOwnLabels(t *testing.T) {
	f := newFakeEngine()
	address := serve(t, f)
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
trusted: {labels: [ubuntu-latest]}
hosts:
  - {name: ci-1, address: "` + address + `", memory_mib: 8000, slots: [a]}
  - name: ci-2
    address: "` + address + `"
    memory_mib: 8000
    slots: [a]
    labels: {trusted: {ubuntu-latest: "ubuntu-latest:host"}}
history: {path: ` + filepath.Join(dir, "history.sqlite") + `}
shadow: true
provider:
  type: docker
  docker:
    labels: {shared: {docker: "docker:docker://node:24"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer hosted.Stop()
	for _, h := range []string{"ci-1", "ci-2"} {
		if err := os.WriteFile(filepath.Join(dir, "slots", h, "a.json"), []byte(`{"state": "ready"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff := provider.Handoff{Assignment: "ci-2-a-7-42", UUID: testUUID, Token: testToken,
		Labels: []string{"ubuntu-latest"}, Class: "trusted", UnusedMinutes: 15}
	if err := hosted.Assign(t.Context(), cfg.Hosts[1], "a", handoff); err != nil {
		t.Fatalf("ci-2 refused its own label: %v", err)
	}
	handoff.Assignment = "ci-1-a-7-42"
	if err := hosted.Assign(t.Context(), cfg.Hosts[0], "a", handoff); err == nil {
		t.Fatal("ci-1 took a label of ci-2's table")
	}
}
