// SPDX-License-Identifier: GPL-3.0-or-later

package provider

import (
	"fmt"
	"strings"
)

// CycleStats is a slot's reset cycle as its metrics show it.
type CycleStats struct {
	// Host is the slot's host for the cycles the broker runs ("" on the host itself).
	Host             string  `json:"host,omitempty"`
	Container        string  `json:"container"`
	State            string  `json:"state"`
	Jobs             int     `json:"jobs"`
	Failures         int     `json:"failures"`
	LastResetSeconds float64 `json:"last_reset_seconds"`
}

// CycleStates are the states the forgejo_berth_cycle_state metric has a series of.
var CycleStates = []string{Resetting, Ready, Assigned, Running, Failed, Stopped, Paused}

// CycleReporter is a provider that runs the slots' cycles itself and reports them.
type CycleReporter interface {
	// Cycles returns the cycles' stats, by host and container.
	Cycles() []CycleStats
}

// CycleMetrics returns the cycles' metrics in Prometheus' text format: those a cycle on its
// host writes to its textfile, with a host label for the broker's cycles.
func CycleMetrics(cycles []CycleStats) string {
	if len(cycles) == 0 {
		return ""
	}
	labels := func(c CycleStats) string {
		if c.Host == "" {
			return fmt.Sprintf("container=%q", c.Container)
		}
		return fmt.Sprintf("host=%q,container=%q", c.Host, c.Container)
	}
	var b strings.Builder
	metric := func(name, kind, help string) {
		fmt.Fprintf(&b, "# HELP forgejo_berth_cycle_%s %s\n# TYPE forgejo_berth_cycle_%s %s\n", name, help, name, kind)
	}
	metric("state", "gauge", "The slot's state in its reset cycle (1: the current one).")
	for _, c := range cycles {
		for _, each := range CycleStates {
			current := 0
			if each == c.State {
				current = 1
			}
			fmt.Fprintf(&b, "forgejo_berth_cycle_state{%s,state=%q} %d\n", labels(c), each, current)
		}
	}
	metric("jobs_total", "counter", "Jobs the slot ran since the cycle started.")
	for _, c := range cycles {
		fmt.Fprintf(&b, "forgejo_berth_cycle_jobs_total{%s} %d\n", labels(c), c.Jobs)
	}
	metric("failures", "gauge", "Resets or runner starts that failed in a row.")
	for _, c := range cycles {
		fmt.Fprintf(&b, "forgejo_berth_cycle_failures{%s} %d\n", labels(c), c.Failures)
	}
	metric("last_reset_seconds", "gauge", "How long the last reset took.")
	for _, c := range cycles {
		fmt.Fprintf(&b, "forgejo_berth_cycle_last_reset_seconds{%s} %.1f\n", labels(c), c.LastResetSeconds)
	}
	return b.String()
}
