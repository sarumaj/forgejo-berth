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

func TestBackoff(t *testing.T) {
	for _, tc := range []struct {
		name, extra   string
		runs          int
		base, maxMins float64
	}{
		{"the defaults: three runners, from unused_minutes to four hours", "", 3, 15, 240},
		{"the base follows unused_minutes", "unused_minutes: 20", 3, 20, 240},
		{"0 turns it off", "backoff: {unused_runs: 0}", 0, 15, 240},
		{"set", "backoff: {unused_runs: 2, base_minutes: 5, max_minutes: 60}", 2, 5, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(minimal + tc.extra + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if b := c.Backoff; b.Runs() != tc.runs || b.BaseMinutes != tc.base || b.MaxMinutes != tc.maxMins {
				t.Fatalf("backoff %d %g %g", b.Runs(), b.BaseMinutes, b.MaxMinutes)
			}
		})
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
		{"a back-off key unknown", "backoff: {unused: 3}", "unused"},
		{"negative unused runs", "backoff: {unused_runs: -1}", "backoff"},
		{"a cap below the base", "backoff: {base_minutes: 30, max_minutes: 10}", "max_minutes"},
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

const dockerHosts = `
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
hosts: [{name: local, address: "unix:///run/docker.sock", memory_mib: 8000, slots: [a, b]}]
history: {path: /srv/berth/history.sqlite}
`

func TestTheDockerProvider(t *testing.T) {
	c, err := Parse([]byte(dockerHosts + `
provider:
  type: docker
  docker:
    labels: {shared: {docker: "docker:docker://node:24"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	d := c.Provider.Docker
	if d.JobEngine != EngineDinD || d.ForgejoURL != c.Forgejo.URL || d.StateDir != "/srv/berth/slots" ||
		d.Image == "" || d.DinDImage == "" || d.JobHours != 4 || d.StopTimeout != 90 ||
		d.TimeoutSeconds != 20 {
		t.Fatalf("docker defaults %+v", d)
	}
	for _, tc := range []struct{ name, provider, want string }{
		{"no labels", "{type: docker}", "labels"},
		{"a class not known", `{type: docker, docker: {labels: {root: {x: y}}}}`, "classes"},
		{"an engine not known", `{type: docker, docker: {job_engine: socket, labels: {shared: {docker: d}}}}`,
			"job_engine"},
		{"a typo", `{type: docker, docker: {imag: x, labels: {shared: {docker: d}}}}`, "imag"},
		{"a negative stop timeout", `{type: docker, docker: {stop_timeout: -1, labels: {shared: {docker: d}}}}`,
			"stop_timeout"},
		{"a remote engine without TLS", `{type: docker, docker: {labels: {shared: {docker: d}}}}`, "needs"},
		{"an incus provider without a template", `{type: incus, incus: {labels: {shared: {docker: d}}}}`,
			"config_template"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := dockerHosts
			if tc.name == "a remote engine without TLS" {
				text = strings.Replace(text, "unix:///run/docker.sock", "tcp://ci-1.example.org:2376", 1)
			}
			_, err := Parse([]byte(text + "provider: " + tc.provider + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one about %s", err, tc.want)
			}
		})
	}
	if _, err := Parse([]byte(dockerHosts)); err == nil || !strings.Contains(err.Error(), "lists its slots itself") {
		t.Fatalf("slots on an SSH host: %v", err)
	}
	noSlots := strings.Replace(dockerHosts, ", slots: [a, b]", "", 1)
	if _, err := Parse([]byte(noSlots + "provider: {type: docker, docker: {labels: {shared: {docker: d}}}}\n")); err == nil ||
		!strings.Contains(err.Error(), "names its slots") {
		t.Fatalf("a docker host without slots: %v", err)
	}
}

func TestTheProviderExamplesAreValid(t *testing.T) {
	for _, name := range []string{"config-docker.yml", "config-incus.yml"} {
		data, err := os.ReadFile("../../examples/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

const incusHosts = `
forgejo: {url: https://forgejo.example.org, admin_token_file: admin.token}
shared: {labels: [docker]}
trusted: {labels: [docker, ubuntu-latest]}
history: {path: /srv/berth/history.sqlite}
hosts:
  - {name: ci-1, address: "https://ci-1.example.org:8443", memory_mib: 8000, slots: [a]}
  - name: ci-2
    address: "https://ci-2.example.org:8443"
    memory_mib: 8000
    slots: [a]
    tls: {ca_file: /etc/berth/ci-2.crt}
    config_template: /etc/berth/ci-2.yml
    labels: {trusted: {ubuntu-latest: "ubuntu-latest:host"}}
`

func TestPerHostSettings(t *testing.T) {
	c, err := Parse([]byte(incusHosts + `
provider:
  type: incus
  incus:
    config_template: /etc/berth/runner.yml
    tls: {ca_file: /etc/berth/server.crt, cert_file: /etc/berth/broker.crt, key_file: broker.key}
    labels:
      trusted: {docker: "docker:docker://node:24"}
      shared: {docker: "docker:docker://node:24"}
`))
	if err != nil {
		t.Fatal(err)
	}
	one, two := c.Hosts[0], c.Hosts[1]
	if got := c.HostTLS(one); got != c.Provider.Incus.TLS {
		t.Fatalf("ci-1's TLS %+v", got)
	}
	if got := c.HostTLS(two); got.CAFile != "/etc/berth/ci-2.crt" || got.CertFile != "/etc/berth/broker.crt" ||
		got.KeyFile != "broker.key" {
		t.Fatalf("ci-2's TLS %+v", got)
	}
	if s := c.HostSlots(one); s.ConfigTemplate != "/etc/berth/runner.yml" || s.Labels["trusted"]["docker"] == "" {
		t.Fatalf("ci-1's slots %+v", s)
	}
	s := c.HostSlots(two)
	if s.ConfigTemplate != "/etc/berth/ci-2.yml" || s.Labels["trusted"]["docker"] != "" ||
		s.Labels["trusted"]["ubuntu-latest"] != "ubuntu-latest:host" || s.Labels["shared"]["docker"] == "" {
		t.Fatalf("ci-2's slots %+v", s)
	}
	if c.Provider.Incus.Labels["trusted"]["ubuntu-latest"] != "" {
		t.Fatal("a host's labels changed the provider's")
	}

	// Without the provider's template, each host needs its own.
	_, err = Parse([]byte(incusHosts + `
provider:
  type: incus
  incus:
    tls: {cert_file: /etc/berth/broker.crt, key_file: broker.key}
    labels: {shared: {docker: d}}
`))
	if err == nil || !strings.Contains(err.Error(), "hosts[0].config_template") || strings.Contains(err.Error(), "hosts[1]") {
		t.Fatalf("a host without a template: %v", err)
	}
	for _, tc := range []struct{ name, hosts, provider, want string }{
		{"a host's class not known", strings.Replace(incusHosts, "{trusted: {ubuntu", "{root: {ubuntu", 1),
			"{type: incus, incus: {config_template: x, tls: {cert_file: c, key_file: k}, labels: {shared: {docker: d}}}}",
			"hosts[1].labels: the classes"},
		{"a host's TLS without the client certificate", incusHosts,
			"{type: incus, incus: {config_template: x, labels: {shared: {docker: d}}}}", "hosts[1] (ci-2): tls"},
		{"no labels anywhere", strings.Replace(incusHosts, "labels: {trusted: {ubuntu-latest: \"ubuntu-latest:host\"}}", "", 1),
			"{type: incus, incus: {config_template: x, tls: {cert_file: c, key_file: k}}}", "names no class"},
		{"an SSH host's TLS", strings.ReplaceAll(strings.ReplaceAll(incusHosts, ", slots: [a]", ""),
			"    slots: [a]\n", ""), "{type: ssh}", "hosts[1]: tls, config_template and labels"},
		{"a typo in a host's TLS", strings.Replace(incusHosts, "ca_file: /etc", "cafile: /etc", 1),
			"{type: incus, incus: {config_template: x, tls: {cert_file: c, key_file: k}, labels: {shared: {docker: d}}}}",
			"cafile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.hosts + "provider: " + tc.provider + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one about %s", err, tc.want)
			}
		})
	}
}

func TestAHostsTLSForADockerEngine(t *testing.T) {
	text := strings.Replace(dockerHosts, `address: "unix:///run/docker.sock"`,
		`address: "tcp://ci-1.example.org:2376", tls: {ca_file: ca.pem, cert_file: c.pem, key_file: k.pem}`, 1)
	c, err := Parse([]byte(text + "provider: {type: docker, docker: {labels: {shared: {docker: d}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.HostTLS(c.Hosts[0]); got.CAFile != "ca.pem" || got.KeyFile != "k.pem" {
		t.Fatalf("TLS %+v", got)
	}
}

func TestCacheVolumesOfEachSlot(t *testing.T) {
	c, err := Parse([]byte(dockerHosts + `provider:
  type: docker
  docker:
    labels: {shared: {docker: d}}
    cache_volumes: {shared: "cache-{host}-{slot}", trusted: cache-trusted}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := ExpandVolume(c.Provider.Docker.CacheVolumes["shared"], "local", "b"); got != "cache-local-b" {
		t.Fatalf("expanded %q", got)
	}
	for _, tc := range []struct{ volume, want string }{
		{"cache-{class}", "placeholders"},
		{"cache {slot}", "letters, digits"},
		{"{slot}/x", "letters, digits"},
	} {
		_, err := Parse([]byte(dockerHosts + "provider: {type: docker, docker: {labels: {shared: {docker: d}}, " +
			"cache_volumes: {shared: \"" + tc.volume + "\"}}}\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one about %s", tc.volume, err, tc.want)
		}
	}
}
