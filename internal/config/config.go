// SPDX-License-Identifier: GPL-3.0-or-later

// Package config reads and checks the broker's configuration file (YAML).
package config

import (
	"errors"
	"fmt"
	"maps"
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

// volumeName is what a cache volume's name must look like once its placeholders are
// expanded: a name both Docker and Incus take.
var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ExpandVolume returns the name of a slot's cache volume: {host} is the host's name, {slot}
// the slot's (an Incus slot's container), so that each slot can have a cache of its own.
func ExpandVolume(name, host, slot string) string {
	return strings.NewReplacer("{host}", host, "{slot}", slot).Replace(name)
}

// CheckVolume tells whether an expanded cache volume name is one: a placeholder left over is
// not known.
func CheckVolume(name string) error {
	if strings.ContainsAny(name, "{}") {
		return fmt.Errorf("cache volume %q: the placeholders are {host} and {slot}", name)
	}
	if !volumeName.MatchString(name) {
		return fmt.Errorf("cache volume %q: letters, digits, '_', '.' and '-'", name)
	}
	return nil
}

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
	Backoff  Backoff  `yaml:"backoff"`
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
	// Slots names the host's slots for the docker and incus providers, whose hosts run no
	// slot side of their own (an SSH host lists its slots itself).
	Slots []string `yaml:"slots"`
	// TLS, ConfigTemplate and Labels are the docker and incus providers' settings of the
	// same names for this host's slots, where it differs from the others: a server with a
	// certificate of its own, a runner configuration or a label table of its own. A TLS
	// field left out, and a class the host's labels do not list, are the provider's.
	TLS            *TLS                         `yaml:"tls"`
	ConfigTemplate string                       `yaml:"config_template"`
	Labels         map[string]map[string]string `yaml:"labels"`
}

// HostTLS is the TLS the provider reaches the host with: the provider's, each field the
// host sets in place of the provider's.
func (c *Config) HostTLS(h Host) TLS {
	t := c.Provider.Docker.TLS
	if c.Provider.Type == ProviderIncus {
		t = c.Provider.Incus.TLS
	}
	if h.TLS != nil {
		for _, f := range []struct {
			to   *string
			from string
		}{{&t.CAFile, h.TLS.CAFile},
			{&t.CertFile, h.TLS.CertFile}, {&t.KeyFile, h.TLS.KeyFile}} {
			if f.from != "" {
				*f.to = f.from
			}
		}
	}
	return t
}

// HostSlots are the slots' settings on the host: the provider's, with the host's
// configuration template and label tables in place of the provider's (per class).
func (c *Config) HostSlots(h Host) Slots {
	s := c.Provider.Docker.Slots
	if c.Provider.Type == ProviderIncus {
		s = c.Provider.Incus.Slots
	}
	if h.ConfigTemplate != "" {
		s.ConfigTemplate = h.ConfigTemplate
	}
	if len(h.Labels) > 0 {
		labels := make(map[string]map[string]string, len(s.Labels)+len(h.Labels))
		maps.Copy(labels, s.Labels)
		maps.Copy(labels, h.Labels)
		s.Labels = labels
	}
	return s
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
	Type   string `yaml:"type"`
	SSH    SSH    `yaml:"ssh"`
	Docker Docker `yaml:"docker"`
	Incus  Incus  `yaml:"incus"`
}

// The provider types.
const (
	ProviderSSH    = "ssh"
	ProviderDocker = "docker"
	ProviderIncus  = "incus"
)

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

// TLS is how the broker reaches a remote API: the server's CA (or its own certificate),
// and the broker's client certificate and key. The key is a secret.
type TLS struct {
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Set tells whether any of it is configured.
func (t TLS) Set() bool { return t.CAFile != "" || t.CertFile != "" || t.KeyFile != "" }

// Slots are the settings of the slots whose reset cycles run in the broker (the docker and
// incus providers), the same for every slot of every host.
type Slots struct {
	// StateDir keeps each slot's state, per host (default: slots/ beside the history).
	StateDir string `yaml:"state_dir"`
	// Labels maps a class (trusted, shared) to its label names and the runner label of each.
	Labels map[string]map[string]string `yaml:"labels"`
	// ConfigTemplate is the runner's configuration, with @@RUNNER_UUID@@ and
	// @@RUNNER_LABELS@@, on the broker's host.
	ConfigTemplate string `yaml:"config_template"`
	// CacheVolumes maps a class to a volume mounted at CachePath (none: no cache). {host}
	// and {slot} in a name are the slot's, for a volume of each slot's own; a volume that
	// does not exist is made at its first use.
	CacheVolumes map[string]string `yaml:"cache_volumes"`
	CachePath    string            `yaml:"cache_path"`
	IdleHours    float64           `yaml:"idle_hours"`
	JobHours     float64           `yaml:"job_hours"`
	BootTimeout  float64           `yaml:"boot_timeout"`
	// TimeoutSeconds bounds one API request; a long one (a pull, a restore) gets more.
	TimeoutSeconds float64 `yaml:"timeout_seconds"`
}

// The docker provider's job engines.
const (
	EngineDinD       = "dind"
	EngineNone       = "none"
	EngineHostSocket = "host-socket"
)

// Docker is the docker provider: each host is a Docker Engine (or Podman's Docker API), each
// slot a runner container made for one job from Image, removed after it.
type Docker struct {
	Slots `yaml:",inline"`
	TLS   TLS `yaml:"tls"`
	// Image is the runner's image: sh and forgejo-runner on its PATH.
	Image string `yaml:"image"`
	// ForgejoURL is the instance as the runners reach it (default: forgejo.url).
	ForgejoURL string `yaml:"forgejo_url"`
	// JobEngine is what a job's containers run on: dind, a Docker of the slot's own beside
	// the runner; none, no engine (host-mode labels only); host-socket, the engine the slots
	// run on, which gives every job the control of that engine.
	JobEngine string `yaml:"job_engine"`
	// DinDImage is the image of the slot's own Docker, DinDArgs more arguments for its
	// dockerd (a registry mirror, say).
	DinDImage string   `yaml:"dind_image"`
	DinDArgs  []string `yaml:"dind_args"`
	// HostSocket is the engine's socket on its host, for host-socket.
	HostSocket string `yaml:"host_socket"`
	// User runs the runner's container as another user than its image's (uid[:gid]).
	User string `yaml:"user"`
	// Network is a network the runner and its engine join (default: the engine's default).
	Network string `yaml:"network"`
}

// Incus is the incus provider: each host is an Incus server reached through its API, each
// slot one of its containers, restored from a snapshot before every job.
type Incus struct {
	Slots `yaml:",inline"`
	TLS   TLS `yaml:"tls"`
	// Project is the Incus project of the slots' containers.
	Project string `yaml:"project"`
	// Snapshot is the clean state each container is restored to.
	Snapshot      string `yaml:"snapshot"`
	ConfigPath    string `yaml:"config_path"`
	TokenPath     string `yaml:"token_path"`
	RunnerService string `yaml:"runner_service"`
	RunnerUser    string `yaml:"runner_user"`
	WorkDir       string `yaml:"work_dir"`
	CachePool     string `yaml:"cache_pool"`
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

// Backoff holds back a waiting job whose runners keep going unused: Forgejo lists it as
// waiting but hands it to no runner (a run held by its concurrency group, say), which berth
// cannot see through the API it uses.
type Backoff struct {
	// UnusedRuns is how many runners in a row that could take the job must go unused, the
	// job waiting all the while, before it is held back; 0 turns the back-off off.
	UnusedRuns *int `yaml:"unused_runs"`
	// BaseMinutes is the first back-off (default: unused_minutes); each further runner gone
	// unused doubles it, up to MaxMinutes.
	BaseMinutes float64 `yaml:"base_minutes"`
	MaxMinutes  float64 `yaml:"max_minutes"`
}

// Runs returns how many unused runners in a row hold a job back (0: never).
func (b Backoff) Runs() int {
	if b.UnusedRuns == nil {
		return 0
	}
	return *b.UnusedRuns
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

func setString(v *string, d string) {
	if *v == "" {
		*v = d
	}
}

// slotDefaults fills in the in-broker slots' settings.
func (c *Config) slotDefaults(s *Slots) {
	setString(&s.CachePath, "/var/cache/forgejo-runner")
	for v, d := range map[*float64]float64{&s.IdleHours: 24, &s.JobHours: 4, &s.BootTimeout: 300,
		&s.TimeoutSeconds: 20} {
		if *v == 0 {
			*v = d
		}
	}
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
		c.Provider.Type = ProviderSSH
	}
	c.slotDefaults(&c.Provider.Docker.Slots)
	c.slotDefaults(&c.Provider.Incus.Slots)
	d := &c.Provider.Docker
	setString(&d.Image, "data.forgejo.org/forgejo/runner:13")
	setString(&d.ForgejoURL, c.Forgejo.URL)
	setString(&d.JobEngine, EngineDinD)
	setString(&d.DinDImage, "docker.io/library/docker:29-dind")
	setString(&d.HostSocket, "/var/run/docker.sock")
	i := &c.Provider.Incus
	setString(&i.Project, "default")
	setString(&i.Snapshot, "clean")
	setString(&i.ConfigPath, "/etc/forgejo-runner/config.yml")
	setString(&i.TokenPath, "/etc/forgejo-runner/token")
	setString(&i.RunnerService, "forgejo-runner")
	setString(&i.RunnerUser, "runner")
	setString(&i.WorkDir, "/var/lib/forgejo-runner/work")
	setString(&i.CachePool, "default")
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
	if c.Backoff.UnusedRuns == nil {
		runs := 3
		c.Backoff.UnusedRuns = &runs
	}
	setFloat(&c.Backoff.BaseMinutes, c.UnusedMinutes)
	setFloat(&c.Backoff.MaxMinutes, 240)
	if c.History.Path == "" {
		c.History.Path = "/var/lib/forgejo-berth/history.sqlite"
	}
	setString(&c.Provider.Docker.StateDir, filepath.Join(filepath.Dir(c.History.Path), "slots"))
	setString(&c.Provider.Incus.StateDir, filepath.Join(filepath.Dir(c.History.Path), "slots"))
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
	c.checkProvider(fail)
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
	if bo := c.Backoff; bo.Runs() < 0 || bo.BaseMinutes < 0 || bo.MaxMinutes < bo.BaseMinutes {
		fail("backoff: unused_runs and base_minutes cannot be negative, max_minutes not below base_minutes")
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

// checkProvider checks the provider's settings and the hosts' addresses and slots for it.
func (c *Config) checkProvider(fail func(string, ...any)) {
	p := c.Provider
	var slots Slots
	switch p.Type {
	case ProviderSSH:
		for i, h := range c.Hosts {
			if len(h.Slots) > 0 {
				fail("hosts[%d].slots: an SSH host lists its slots itself", i)
			}
			if h.TLS != nil || h.ConfigTemplate != "" || len(h.Labels) > 0 {
				fail("hosts[%d]: tls, config_template and labels are the docker and incus providers'; "+
					"an SSH host has its own in its slot files", i)
			}
		}
		return
	case ProviderDocker:
		slots = p.Docker.Slots
		if !slices.Contains([]string{EngineDinD, EngineNone, EngineHostSocket}, p.Docker.JobEngine) {
			fail("provider.docker.job_engine must be dind, none or host-socket")
		}
		if u, err := url.Parse(p.Docker.ForgejoURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			fail("provider.docker.forgejo_url must be an http(s) URL")
		}
		if p.Docker.Image == "" || strings.HasPrefix(p.Docker.Image, "-") {
			fail("provider.docker.image is required")
		}
		if !filepath.IsAbs(p.Docker.HostSocket) {
			fail("provider.docker.host_socket must be an absolute path")
		}
	case ProviderIncus:
		slots = p.Incus.Slots
		for i, h := range c.Hosts {
			if slots.ConfigTemplate == "" && h.ConfigTemplate == "" {
				fail("provider.incus.config_template is required (or hosts[%d].config_template)", i)
			}
		}
		for name, v := range map[string]string{"config_path": p.Incus.ConfigPath, "token_path": p.Incus.TokenPath,
			"work_dir": p.Incus.WorkDir} {
			if !filepath.IsAbs(v) {
				fail("provider.incus.%s must be an absolute path", name)
			}
		}
		if strings.ContainsAny(p.Incus.Project, "/?&#% ") {
			fail("provider.incus.project %q is not a project name", p.Incus.Project)
		}
	default:
		fail("provider.type %q: ssh, docker or incus", p.Type)
		return
	}
	where := "provider." + p.Type
	checkLabels(fail, where, slots.Labels)
	for i, h := range c.Hosts {
		checkLabels(fail, fmt.Sprintf("hosts[%d]", i), h.Labels)
		if len(c.HostSlots(h).Labels) == 0 {
			fail("%s.labels names no class (nor hosts[%d].labels)", where, i)
		}
	}
	for class, volume := range slots.CacheVolumes {
		if class != "trusted" && class != "shared" {
			fail("%s.cache_volumes: the classes are trusted and shared, not %q", where, class)
		}
		if volume == "" {
			continue
		}
		problem := CheckVolume(ExpandVolume(volume, "host", "slot"))
		for _, h := range c.Hosts {
			for _, s := range h.Slots {
				if problem == nil {
					problem = CheckVolume(ExpandVolume(volume, h.Name, s))
				}
			}
		}
		if problem != nil {
			fail("%s.cache_volumes.%s: %v", where, class, problem)
		}
	}
	if !filepath.IsAbs(slots.CachePath) {
		fail("%s.cache_path must be an absolute path", where)
	}
	if slots.IdleHours < 0 || slots.JobHours < 0 || slots.BootTimeout < 0 || slots.TimeoutSeconds < 0 {
		fail("%s: idle_hours, job_hours, boot_timeout and timeout_seconds cannot be negative", where)
	}
	for i, h := range c.Hosts {
		secure := c.HostTLS(h)
		if secure.Set() && (secure.CertFile == "" || secure.KeyFile == "") {
			fail("hosts[%d] (%s): tls: cert_file and key_file go together", i, h.Name)
		}
		if len(h.Slots) == 0 {
			fail("hosts[%d].slots: a %s host names its slots", i, p.Type)
		}
		seen := map[string]bool{}
		for _, s := range h.Slots {
			// The slot's name is part of an assignment id, and of the container's name
			// (<runner_prefix><host>-<slot>-docker for the docker provider).
			if !Name.MatchString(s) || len(h.Name)+len(s) > 48 {
				fail("hosts[%d].slots: %q: lowercase letters, digits and dashes, short", i, s)
			}
			if seen[s] {
				fail("hosts[%d].slots: %q appears twice", i, s)
			}
			seen[s] = true
		}
		u, err := url.Parse(h.Address)
		switch {
		case err != nil:
			fail("hosts[%d].address %q is not a URL", i, h.Address)
		case u.Scheme == "unix" && filepath.IsAbs(u.Path):
		case p.Type == ProviderDocker && u.Scheme == "tcp" && u.Host != "":
			if !secure.Set() {
				fail("hosts[%d].address %q: a tcp:// engine needs provider.docker.tls (or the host's tls)", i, h.Address)
			}
		case p.Type == ProviderIncus && u.Scheme == "https" && u.Host != "":
			if secure.CertFile == "" {
				fail("hosts[%d].address %q: an https:// server needs provider.incus.tls (or the host's tls)", i, h.Address)
			}
		default:
			fail("hosts[%d].address %q: unix:///path or %s", i, h.Address,
				map[string]string{ProviderDocker: "tcp://host:port", ProviderIncus: "https://host:port"}[p.Type])
		}
	}
}

// checkLabels checks label tables: the classes are trusted and shared, none empty.
func checkLabels(fail func(string, ...any), where string, labels map[string]map[string]string) {
	for class, table := range labels {
		if class != "trusted" && class != "shared" {
			fail("%s.labels: the classes are trusted and shared, not %q", where, class)
		}
		if len(table) == 0 {
			fail("%s.labels.%s is empty", where, class)
		}
	}
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
