// SPDX-License-Identifier: GPL-3.0-or-later

// Package config reads and checks the broker's configuration file (YAML).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Name is what host and container names must look like: they become part of an assignment id
// and of a runner's name, which the slot side checks with the same pattern.
var Name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ownerName is a Forgejo user name.
var ownerName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Config is the whole configuration file.
type Config struct {
	Forgejo  Forgejo  `yaml:"forgejo"`
	Trusted  Trusted  `yaml:"trusted"`
	Shared   Shared   `yaml:"shared"`
	Hosts    []Host   `yaml:"hosts"`
	Provider Provider `yaml:"provider"`
	Estimate Estimate `yaml:"estimate"`
	CPU      CPU      `yaml:"cpu"`
	History  History  `yaml:"history"`

	// IntervalSeconds is the time between two looks at the jobs and the slots.
	IntervalSeconds float64 `yaml:"interval_seconds"`
	// UnusedMinutes is how long a slot keeps a runner that took no job.
	UnusedMinutes float64 `yaml:"unused_minutes"`
	// StarveMinutes is how long a job that fits nowhere lets younger jobs go ahead before
	// it holds a host for itself.
	StarveMinutes float64 `yaml:"starve_minutes"`
	// RunnerPrefix starts the name of every runner the broker registers; the sweep deletes
	// leftovers with this prefix only.
	RunnerPrefix string `yaml:"runner_prefix"`
	// Listen is the address of the metrics and state endpoints ("" for none).
	Listen string `yaml:"listen"`
	// StateFile, when set, gets the state document after every look.
	StateFile string `yaml:"state_file"`
	// MetricsFile, when set, gets the metrics after every look, for node_exporter's textfile
	// collector (a .prom file in its directory) where the endpoint is not scraped.
	MetricsFile string `yaml:"metrics_file"`
	// Shadow computes and logs decisions without acting on them.
	Shadow bool `yaml:"shadow"`
	Log    Log  `yaml:"log"`
}

// Forgejo is the instance and the admin token.
type Forgejo struct {
	URL string `yaml:"url"`
	// AdminTokenFile holds a token with write:admin and read:repository.
	AdminTokenFile string `yaml:"admin_token_file"`
}

// Trusted are the owners whose jobs get runners at their own user scope.
type Trusted struct {
	// Owners maps a user name to the file of a token of theirs with write:user.
	Owners map[string]string `yaml:"owners"`
	// Labels are the label names a trusted owner's job may ask for (besides host features).
	Labels []string `yaml:"labels"`
}

// Shared is what everyone else's jobs may get: global runners.
type Shared struct {
	Labels []string `yaml:"labels"`
}

// Host is a CI host with its slots.
type Host struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
	// MemoryMiB is what the jobs of its slots may use together.
	MemoryMiB int `yaml:"memory_mib"`
	// Features are labels only this host offers; a job asking for one goes only here.
	Features []string `yaml:"features"`
	// CPUCores are the cores its slots share, for the CPU admission (cpu.overcommit).
	CPUCores float64 `yaml:"cpu_cores"`
	// MaxJobs caps the jobs running on it at once (0: no cap).
	MaxJobs int `yaml:"max_jobs"`
}

// CPU is the optional CPU admission. Every finished job's CPU use is recorded either way.
type CPU struct {
	// Overcommit, when above 0, keeps the predicted cores of a host's jobs within its
	// cpu_cores times this factor; 0 admits by memory alone.
	Overcommit float64 `yaml:"overcommit"`
	// UnknownCores are predicted for a job without a CPU history.
	UnknownCores float64 `yaml:"unknown_cores"`
}

// Provider selects and configures the slot provider.
type Provider struct {
	Type string `yaml:"type"`
	SSH  SSH    `yaml:"ssh"`
}

// SSH is the SSH provider: every host is reached as User with KeyFile, whose
// authorized_keys entry forces `forgejo-berth slot`.
type SSH struct {
	User           string `yaml:"user"`
	Port           int    `yaml:"port"`
	KeyFile        string `yaml:"key_file"`
	KnownHostsFile string `yaml:"known_hosts_file"`
	// ControlDir keeps the connections open between looks (ControlMaster); "" for none.
	ControlDir     string  `yaml:"control_dir"`
	TimeoutSeconds float64 `yaml:"timeout_seconds"`
}

// Estimate holds the rules that turn a job's history into a reservation.
type Estimate struct {
	HistoryRuns    int     `yaml:"history_runs"`
	SpikeDays      float64 `yaml:"spike_days"`
	Margin         float64 `yaml:"margin"`
	RelativeMargin float64 `yaml:"relative_margin"`
	FloorMiB       int     `yaml:"floor_mib"`
	// UnknownMiB is reserved for a job without any history; 0 gives it a host alone.
	UnknownMiB    *int    `yaml:"unknown_mib"`
	RetentionDays float64 `yaml:"retention_days"`
}

// History is where runs and kept assignments are stored.
type History struct {
	Path string `yaml:"path"`
}

// Log configures the structured log.
type Log struct {
	// Format is text or json.
	Format string `yaml:"format"`
	// Level is debug, info, warn or error.
	Level string `yaml:"level"`
}

// Unknown returns the reservation for a job without history.
func (e Estimate) Unknown() int {
	if e.UnknownMiB == nil {
		return 0
	}
	return *e.UnknownMiB
}

// Load reads, defaults and checks the file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse defaults and checks a configuration; unknown keys are errors, as a typo would
// otherwise silently keep a default.
func Parse(data []byte) (*Config, error) {
	var c Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	c.defaults()
	if err := c.check(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) defaults() {
	setFloat := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	setFloat(&c.IntervalSeconds, 10)
	setFloat(&c.UnusedMinutes, 15)
	setFloat(&c.StarveMinutes, 10)
	if c.RunnerPrefix == "" {
		c.RunnerPrefix = "berth-"
	}
	if c.Provider.Type == "" {
		c.Provider.Type = "ssh"
	}
	if c.Provider.SSH.User == "" {
		c.Provider.SSH.User = "berth"
	}
	if c.Provider.SSH.Port == 0 {
		c.Provider.SSH.Port = 22
	}
	setFloat(&c.Provider.SSH.TimeoutSeconds, 20)
	e := &c.Estimate
	if e.HistoryRuns == 0 {
		e.HistoryRuns = 5
	}
	setFloat(&e.SpikeDays, 30)
	setFloat(&e.Margin, 1.2)
	setFloat(&e.RelativeMargin, 1.5)
	if e.FloorMiB == 0 {
		e.FloorMiB = 512
	}
	if e.UnknownMiB == nil {
		unknown := 6144
		e.UnknownMiB = &unknown
	}
	setFloat(&e.RetentionDays, 180)
	setFloat(&c.CPU.UnknownCores, 1)
	if c.History.Path == "" {
		c.History.Path = "/var/lib/forgejo-berth/history.sqlite"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
}

func (c *Config) check() error {
	var problems []error
	fail := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	if u, err := url.Parse(c.Forgejo.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		fail("forgejo.url must be an http(s) URL")
	}
	if c.Forgejo.AdminTokenFile == "" {
		fail("forgejo.admin_token_file is required")
	}
	for owner, file := range c.Trusted.Owners {
		if !ownerName.MatchString(owner) {
			fail("trusted.owners: %q is not a user name", owner)
		}
		if file == "" {
			fail("trusted.owners.%s: a token file is required", owner)
		}
	}
	if len(c.Trusted.Labels) == 0 && len(c.Shared.Labels) == 0 {
		fail("trusted.labels or shared.labels must name at least one label")
	}
	if len(c.Hosts) == 0 {
		fail("hosts: at least one host is required")
	}
	seen := map[string]bool{}
	for i, h := range c.Hosts {
		if !Name.MatchString(h.Name) {
			fail("hosts[%d].name %q: lowercase letters, digits and dashes", i, h.Name)
		}
		if seen[h.Name] {
			fail("hosts[%d].name %q appears twice", i, h.Name)
		}
		seen[h.Name] = true
		if h.Address == "" || strings.ContainsAny(h.Address, " @") || strings.HasPrefix(h.Address, "-") {
			fail("hosts[%d].address %q is not a host name or address", i, h.Address)
		}
		if h.MemoryMiB <= 0 {
			fail("hosts[%d].memory_mib must be positive", i)
		}
		if h.MaxJobs < 0 || h.CPUCores < 0 {
			fail("hosts[%d]: max_jobs and cpu_cores cannot be negative", i)
		}
		if c.CPU.Overcommit > 0 && h.CPUCores <= 0 {
			fail("hosts[%d].cpu_cores is required with cpu.overcommit", i)
		}
	}
	if c.Provider.Type != "ssh" {
		fail("provider.type %q: only ssh is implemented", c.Provider.Type)
	}
	if c.Provider.SSH.Port < 1 || c.Provider.SSH.Port > 65535 {
		fail("provider.ssh.port is out of range")
	}
	if strings.HasPrefix(c.Provider.SSH.User, "-") || strings.ContainsAny(c.Provider.SSH.User, " @") {
		fail("provider.ssh.user %q is not a user name", c.Provider.SSH.User)
	}
	e := c.Estimate
	if e.HistoryRuns < 1 || e.SpikeDays < 0 || e.RetentionDays <= 0 || e.FloorMiB < 0 || e.Unknown() < 0 {
		fail("estimate: history_runs, spike_days, retention_days, floor_mib and unknown_mib must be positive")
	}
	if e.Margin < 1 || e.RelativeMargin < 1 {
		fail("estimate: margin and relative_margin must be at least 1")
	}
	if c.CPU.Overcommit < 0 || c.CPU.UnknownCores < 0 {
		fail("cpu: overcommit and unknown_cores cannot be negative")
	}
	if c.UnusedMinutes < 1 || c.UnusedMinutes > 24*60 {
		fail("unused_minutes must be between 1 and 1440")
	}
	if c.IntervalSeconds < 1 {
		fail("interval_seconds must be at least 1")
	}
	if !Name.MatchString(strings.TrimSuffix(c.RunnerPrefix, "-")) {
		fail("runner_prefix %q: lowercase letters, digits and dashes", c.RunnerPrefix)
	}
	if !slices.Contains([]string{"text", "json"}, c.Log.Format) {
		fail("log.format must be text or json")
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		fail("log.level must be debug, info, warn or error")
	}
	return errors.Join(problems...)
}

// Features returns every host's features.
func (c *Config) Features() map[string]bool {
	found := map[string]bool{}
	for _, h := range c.Hosts {
		for _, f := range h.Features {
			found[f] = true
		}
	}
	return found
}

// Host returns the host of that name.
func (c *Config) Host(name string) (Host, bool) {
	for _, h := range c.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return Host{}, false
}

// Secret returns the path of a secret file: an absolute path as it is, a relative one in
// $CREDENTIALS_DIRECTORY when that is set (systemd's LoadCredential), else as it is.
func Secret(path string) string {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if filepath.IsAbs(path) || dir == "" {
		return path
	}
	return filepath.Join(dir, path)
}

// ReadSecret reads a secret file, without surrounding white space.
func ReadSecret(path string) (string, error) {
	data, err := os.ReadFile(Secret(path))
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return value, nil
}
