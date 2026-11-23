// SPDX-License-Identifier: GPL-3.0-or-later

package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

// The shapes of an assignment's fields, checked on the slot side before anything is written.
var (
	AssignmentID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
	RunnerUUID   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// RunnerToken: Forgejo's runner tokens are 40 hex digits today; any token of safe
	// characters is taken.
	RunnerToken = regexp.MustCompile(`^[0-9A-Za-z_-]{20,256}$`)
)

// Classes are the slot classes: an owner's runners are trusted, global ones shared.
var Classes = []string{"trusted", "shared"}

// SlotClasses is what a slot allows: per class, the runner label of each label name, and
// whether the class is configured at all.
type SlotClasses interface {
	// Labels returns the class's label table, false when the slot has no such class.
	Labels(class string) (map[string]string, bool)
}

// Parsed is an assignment checked against a slot's classes, its labels mapped.
type Parsed struct {
	ID            string
	UUID          string
	Token         string
	Class         string
	Labels        []string
	UnusedMinutes float64
}

// ParseHandoff checks an assignment as a slot receives it, and maps its label names to the
// runner labels of its class: a shared runner never gets a trusted label, whatever it was
// handed.
func ParseHandoff(data []byte, slot SlotClasses) (Parsed, error) {
	var h struct {
		Assignment    string   `json:"assignment"`
		UUID          string   `json:"uuid"`
		Token         string   `json:"token"`
		Labels        []string `json:"labels"`
		Class         string   `json:"class"`
		UnusedMinutes *float64 `json:"unused_minutes"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return Parsed{}, fmt.Errorf("not an assignment object: %w", err)
	}
	table, ok := slot.Labels(h.Class)
	if !ok || !slices.Contains(Classes, h.Class) {
		return Parsed{}, fmt.Errorf("no such class: %q", h.Class)
	}
	if len(h.Labels) == 0 {
		return Parsed{}, fmt.Errorf("no labels")
	}
	mapped := make([]string, 0, len(h.Labels))
	for _, name := range h.Labels {
		label, ok := table[name]
		if !ok {
			return Parsed{}, fmt.Errorf("labels %v are not all of the class %s", h.Labels, h.Class)
		}
		mapped = append(mapped, label)
	}
	for field, check := range map[string]struct {
		value   string
		pattern *regexp.Regexp
	}{
		"assignment": {h.Assignment, AssignmentID},
		"uuid":       {h.UUID, RunnerUUID},
		"token":      {h.Token, RunnerToken},
	} {
		if !check.pattern.MatchString(check.value) {
			return Parsed{}, fmt.Errorf("the assignment's %s is not valid", field)
		}
	}
	minutes := 15.0
	if h.UnusedMinutes != nil {
		minutes = *h.UnusedMinutes
	}
	if minutes < 1 || minutes > 24*60 {
		return Parsed{}, fmt.Errorf("unused_minutes is out of range")
	}
	return Parsed{ID: h.Assignment, UUID: h.UUID, Token: h.Token, Class: h.Class, Labels: mapped,
		UnusedMinutes: minutes}, nil
}
