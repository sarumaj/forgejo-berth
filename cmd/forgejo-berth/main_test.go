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
