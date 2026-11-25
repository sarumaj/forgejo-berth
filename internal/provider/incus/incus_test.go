// SPDX-License-Identifier: GPL-3.0-or-later

package incus

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
)

const (
	testUUID  = "0f0e0d0c-0b0a-4908-8706-050403020100"
	testToken = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// fakeIncus serves the shapes of the Incus REST API (doc/rest-api.yaml) for one container
// ci: sync answers, background operations waited for at /1.0/operations/{id}/wait, exec
// with record-output (its logs under /1.0/instances/ci/logs/exec-output/), the file API.
type fakeIncus struct {
	mu      sync.Mutex
	calls   []string
	status  string
	devices map[string]map[string]string
	files   map[string]string
	logs    map[string]string
	ops     map[string]map[string]any
	runner  string
	// stopLooks is how many looks a runner stopped through systemctl is still busy.
	stopLooks int
	cgroup    string
	journal   string
	peak      int64
	etag      string
	badSnap   bool
	trusted   bool
	execCount int
	// volumes are the custom volumes of the pool default; hidden ones are not seen by
	// the next look at them (another slot makes them meanwhile).
	volumes, hidden map[string]bool
}

func newFakeIncus() *fakeIncus {
	return &fakeIncus{status: "Stopped", files: map[string]string{}, logs: map[string]string{},
		ops: map[string]map[string]any{}, runner: "active", trusted: true, etag: `"e1"`,
		volumes: map[string]bool{"cache-shared": true}, hidden: map[string]bool{},
		devices: map[string]map[string]string{
			"root":  {"type": "disk", "pool": "default", "path": "/"},
			"cache": {"type": "disk", "pool": "default", "source": "old", "path": "/var/cache/forgejo-runner"},
		}}
}

func sync200(w http.ResponseWriter, metadata any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status": "Success", "status_code": 200,
		"metadata": metadata})
}

func (f *fakeIncus) async(w http.ResponseWriter, metadata map[string]any, failure string) {
	id := fmt.Sprintf("op-%d", len(f.ops)+1)
	op := map[string]any{"id": id, "class": "task", "status": "Success", "status_code": 200, "err": "",
		"metadata": metadata, "may_cancel": false}
	if failure != "" {
		op["status"], op["status_code"], op["err"] = "Failure", 400, failure
	}
	f.ops[id] = op
	w.Header().Set("Location", "/1.0/operations/"+id)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "async", "status": "Operation created",
		"status_code": 100, "operation": "/1.0/operations/" + id, "metadata": op})
}

func fail(w http.ResponseWriter, code int, message string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": message, "error_code": code,
		"metadata": map[string]any{}})
}

func (f *fakeIncus) exec(command []string) (string, string, int) {
	line := strings.Join(command, " ")
	switch {
	case strings.Contains(line, "is-system-running"):
		return "running\n", "", 0
	case strings.HasSuffix(line, "look forgejo-runner.service /var/lib/forgejo-runner/work"):
		if f.runner == "stopping" {
			if f.stopLooks > 0 {
				f.stopLooks--
				return "busy\n", "", 0
			}
			f.runner = "inactive"
			f.calls = append(f.calls, "runner exited")
		}
		return f.runner + "\n", "", 0
	case line == "systemctl stop --no-block forgejo-runner.service":
		f.runner = "stopping"
		return "", "", 0
	case strings.Contains(line, "/sys/fs/cgroup 2>/dev/null"):
		return f.cgroup, "", 0
	case command[0] == "journalctl":
		return f.journal, "", 0
	case strings.HasPrefix(line, "sh -c chgrp"):
		path := command[4]
		f.files[path] = f.files[path+".tmp"]
		delete(f.files, path+".tmp")
		return "", "", 0
	case command[0] == "false":
		return "", "it failed\n", 3
	}
	return "", "", 0
}

func (f *fakeIncus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.URL.Path == "/1.0":
		auth := "trusted"
		if !f.trusted {
			auth = "untrusted"
		}
		sync200(w, map[string]any{"auth": auth, "api_extensions": []string{"container_exec_recording"}})
	case strings.HasPrefix(r.URL.Path, "/1.0/operations/") && strings.HasSuffix(r.URL.Path, "/wait"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/1.0/operations/"), "/wait")
		op, ok := f.ops[id]
		if !ok {
			fail(w, 404, "Operation not found")
			return
		}
		sync200(w, op)
	case r.URL.Path == "/1.0/instances/ci/state" && r.Method == http.MethodGet:
		sync200(w, map[string]any{"status": f.status, "status_code": 103, "pid": 42, "processes": 12,
			"cpu": map[string]any{"usage": 5_000_000_000},
			"memory": map[string]any{"usage": 100 << 20, "usage_peak": f.peak, "swap_usage": 0,
				"swap_usage_peak": 1 << 20, "total": 4 << 30}})
	case r.URL.Path == "/1.0/instances/ci/state" && r.Method == http.MethodPut:
		var body struct {
			Action string `json:"action"`
			Force  bool   `json:"force"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, fmt.Sprintf("state %s force=%v", body.Action, body.Force))
		if body.Action == "start" {
			f.status = "Running"
		} else {
			f.status = "Stopped"
		}
		f.async(w, map[string]any{}, "")
	case r.URL.Path == "/1.0/instances/ci" && r.Method == http.MethodGet:
		w.Header().Set("ETag", f.etag)
		sync200(w, map[string]any{"name": "ci", "architecture": "x86_64", "config": map[string]string{"limits.memory": "4GiB"},
			"devices": f.devices, "ephemeral": false, "profiles": []string{"default"}, "stateful": false,
			"description": "", "status": f.status, "expanded_config": map[string]string{}})
	case r.URL.Path == "/1.0/instances/ci" && r.Method == http.MethodPut:
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if raw, ok := body["restore"]; ok {
			var snapshot string
			_ = json.Unmarshal(raw, &snapshot)
			f.calls = append(f.calls, "restore "+snapshot)
			failure := ""
			if f.badSnap {
				failure = "Failed to restore snapshot: not found"
			}
			f.async(w, map[string]any{}, failure)
			return
		}
		if r.Header.Get("If-Match") != f.etag {
			fail(w, 412, "ETag doesn't match")
			return
		}
		if _, ok := body["status"]; ok {
			fail(w, 400, "status is not writable")
			return
		}
		_ = json.Unmarshal(body["devices"], &f.devices)
		f.async(w, map[string]any{}, "")
	case r.URL.Path == "/1.0/instances/ci/exec" && r.Method == http.MethodPost:
		var body struct {
			Command      []string `json:"command"`
			RecordOutput bool     `json:"record-output"`
			WaitForWS    bool     `json:"wait-for-websocket"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.RecordOutput || body.WaitForWS {
			fail(w, 400, "the test serves recorded output only")
			return
		}
		f.calls = append(f.calls, "exec "+strings.Join(body.Command, " "))
		stdout, stderr, code := f.exec(body.Command)
		f.execCount++
		base := fmt.Sprintf("/1.0/instances/ci/logs/exec-output/exec_%d", f.execCount)
		f.logs[base+".stdout"], f.logs[base+".stderr"] = stdout, stderr
		f.async(w, map[string]any{"return": code, "output": map[string]string{"1": base + ".stdout", "2": base + ".stderr"}}, "")
	case strings.HasPrefix(r.URL.Path, "/1.0/instances/ci/logs/exec-output/"):
		text, ok := f.logs[r.URL.Path]
		if !ok {
			fail(w, 404, "not found")
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.logs, r.URL.Path)
			sync200(w, map[string]any{})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, text)
	case strings.HasPrefix(r.URL.Path, "/1.0/storage-pools/default/volumes/custom/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(r.URL.Path, "/1.0/storage-pools/default/volumes/custom/")
		if !f.volumes[name] || f.hidden[name] {
			delete(f.hidden, name)
			fail(w, 404, "Storage volume not found")
			return
		}
		sync200(w, map[string]any{"name": name, "type": "custom", "content_type": "filesystem"})
	case r.URL.Path == "/1.0/storage-pools/default/volumes" && r.Method == http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			ContentType string `json:"content_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, fmt.Sprintf("volume create %s %s %s", body.Name, body.Type, body.ContentType))
		if f.volumes[body.Name] || body.Type != "custom" {
			fail(w, 409, "Volume by that name already exists")
			return
		}
		f.volumes[body.Name] = true
		sync200(w, map[string]any{})
	case r.URL.Path == "/1.0/instances/ci/logs/lxc.log":
		_, _ = io.WriteString(w, "lxc ci: started\nlxc ci: last line\n")
	case r.URL.Path == "/1.0/instances/ci/files" && r.Method == http.MethodPost:
		data, _ := io.ReadAll(r.Body)
		path := r.URL.Query().Get("path")
		f.calls = append(f.calls, fmt.Sprintf("file %s uid=%s mode=%s write=%s", path, r.Header.Get("X-Incus-Uid"),
			r.Header.Get("X-Incus-Mode"), r.Header.Get("X-Incus-Write")))
		f.files[path] = string(data)
		sync200(w, map[string]any{})
	default:
		fail(w, 404, "not found")
	}
}

// certificate makes a self-signed certificate (no CA) and its key, as PEM files.
func certificate(t *testing.T, dir, name string, server bool) (string, string, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.DNSNames = []string{"incus.invalid"}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certFile, keyFile := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, pair
}

// serveTLS starts the fake API with TLS on a unix socket (a sandbox may refuse loopback),
// requiring a client certificate, and returns a client of it through that socket.
func serveTLS(t *testing.T, f *fakeIncus) (*Client, config.TLS, *[]*x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	serverCert, _, serverPair := certificate(t, dir, "server", true)
	clientCert, clientKey, _ := certificate(t, dir, "client", false)
	path := filepath.Join(dir, "incus.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var seen []*x509.Certificate
	server := httptest.NewUnstartedServer(f)
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			cert, err := x509.ParseCertificate(raw[0])
			if err == nil {
				seen = append(seen, cert)
			}
			return err
		}}
	server.StartTLS()
	t.Cleanup(server.Close)
	settings := config.TLS{CAFile: serverCert, CertFile: clientCert, KeyFile: clientKey}
	client, err := NewClient("https://ci-1.example.org:8443", settings, "default")
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
	return client, settings, &seen
}

func testSettings(t *testing.T) *slot.Settings {
	t.Helper()
	dir := t.TempDir()
	template := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(template, []byte("uuid: @@RUNNER_UUID@@\nlabels: @@RUNNER_LABELS@@\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(`
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
hosts: [{name: ci-1, address: "https://ci-1.example.org:8443", memory_mib: 8000, slots: [ci]}]
provider:
  type: incus
  incus:
    config_template: ` + template + `
    state_dir: ` + filepath.Join(dir, "state") + `
    boot_timeout: 5
    tls: {cert_file: c, key_file: k}
    labels: {shared: {docker: "docker:docker://node:24"}}
    cache_volumes: {shared: cache-shared}
`))
	if err != nil {
		t.Fatal(err)
	}
	return Settings(cfg.Provider.Incus, "ci-1", "ci")
}

func TestASlotsLifeThroughTheAPI(t *testing.T) {
	f := newFakeIncus()
	f.status = "Running"
	client, _, seen := serveTLS(t, f)
	s := testSettings(t)
	system := slot.NewIncusOn(s, NewInstance(client, "ci", 5*time.Second))
	if err := system.Reset(); err != nil {
		t.Fatal(err)
	}
	if len(*seen) == 0 || (*seen)[0].Subject.CommonName != "client" {
		t.Fatal("the client certificate was not presented")
	}
	want := []string{"state stop force=true", "restore clean", "state start force=false"}
	got := slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool {
		return !strings.HasPrefix(c, "state ") && !strings.HasPrefix(c, "restore ")
	})
	if !slices.Equal(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	a := provider.Parsed{ID: "ci-1-ci-7-42", UUID: testUUID, Token: testToken, Class: "shared",
		Labels: []string{"docker:docker://node:24"}, UnusedMinutes: 15}
	if err := system.Apply(a); err != nil {
		t.Fatal(err)
	}
	cache := f.devices["cache"]
	if cache["source"] != "cache-shared" || cache["pool"] != "default" || f.devices["root"] == nil {
		t.Fatalf("devices %v", f.devices)
	}
	if f.files["/etc/forgejo-runner/token"] != testToken ||
		f.files["/etc/forgejo-runner/config.yml"] != "uuid: "+testUUID+"\nlabels: [\"docker:docker://node:24\"]\n" {
		t.Fatalf("files %v", f.files)
	}
	if !slices.Contains(f.calls, "file /etc/forgejo-runner/token.tmp uid=0 mode=0600 write=overwrite") {
		t.Fatalf("calls %v", f.calls)
	}
	for _, c := range f.calls {
		if strings.Contains(c, testToken) {
			t.Fatal("the token is in a command")
		}
	}
	if err := system.StartRunner(); err != nil || !slices.Contains(f.calls, "exec systemctl start forgejo-runner.service") {
		t.Fatalf("start: %v", err)
	}
	if !system.RunnerActive() || system.Busy() {
		t.Fatal("the runner alone is active, not busy")
	}
	f.mu.Lock()
	f.runner = "busy"
	f.mu.Unlock()
	time.Sleep(lookFor + 100*time.Millisecond)
	if !system.Busy() {
		t.Fatal("a job is not seen")
	}
	f.mu.Lock()
	f.journal = `Nov 23 runner[1]: time="x" level=info msg="task 8480 repo is alice/app https://data.forgejo.org https://forgejo.example.org"` + "\n"
	f.peak = 3 << 20
	f.cgroup = "@memory.peak\n1\n@memory.swap.peak\n\n@cpu.stat\nusage_usec 7000000\n@cpu.pressure\nsome avg10=0.00 total=42\n"
	f.mu.Unlock()
	if task, peak := system.Measure(); task != 8480 || peak != 4 {
		t.Fatalf("measured %d %d", task, peak)
	}
	if u, p, ok := system.CPU(); !ok || u != 7000000 || p != 42 {
		t.Fatalf("CPU %d %d %v", u, p, ok)
	}
	if len(f.logs) != 0 {
		t.Fatalf("exec records left behind: %v", f.logs)
	}
	f.mu.Lock()
	f.peak = 0
	f.cgroup = "@memory.peak\n2097152\n@memory.swap.peak\n0\n"
	f.mu.Unlock()
	if _, peak := system.Measure(); peak != 2 {
		t.Fatalf("without the state's peak: %d", peak)
	}
}

func TestAFailedOperationIsAnError(t *testing.T) {
	f := newFakeIncus()
	f.badSnap = true
	client, _, _ := serveTLS(t, f)
	system := slot.NewIncusOn(testSettings(t), NewInstance(client, "ci", 5*time.Second))
	if err := system.Reset(); err == nil || !strings.Contains(err.Error(), "Failed to restore snapshot") {
		t.Fatalf("reset: %v", err)
	}
	instance := NewInstance(client, "ci", 5*time.Second)
	_, errOut, err := instance.Exec(t.Context(), "false")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 || string(errOut) != "it failed\n" {
		t.Fatalf("exec: %v %q", err, errOut)
	}
	if _, err := NewInstance(client, "other", time.Second).Running(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("an unknown container: %v", err)
	}
}

func TestTheServerIsPinnedAndMustTrustTheBroker(t *testing.T) {
	f := newFakeIncus()
	client, settings, _ := serveTLS(t, f)
	if err := client.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.trusted = false
	if err := client.Ping(t.Context()); err == nil || !strings.Contains(err.Error(), "does not trust") {
		t.Fatalf("an untrusted broker: %v", err)
	}
	// Another server's certificate pinned: the handshake fails.
	other, _, _ := certificate(t, t.TempDir(), "other", true)
	settings.CAFile = other
	wrong, err := NewClient("https://ci-1.example.org:8443", settings, "default")
	if err != nil {
		t.Fatal(err)
	}
	wrong.http.Transport.(*http.Transport).DialContext = client.http.Transport.(*http.Transport).DialContext
	if err := wrong.Ping(t.Context()); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("a server not pinned: %v", err)
	}
	if _, err := NewClient("http://ci-1.example.org", config.TLS{}, ""); err == nil {
		t.Fatal("plain HTTP was taken")
	}
}

func TestTheProviderReadsItsSlotsInShadowMode(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
hosts: [{name: ci-1, address: "unix://` + filepath.Join(dir, "none.sock") + `", memory_mib: 8000, slots: [ci-a, ci-b]}]
history: {path: ` + filepath.Join(dir, "history.sqlite") + `}
shadow: true
provider:
  type: incus
  incus:
    config_template: /dev/null
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
	if _, err := hosted.Status(t.Context(), cfg.Hosts[0]); err == nil {
		t.Fatal("a server that is not there answered")
	}
	if !strings.HasSuffix(cfg.Provider.Incus.StateDir, "/slots") {
		t.Fatalf("state dir %s", cfg.Provider.Incus.StateDir)
	}
	if _, err := os.Stat(filepath.Join(dir, "slots", "ci-1")); err != nil {
		t.Fatal(err)
	}
}

// Each host is reached with its own TLS where it has one: here a host's certificate to pin
// that is not there fails that host alone.
func TestAHostsOwnServerCertificate(t *testing.T) {
	dir := t.TempDir()
	serverCert, _, _ := certificate(t, dir, "server", true)
	clientCert, clientKey, _ := certificate(t, dir, "client", false)
	text := `
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
hosts:
  - {name: ci-1, address: "https://ci-1.example.org:8443", memory_mib: 8000, slots: [ci]}
  - {name: ci-2, address: "https://ci-2.example.org:8443", memory_mib: 8000, slots: [ci],
     tls: {ca_file: ` + filepath.Join(dir, "missing.crt") + `}}
history: {path: ` + filepath.Join(dir, "history.sqlite") + `}
shadow: true
provider:
  type: incus
  incus:
    config_template: /dev/null
    tls: {ca_file: ` + serverCert + `, cert_file: ` + clientCert + `, key_file: ` + clientKey + `}
    labels: {shared: {docker: "docker:docker://node:24"}}
`
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "missing.crt") {
		t.Fatalf("ci-2's own certificate was not used: %v", err)
	}
	cfg.Hosts = cfg.Hosts[:1]
	hosted, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("ci-1 with the provider's: %v", err)
	}
	hosted.Stop()
}

// A slot's own cache volume ({host}, {slot}) is made in the pool at its first use, and
// attached as it is afterwards.
func TestASlotsOwnCacheVolumeIsMade(t *testing.T) {
	f := newFakeIncus()
	f.status = "Running"
	client, _, _ := serveTLS(t, f)
	s := testSettings(t)
	cfg := config.Incus{}
	cfg.CacheVolumes = map[string]string{"shared": "cache-{host}-{slot}"}
	s.CacheVolumes = slot.ExpandVolumes(cfg.CacheVolumes, "ci-1", "ci")
	system := slot.NewIncusOn(s, NewInstance(client, "ci", 5*time.Second))
	a := provider.Parsed{ID: "ci-1-ci-7-42", UUID: testUUID, Token: testToken, Class: "shared",
		Labels: []string{"docker:docker://node:24"}, UnusedMinutes: 15}
	for range 2 {
		if err := system.Apply(a); err != nil {
			t.Fatal(err)
		}
	}
	if f.devices["cache"]["source"] != "cache-ci-1-ci" || !f.volumes["cache-ci-1-ci"] {
		t.Fatalf("devices %v volumes %v", f.devices, f.volumes)
	}
	var made []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "volume create") {
			made = append(made, c)
		}
	}
	if !slices.Equal(made, []string{"volume create cache-ci-1-ci custom filesystem"}) {
		t.Fatalf("made %v", made)
	}
	// Made by another slot in between: the conflict is no failure.
	f.mu.Lock()
	f.volumes["raced"], f.hidden["raced"] = true, true
	f.mu.Unlock()
	instance := NewInstance(client, "ci", 5*time.Second)
	if err := instance.ensureVolume(t.Context(), "default", "raced"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "volume create raced custom filesystem") {
		t.Fatal("the race was not run")
	}
	// A volume that cannot be made fails the attachment.
	f.mu.Lock()
	f.volumes = map[string]bool{}
	f.mu.Unlock()
	if err := instance.SetCache(t.Context(), "other-pool", "x", "/cache"); err == nil {
		t.Fatal("a volume of a pool that is not there was attached")
	}
}

// The runner's journal goes into the broker's log at info, line by line, and at warn when
// the runner failed: a runner that does not start is seen without debug logging.
func TestTheRunnersJournalIsInTheLog(t *testing.T) {
	f := newFakeIncus()
	f.status = "Running"
	f.journal = "2026-11-24T10:00:00+0000 ci runner[1]: level=info msg=\"Starting runner daemon\"\n" +
		"2026-11-24T10:00:01+0000 ci runner[1]: level=error msg=\"cannot reach the instance\"\n"
	client, _, _ := serveTLS(t, f)
	var logs bytes.Buffer
	system := slot.NewIncusOn(testSettings(t), NewInstance(client, "ci", 5*time.Second))
	system.Log = slog.New(slog.NewTextHandler(&logs, nil))
	system.SaveJournal(false)
	if n := strings.Count(logs.String(), "level=INFO msg=runner line="); n != 2 ||
		!strings.Contains(logs.String(), `cannot reach the instance`) {
		t.Fatalf("log:\n%s", logs.String())
	}
	logs.Reset()
	system.SaveJournal(true)
	if n := strings.Count(logs.String(), "level=WARN msg=runner line="); n != 2 {
		t.Fatalf("log of a failed runner:\n%s", logs.String())
	}
	logs.Reset()
	f.mu.Lock()
	f.status = "Stopped"
	f.mu.Unlock()
	system.SaveJournal(true)
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "not running") {
		t.Fatalf("log of a failed runner whose container stopped:\n%s", logs.String())
	}
}

// A job cut short (the slot paused at once) is stopped through the runner's service: the
// container is stopped for its reset only after the runner exited, having reported the
// job's end itself.
func TestACutJobsRunnerExitsBeforeTheContainerStops(t *testing.T) {
	f := newFakeIncus()
	f.status = "Running"
	f.runner = "busy"
	f.stopLooks = 3
	f.journal = `Nov 23 runner[1]: time="x" level=info msg="task 8480 repo is alice/app https://data.forgejo.org https://forgejo.example.org"` + "\n"
	client, _, _ := serveTLS(t, f)
	s := testSettings(t)
	instance := NewInstance(client, "ci", 5*time.Second)
	system := slot.NewIncusOn(s, instance)
	system.Log = slog.New(slog.DiscardHandler)
	now := time.Unix(1_000_000, 0)
	c := slot.NewCycle(s, system, slot.CycleOptions{Log: system.Log,
		Now: func() time.Time { return now },
		Sleep: func(d time.Duration) {
			now = now.Add(d)
			// Every look of the fake clock's is a new one.
			instance.mu.Lock()
			instance.lookedAt = time.Time{}
			instance.mu.Unlock()
		}})
	handoff := `{"assignment": "ci-1-ci-7-42", "uuid": "` + testUUID + `", "token": "` + testToken +
		`", "labels": ["docker"], "class": "shared", "unused_minutes": 15}`
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"ci.assignment": handoff, "ci.pause": slot.PauseStop} {
		if err := os.WriteFile(filepath.Join(s.StateDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, ok := c.WaitAssignment()
	if !ok || !c.Serve(a) || c.Last().Task != 8480 {
		t.Fatalf("served %v, last %+v", ok, c.Last())
	}
	if !c.Reset() {
		t.Fatal("the reset failed")
	}
	stop := slices.Index(f.calls, "exec systemctl stop --no-block forgejo-runner.service")
	exited := slices.Index(f.calls, "runner exited")
	stopped := slices.Index(f.calls, "state stop force=true")
	if stop < 0 || exited < stop || stopped < exited {
		t.Fatalf("calls %v", f.calls)
	}
}
