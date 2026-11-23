// SPDX-License-Identifier: GPL-3.0-or-later

// Package slot is the host side of forgejo-berth: the forced SSH command through which the
// broker sees and fills a host's slots, and the reset cycle that runs each slot's container
// around one job at a time.
package slot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
)

// Settings are one slot's settings: <settings dir>/<container>.yml (or .yaml, .json).
type Settings struct {
	Container string `yaml:"container" json:"container"`
	// Enabled false leaves the file out of the slots (a container kept for something else).
	Enabled *bool `yaml:"enabled" json:"enabled"`
	// Snapshot is the clean state the container is restored to before every job.
	Snapshot string `yaml:"snapshot" json:"snapshot"`
	// ConfigTemplate is the runner's configuration on the host, with @@RUNNER_UUID@@ and
	// @@RUNNER_LABELS@@; ConfigPath and TokenPath are where it and the token go in the
	// container.
	ConfigTemplate string `yaml:"config_template" json:"config_template"`
	ConfigPath     string `yaml:"config_path" json:"config_path"`
	TokenPath      string `yaml:"token_path" json:"token_path"`
	// RunnerService is the runner's systemd unit in the container (without .service), run
	// for one job (forgejo-runner one-job); RunnerUser the group that may read its files.
	RunnerService string `yaml:"runner_service" json:"runner_service"`
	RunnerUser    string `yaml:"runner_user" json:"runner_user"`
	// WorkDir is the runner's work directory in the container (a host-mode job's directory
	// there shows that it runs).
	WorkDir string `yaml:"work_dir" json:"work_dir"`
	// IdleHours refreshes a ready container that long without an assignment; JobHours cuts a
	// job that runs longer.
	IdleHours    float64 `yaml:"idle_hours" json:"idle_hours"`
	JobHours     float64 `yaml:"job_hours" json:"job_hours"`
	BootTimeout  float64 `yaml:"boot_timeout" json:"boot_timeout"`
	JournalLines int     `yaml:"journal_lines" json:"journal_lines"`
	// Interval is the time between two looks at the runner (seconds).
	Interval    float64 `yaml:"interval" json:"interval"`
	StateDir    string  `yaml:"state_dir" json:"state_dir"`
	TextfileDir string  `yaml:"textfile_dir" json:"textfile_dir"`
	CgroupRoot  string  `yaml:"cgroup_root" json:"cgroup_root"`
	// LabelTables maps a class (trusted, shared) to its label names and the runner label of
	// each: a class not listed is refused.
	LabelTables map[string]map[string]string `yaml:"labels" json:"labels"`
	// CacheVolumes maps a class to the Incus volume attached at CachePath (none: no cache).
	CacheVolumes map[string]string `yaml:"cache_volumes" json:"cache_volumes"`
	CachePool    string            `yaml:"cache_pool" json:"cache_pool"`
	CachePath    string            `yaml:"cache_path" json:"cache_path"`
}

// Labels returns the class's label table.
func (s *Settings) Labels(class string) (map[string]string, bool) {
	table, ok := s.LabelTables[class]
	return table, ok && len(table) > 0
}

// Defaults fills in what the settings leave out, as a settings file's are.
func (s *Settings) Defaults() { s.defaults() }

func (s *Settings) defaults() {
	str := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	num := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	str(&s.Snapshot, "clean")
	str(&s.ConfigPath, "/etc/forgejo-runner/config.yml")
	str(&s.TokenPath, "/etc/forgejo-runner/token")
	str(&s.RunnerService, "forgejo-runner")
	str(&s.RunnerUser, "runner")
	str(&s.WorkDir, "/var/lib/forgejo-runner/work")
	num(&s.IdleHours, 24)
	num(&s.JobHours, 4)
	num(&s.BootTimeout, 300)
	if s.JournalLines == 0 {
		s.JournalLines = 2000
	}
	num(&s.Interval, 5)
	str(&s.StateDir, "/run/forgejo-berth")
	str(&s.CgroupRoot, "/sys/fs/cgroup")
	str(&s.CachePool, "default")
	str(&s.CachePath, "/var/cache/forgejo-runner")
}

func (s *Settings) check() error {
	if !config.Name.MatchString(s.Container) {
		return fmt.Errorf("container %q: lowercase letters, digits and dashes", s.Container)
	}
	if len(s.LabelTables) == 0 {
		return fmt.Errorf("%s: labels names no class", s.Container)
	}
	for class := range s.LabelTables {
		if class != "trusted" && class != "shared" {
			return fmt.Errorf("%s: labels: the classes are trusted and shared, not %q", s.Container, class)
		}
	}
	if s.ConfigTemplate == "" {
		return fmt.Errorf("%s: config_template is required", s.Container)
	}
	return nil
}

// LoadSettings reads one slot's settings file; unknown keys are ignored, as the file may
// carry settings of other tools.
func LoadSettings(path string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Settings
	if strings.HasSuffix(path, ".json") {
		err = json.Unmarshal(data, &s)
	} else {
		err = yaml.Unmarshal(data, &s)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.defaults()
	if err := s.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// LoadSlots reads every enabled slot of the settings directory, by container. A file that
// cannot be read is skipped: the other slots go on.
func LoadSlots(dir string) map[string]*Settings {
	slots := map[string]*Settings{}
	var files []string
	for _, pattern := range []string{"*.yml", "*.yaml", "*.json"} {
		found, _ := filepath.Glob(filepath.Join(dir, pattern))
		files = append(files, found...)
	}
	slices.Sort(files)
	for _, f := range files {
		s, err := LoadSettings(f)
		if err != nil || (s.Enabled != nil && !*s.Enabled) {
			continue
		}
		slots[s.Container] = s
	}
	return slots
}

func (s *Settings) path(suffix string) string {
	return filepath.Join(s.StateDir, s.Container+suffix)
}
