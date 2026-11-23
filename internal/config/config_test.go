// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
hosts: [{name: ci-1, address: ci-1.example.org, memory_mib: 8000}]
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Estimate
	if e.HistoryRuns != 5 || e.SpikeDays != 30 || e.Margin != 1.2 || e.RelativeMargin != 1.5 || e.FloorMiB != 512 ||
		e.Unknown() != 6144 || e.RetentionDays != 180 {
		t.Fatalf("estimate defaults %+v", e)
	}
	if c.IntervalSeconds != 10 || c.UnusedMinutes != 15 || c.StarveMinutes != 10 || c.RunnerPrefix != "berth-" ||
		c.Provider.Type != "ssh" || c.Provider.SSH.Port != 22 || c.Provider.SSH.User != "berth" {
		t.Fatalf("defaults %+v", c)
	}
}

func TestUnknownMiBZeroIsKept(t *testing.T) {
	c, err := Parse([]byte(minimal + "estimate: {unknown_mib: 0}\n"))
	if err != nil || c.Estimate.Unknown() != 0 {
		t.Fatalf("unknown_mib %v, %v", c.Estimate.UnknownMiB, err)
	}
}

func TestWhatIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"a typo", "intervall_seconds: 5", "intervall_seconds"},
		{"a host name with capitals", "hosts: [{name: CI, address: a, memory_mib: 1}]", "hosts[0].name"},
		{"an address like an option", "hosts: [{name: ci, address: -oProxyCommand=x, memory_mib: 1}]", "address"},
		{"no memory", "hosts: [{name: ci, address: a}]", "memory_mib"},
		{"a provider not built", "provider: {type: kubernetes}", "provider.type"},
		{"a margin below one", "estimate: {margin: 0.5}", "margin"},
		{"an owner name with a slash", "trusted: {owners: {a/b: t}, labels: [x]}", "user name"},
		{"no labels at all", "shared: {labels: []}", "labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := minimal
			// A later key of the same name would be a duplicate: replace the line instead.
			for _, key := range []string{"hosts:", "shared:"} {
				if strings.HasPrefix(tc.extra, key) {
					lines := strings.Split(text, "\n")
					for i, l := range lines {
						if strings.HasPrefix(l, key) {
							lines[i] = ""
						}
					}
					text = strings.Join(lines, "\n")
				}
			}
			_, err := Parse([]byte(text + tc.extra + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one about %s", err, tc.want)
			}
		})
	}
}

func TestSecretsFromTheCredentialsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin.token"), []byte(" s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if got, err := ReadSecret("admin.token"); err != nil || got != "s3cret" {
		t.Fatalf("ReadSecret = %q, %v", got, err)
	}
	if got := Secret("/etc/x"); got != "/etc/x" {
		t.Fatalf("an absolute path moved: %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret("empty"); err == nil {
		t.Fatal("an empty secret was taken")
	}
}

func TestTheExampleIsValid(t *testing.T) {
	data, err := os.ReadFile("../../examples/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
}
