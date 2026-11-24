// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// slotDir is a settings directory with one ready slot ci, and another one with a slot of a
// different name: a request that changed the settings directory would show that slot.
func slotDir(t *testing.T, container string) string {
	t.Helper()
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := "container: " + container + "\nstate_dir: " + run + "\nconfig_template: /dev/null\n" +
		"labels: {shared: {docker: \"docker:docker://node:24\"}}\n"
	if err := os.WriteFile(filepath.Join(dir, container+".yml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTheSlotRequestAfterTheDashesIsNeverAFlag(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "")
	dir := slotDir(t, "ci")
	other := slotDir(t, "evil")
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"status", []string{"--settings-dir", dir, "--", "status"}, 0, `"container":"ci"`},
		{"a request of a flag's shape", []string{"--settings-dir", dir, "--", "--settings-dir=" + other},
			1, "usage: status"},
		{"a flag and a request in one", []string{"--settings-dir", dir, "--", "-settings-dir=" + other + " status"},
			1, "usage: status"},
		{"a bare dash", []string{"--settings-dir", dir, "--", "-"}, 1, "usage: status"},
		{"two requests", []string{"--settings-dir", dir, "--", "status", "status"}, 1, "one argument"},
		{"an unknown flag", []string{"--settings", dir, "status"}, 2, "usage: forgejo-berth slot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(append([]string{"slot"}, tc.args...), strings.NewReader(""), &out, &errOut)
			text := out.String() + errOut.String()
			if code != tc.code || !strings.Contains(text, tc.want) || strings.Contains(text, "evil") {
				t.Fatalf("exit %d, output %q; want %d and %q", code, text, tc.code, tc.want)
			}
		})
	}
}

func TestTheSlotRequestFromTheEnvironment(t *testing.T) {
	dir := slotDir(t, "ci")
	t.Setenv("SSH_ORIGINAL_COMMAND", "status")
	var out, errOut bytes.Buffer
	if code := run([]string{"slot", "--settings-dir", dir}, strings.NewReader(""), &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), `"container":"ci"`) {
		t.Fatalf("exit %d: %s%s", code, out.String(), errOut.String())
	}
}

// pause and resume of a docker slot change its state directory, where the broker's cycle
// sees them; a slot that is not configured is refused.
func TestPauseAndResumeADockerSlot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	text := `forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
hosts: [{name: local, address: "unix:///run/docker.sock", memory_mib: 8000, slots: [a, b]}]
history: {path: ` + filepath.Join(dir, "history.sqlite") + `}
provider: {type: docker, docker: {labels: {shared: {docker: d}}}}
`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	pauseFile := filepath.Join(dir, "slots", "local", "b.pause")
	call := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := run(args, strings.NewReader(""), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, out := call("pause", "--config", path, "--stop", "local/b"); code != 0 ||
		out != "local/b: paused; the slot is unknown\n" {
		t.Fatalf("pause = %d %q", code, out)
	}
	if data, _ := os.ReadFile(pauseFile); string(data) != "stop\n" {
		t.Fatalf("pause file %q", data)
	}
	if code, out := call("resume", "--config", path, "local/b"); code != 0 || !strings.Contains(out, "resumed") {
		t.Fatalf("resume = %d %q", code, out)
	}
	if _, err := os.Stat(pauseFile); err == nil {
		t.Fatal("still paused")
	}
	for _, args := range [][]string{{"pause", "--config", path, "local/c"}, {"pause", "--config", path, "ci-9/a"}} {
		if code, out := call(args...); code != 1 || !strings.Contains(out, "no ") {
			t.Errorf("%v = %d %q", args, code, out)
		}
	}
	for _, args := range [][]string{{"pause", "--config", path, "local"}, {"resume", "--stop", "local/b"},
		{"pause", "--config", path}} {
		if code, _ := call(args...); code != 2 {
			t.Errorf("%v was taken", args)
		}
	}
}
