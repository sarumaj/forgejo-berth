// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// State is what the broker publishes after every look (the /state endpoint, state_file).
// It holds no token.
type State struct {
	LastTick    int64                `json:"last_tick"`
	Shadow      bool                 `json:"shadow"`
	Waiting     int                  `json:"waiting"`
	Unservable  int                  `json:"unservable"`
	Hosts       map[string]HostState `json:"hosts"`
	Assignments []AssignmentState    `json:"assignments"`
	Counts      map[string]int       `json:"counts"`
	Estimates   map[string]int       `json:"estimates"`
	JobsLearned int                  `json:"jobs_learned"`
}

// HostState is a host's part of the state.
type HostState struct {
	Reachable   bool `json:"reachable"`
	ReservedMiB int  `json:"reserved_mib"`
}

// AssignmentState is an assignment as the state shows it.
type AssignmentState struct {
	ID         string   `json:"id"`
	Host       string   `json:"host"`
	Container  string   `json:"container"`
	Scope      string   `json:"scope"`
	Labels     []string `json:"labels"`
	ReserveMiB int      `json:"reserve_mib"`
	Exclusive  bool     `json:"exclusive"`
	Since      int64    `json:"since"`
}

var estimateSources = []string{"job", "family", "repo", "none"}

func (b *Broker) publish(now float64, waiting, unservable int, sources map[string]int) {
	known, err := b.history.Known()
	if err != nil {
		b.log.Error("the history's jobs were not counted", "error", err)
	}
	s := State{
		LastTick:    int64(now),
		Shadow:      b.shadow,
		Waiting:     waiting,
		Unservable:  unservable,
		Hosts:       map[string]HostState{},
		Assignments: []AssignmentState{},
		Counts:      maps.Clone(b.counts),
		Estimates:   map[string]int{},
		JobsLearned: known,
	}
	for _, src := range estimateSources {
		s.Estimates[src] = sources[src]
	}
	for _, h := range b.cfg.Hosts {
		s.Hosts[h.Name] = HostState{Reachable: b.reachable[h.Name]}
	}
	for _, a := range b.assignments() {
		if hs, ok := s.Hosts[a.Host]; ok {
			hs.ReservedMiB += a.ReserveMiB
			s.Hosts[a.Host] = hs
		}
		s.Assignments = append(s.Assignments, AssignmentState{
			ID: a.ID, Host: a.Host, Container: a.Container, Scope: a.Scope, Labels: a.Labels,
			ReserveMiB: a.ReserveMiB, Exclusive: a.Exclusive, Since: int64(a.Since),
		})
	}
	b.mu.Lock()
	b.state = s
	b.mu.Unlock()
}

// Snapshot returns the state of the last look.
func (b *Broker) Snapshot() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Healthy tells whether the last look is younger than three intervals.
func (b *Broker) Healthy(now time.Time) bool {
	s := b.Snapshot()
	return s.LastTick > 0 && float64(now.Unix()-s.LastTick) < 3*b.cfg.IntervalSeconds+1
}

// Metrics returns the state in Prometheus' text format.
func (s State) Metrics() string {
	var w strings.Builder
	metric := func(name, kind, help string) {
		fmt.Fprintf(&w, "# HELP forgejo_berth_%s %s\n# TYPE forgejo_berth_%s %s\n", name, help, name, kind)
	}
	metric("last_tick_seconds", "gauge", "When the broker last looked at the jobs and slots.")
	fmt.Fprintf(&w, "forgejo_berth_last_tick_seconds %d\n", s.LastTick)
	metric("shadow", "gauge", "Whether the broker only computes and logs its decisions.")
	fmt.Fprintf(&w, "forgejo_berth_shadow %d\n", btoi(s.Shadow))
	metric("waiting_jobs", "gauge", "Jobs waiting for a runner (all), and those no runner may take.")
	fmt.Fprintf(&w, "forgejo_berth_waiting_jobs{kind=\"all\"} %d\n", s.Waiting)
	fmt.Fprintf(&w, "forgejo_berth_waiting_jobs{kind=\"unservable\"} %d\n", s.Unservable)
	hosts := slices.Sorted(maps.Keys(s.Hosts))
	metric("host_reachable", "gauge", "Whether the host answered the last status.")
	for _, h := range hosts {
		fmt.Fprintf(&w, "forgejo_berth_host_reachable{host=%q} %d\n", h, btoi(s.Hosts[h].Reachable))
	}
	metric("reserved_mib", "gauge", "Memory the broker reserved on a host for its runners.")
	for _, h := range hosts {
		fmt.Fprintf(&w, "forgejo_berth_reserved_mib{host=%q} %d\n", h, s.Hosts[h].ReservedMiB)
	}
	metric("events_total", "counter", "Runners assigned, jobs learned or not, runners unused, failures.")
	for _, e := range slices.Sorted(maps.Keys(s.Counts)) {
		fmt.Fprintf(&w, "forgejo_berth_events_total{event=%q} %d\n", e, s.Counts[e])
	}
	metric("estimates", "gauge", "Waiting jobs by where their memory estimate came from.")
	for _, src := range estimateSources {
		fmt.Fprintf(&w, "forgejo_berth_estimates{source=%q} %d\n", src, s.Estimates[src])
	}
	metric("jobs_learned", "gauge", "Jobs with a memory history.")
	fmt.Fprintf(&w, "forgejo_berth_jobs_learned %d\n", s.JobsLearned)
	return w.String()
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

// WriteFile writes data to path at once: readers never see half of it.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
