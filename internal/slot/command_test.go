// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testUUID  = "0f0e0d0c-0b0a-4908-8706-050403020100"
	testToken = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// hostDir is the settings of a slot ci (ready) and of a container kept for something else.
func hostDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"etc", "run"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	slot := `container: ci
state_dir: ` + filepath.Join(dir, "run") + `
config_template: /dev/null
labels:
  trusted: {ubuntu-latest: "ubuntu-latest:host"}
  shared: {docker: "docker:docker://node:24-trixie"}
cache_volumes: {trusted: ci-cache-trusted, shared: ci-cache-shared}
`
	write(t, filepath.Join(dir, "etc/ci.yml"), slot)
	write(t, filepath.Join(dir, "etc/other.yml"), "container: other\nenabled: false\n")
	write(t, filepath.Join(dir, "run/ci.json"),
		`{"state": "ready", "state_since": 5, "assignment": "", "last": {"assignment": "a-0", "task": 3}}`)
	return dir
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func command(dir, request, stdin string) (int, string) {
	var out, errOut bytes.Buffer
	code := Command(request, filepath.Join(dir, "etc"), strings.NewReader(stdin), &out, &errOut)
	return code, out.String() + errOut.String()
}

func assignment(changes map[string]any) string {
	data := map[string]any{
		"assignment": "ci-1-ci-1-7", "uuid": testUUID, "token": testToken,
		"labels": []string{"ubuntu-latest"}, "class": "trusted", "unused_minutes": 15,
	}
	maps.Copy(data, changes)
	text, _ := json.Marshal(data)
	return string(text)
}

func TestStatusListsTheEnabledSlotsOnly(t *testing.T) {
	dir := hostDir(t)
	code, out := command(dir, "status", "")
	want := `{"slots":[{"container":"ci","state":"ready","state_since":5,"assignment":"",` +
		`"last":{"assignment":"a-0","task":3}}]}`
	if code != 0 || strings.TrimSpace(out) != want {
		t.Fatalf("status = %d %s", code, out)
	}
}

func TestAReadySlotTakesAnAssignment(t *testing.T) {
	dir := hostDir(t)
	code, out := command(dir, "assign ci", assignment(nil))
	if code != 0 || strings.TrimSpace(out) != "ci-1-ci-1-7" {
		t.Fatalf("assign = %d %s", code, out)
	}
	path := filepath.Join(dir, "run/ci.assignment")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("written %v %v", st, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), testToken) {
		t.Fatal("the token was not written for the cycle")
	}
}

func TestWhatASlotRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, request, state, stdin, refusal string
	}{
		{"busy", "assign ci", "running", assignment(nil), "not ready"},
		{"not a slot", "assign other", "ready", assignment(nil), "no slot"},
		{"class", "assign ci", "ready", assignment(map[string]any{"class": "shared"}), "refused"},
		{"token", "assign ci", "ready", assignment(map[string]any{"token": "x"}), "refused"},
		{"list", "assign ci", "ready", "[]", "refused"},
		{"large", "assign ci", "ready", strings.Repeat("x", 20000), "too large"},
		{"path", "assign ../ci", "ready", assignment(nil), "usage"},
		{"shell", "shell", "ready", "", "usage"},
		{"chained", "status; id", "ready", "", "usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := hostDir(t)
			write(t, filepath.Join(dir, "run/ci.json"), `{"state": "`+tc.state+`"}`)
			code, out := command(dir, tc.request, tc.stdin)
			if code != 1 || !strings.Contains(out, tc.refusal) {
				t.Fatalf("%s = %d %q, want a refusal with %q", tc.request, code, out, tc.refusal)
			}
			if _, err := os.Stat(filepath.Join(dir, "run/ci.assignment")); err == nil {
				t.Fatal("an assignment was written")
			}
		})
	}
}

func TestAPendingAssignmentIsReported(t *testing.T) {
	dir := hostDir(t)
	if code, out := command(dir, "assign ci", assignment(nil)); code != 0 {
		t.Fatal(out)
	}
	_, out := command(dir, "status", "")
	var doc struct {
		Slots []struct{ State, Assignment string }
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Slots[0].State != "pending" || doc.Slots[0].Assignment != "ci-1-ci-1-7" || strings.Contains(out, testToken) {
		t.Fatalf("status %s", out)
	}
}

func TestASecondAssignmentWaitsForNone(t *testing.T) {
	dir := hostDir(t)
	if code, out := command(dir, "assign ci", assignment(nil)); code != 0 {
		t.Fatal(out)
	}
	code, out := command(dir, "assign ci", assignment(map[string]any{"assignment": "ci-1-ci-2-8"}))
	if code != 1 || !strings.Contains(out, "waiting") {
		t.Fatalf("second assign = %d %s", code, out)
	}
}

func TestRelease(t *testing.T) {
	dir := hostDir(t)
	if code, out := command(dir, "release ci ci-1-ci-1-7", ""); code != 1 || !strings.Contains(out, "does not hold") {
		t.Fatalf("release of a foreign assignment = %d %s", code, out)
	}
	write(t, filepath.Join(dir, "run/ci.json"), `{"state": "assigned", "assignment": "ci-1-ci-1-7"}`)
	if code, out := command(dir, "release ci ci-1-ci-1-7", ""); code != 0 {
		t.Fatalf("release = %d %s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "run/ci.release")); string(data) != "ci-1-ci-1-7" {
		t.Fatalf("release file %q", data)
	}
}

func TestPauseAndResume(t *testing.T) {
	dir := hostDir(t)
	if code, out := command(dir, "pause ci", ""); code != 0 || out != "ready\n" {
		t.Fatalf("pause = %d %q", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "run/ci.pause")); string(data) != "finish\n" {
		t.Fatalf("pause file %q", data)
	}
	code, out := command(dir, "status", "")
	if code != 0 || !strings.Contains(out, `"paused":true`) {
		t.Fatalf("status = %d %s", code, out)
	}
	// A paused slot takes nothing, even while its cycle still reports it ready.
	if code, out := command(dir, "assign ci", assignment(nil)); code != 1 || !strings.Contains(out, "is paused") {
		t.Fatalf("assign to a paused slot = %d %s", code, out)
	}
	if code, _ := command(dir, "pause ci stop", ""); code != 0 {
		t.Fatal("pause stop failed")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "run/ci.pause")); string(data) != "stop\n" {
		t.Fatalf("pause file %q", data)
	}
	for _, request := range []string{"pause ci now", "pause other", "pause", "resume ci x"} {
		if code, _ := command(dir, request, ""); code != 1 {
			t.Errorf("%q was taken", request)
		}
	}
	if code, out := command(dir, "resume ci", ""); code != 0 || out != "ready\n" || exists(filepath.Join(dir, "run/ci.pause")) {
		t.Fatalf("resume = %d %q", code, out)
	}
	if code, _ := command(dir, "resume ci", ""); code != 0 {
		t.Fatal("resuming a slot not paused failed")
	}
	if code, out := command(dir, "assign ci", assignment(nil)); code != 0 {
		t.Fatalf("assign after the resume = %d %s", code, out)
	}
}

func TestTheSlotExampleIsValid(t *testing.T) {
	s, err := LoadSettings("../../examples/slot.yml")
	if err != nil {
		t.Fatal(err)
	}
	if s.CacheVolumes["shared"] != "ci-cache-ci-a-shared" {
		t.Fatalf("volumes %v", s.CacheVolumes)
	}
}
