// SPDX-License-Identifier: GPL-3.0-or-later

// Package provider defines the slot provider interface, through which the broker sees and
// fills the container slots of its hosts, and its implementations.
package provider

import (
	"context"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
)

// Slot states, as the slot side reports them.
const (
	Ready     = "ready"
	Pending   = "pending"
	Assigned  = "assigned"
	Running   = "running"
	Resetting = "resetting"
	Failed    = "failed"
	Stopped   = "stopped"
)

// Slot is a container slot as its host reports it.
type Slot struct {
	Host      string
	Container string
	State     string
	// Assignment is the one the slot holds now ("" for none).
	Assignment string
	// Finished is the last assignment the slot finished, with the task its runner ran, the
	// most memory the container used (swap included) and the job's duration.
	Finished string
	Task     int64
	PeakMiB  int
	Seconds  float64
	// Unused is set when the finished assignment's runner took no job.
	Unused bool
	// CPUCores is the job's average CPU use in cores, CPUPressure the share of its run time
	// some of its tasks waited for a CPU (0: not measured).
	CPUCores    float64
	CPUPressure float64
}

// Handoff is what a slot gets for a runner: never logged, never kept.
type Handoff struct {
	Assignment    string   `json:"assignment"`
	UUID          string   `json:"uuid"`
	Token         string   `json:"token"`
	Labels        []string `json:"labels"`
	Class         string   `json:"class"`
	UnusedMinutes float64  `json:"unused_minutes"`
}

// String keeps the token out of anything that formats a Handoff.
func (h Handoff) String() string {
	return "handoff " + h.Assignment
}

// GoString keeps the token out of %#v too.
func (h Handoff) GoString() string {
	return h.String()
}

// Provider is how the broker reaches its slots.
type Provider interface {
	// Status lists the host's slots; an error means the host did not answer.
	Status(ctx context.Context, host config.Host) ([]Slot, error)
	// Assign hands a ready slot a runner; an error means the slot refused it.
	Assign(ctx context.Context, host config.Host, container string, h Handoff) error
	// Release asks the slot to stop the assignment's runner if it has no job yet.
	Release(ctx context.Context, host config.Host, container, assignment string) error
}
