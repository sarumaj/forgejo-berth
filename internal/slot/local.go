// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// Local is the slot side of one host run in the broker's own process, for a provider that
// reaches the host's containers through an API and needs nothing on the host: one reset
// cycle per slot, and the slot command's status, assign and release on the cycles' state
// files. Its cycles resume, so a restart of the broker leaves the jobs running.
type Local struct {
	slots map[string]*Settings
	// Ping tells whether the host's API answers (nil: always).
	Ping     func(ctx context.Context) error
	stopping atomic.Bool
	started  atomic.Bool
	wg       sync.WaitGroup
}

// NewLocal makes the slot side of the slots; their state directories are made.
func NewLocal(slots map[string]*Settings) (*Local, error) {
	for _, s := range slots {
		if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
			return nil, err
		}
	}
	return &Local{slots: slots}, nil
}

// Start runs a cycle for each slot, with the system the function makes for it.
func (l *Local) Start(system func(*Settings) System, log *slog.Logger) {
	l.started.Store(true)
	for _, name := range slices.Sorted(maps.Keys(l.slots)) {
		s := l.slots[name]
		c := NewCycle(s, system(s), CycleOptions{Log: log, Stopping: l.stopping.Load, Resume: true})
		l.wg.Go(c.Run)
	}
}

// Stop ends the cycles after the step each is in, and waits for them; the containers and
// their runners stay as they are, for the next start to resume.
func (l *Local) Stop() {
	l.stopping.Store(true)
	l.wg.Wait()
}

// Status reports the slots as the slot command does.
func (l *Local) Status(ctx context.Context, host string) ([]provider.Slot, error) {
	if l.Ping != nil {
		if err := l.Ping(ctx); err != nil {
			return nil, err
		}
	}
	return provider.FromStatus(host, Status(l.slots)), nil
}

// Assign checks the handoff and writes it for the slot's cycle.
func (l *Local) Assign(container string, h provider.Handoff) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = Assign(l.slots, container, data)
	clear(data)
	return err
}

// Release asks the slot's cycle to stop the assignment's runner if it has no job yet.
func (l *Local) Release(container, id string) error {
	return Release(l.slots, container, id)
}

// Cycles reports the host's cycles from their state files, as the cycles on an SSH host
// write them to their textfiles; none when they do not run here (shadow mode).
func (l *Local) Cycles(host string) []provider.CycleStats {
	if !l.started.Load() {
		return nil
	}
	var stats []provider.CycleStats
	for _, name := range slices.Sorted(maps.Keys(l.slots)) {
		var st struct {
			State            string  `json:"state"`
			Jobs             int     `json:"jobs"`
			Failures         int     `json:"failures"`
			LastResetSeconds float64 `json:"last_reset_seconds"`
		}
		if data, err := os.ReadFile(l.slots[name].path(".json")); err == nil {
			_ = json.Unmarshal(data, &st)
		}
		stats = append(stats, provider.CycleStats{Host: host, Container: name, State: st.State, Jobs: st.Jobs,
			Failures: st.Failures, LastResetSeconds: st.LastResetSeconds})
	}
	return stats
}

// Hosted is a provider whose hosts' slot sides run in this process, by host name.
type Hosted map[string]*Local

// Status reports the host's slots.
func (h Hosted) Status(ctx context.Context, host config.Host) ([]provider.Slot, error) {
	l, ok := h[host.Name]
	if !ok {
		return nil, fmt.Errorf("%s: no such host", host.Name)
	}
	return l.Status(ctx, host.Name)
}

// Assign hands a ready slot of the host a runner.
func (h Hosted) Assign(_ context.Context, host config.Host, container string, handoff provider.Handoff) error {
	l, ok := h[host.Name]
	if !ok {
		return fmt.Errorf("%s: no such host", host.Name)
	}
	return l.Assign(container, handoff)
}

// Release asks a slot of the host to stop the assignment's runner if it has no job yet.
func (h Hosted) Release(_ context.Context, host config.Host, container, assignment string) error {
	l, ok := h[host.Name]
	if !ok {
		return fmt.Errorf("%s: no such host", host.Name)
	}
	return l.Release(container, assignment)
}

// Cycles reports every host's cycles, by host and container.
func (h Hosted) Cycles() []provider.CycleStats {
	var stats []provider.CycleStats
	for _, name := range slices.Sorted(maps.Keys(h)) {
		stats = append(stats, h[name].Cycles(name)...)
	}
	return stats
}

// Stop stops every host's cycles.
func (h Hosted) Stop() {
	var wg sync.WaitGroup
	for _, l := range h {
		wg.Go(l.Stop)
	}
	wg.Wait()
}
