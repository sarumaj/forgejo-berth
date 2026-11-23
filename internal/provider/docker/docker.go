// SPDX-License-Identifier: GPL-3.0-or-later

package docker

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
)

// Settings are the cycle's settings of a host's slot: its state in <state_dir>/<host>/, its
// cache volumes' names expanded for it.
func Settings(s config.Slots, host, name string) *slot.Settings {
	settings := &slot.Settings{Container: name, StateDir: filepath.Join(s.StateDir, host),
		LabelTables: s.Labels, ConfigTemplate: s.ConfigTemplate, CacheVolumes: slot.ExpandVolumes(s.CacheVolumes, host, name),
		CachePath: s.CachePath, IdleHours: s.IdleHours, JobHours: s.JobHours, BootTimeout: s.BootTimeout}
	settings.Defaults()
	return settings
}

// New makes the provider of the configuration's hosts. Its cycles run once started (never
// in shadow mode, which only reads the slots' state); Stop ends them and leaves the
// containers to the next start.
func New(cfg *config.Config, log *slog.Logger) (slot.Hosted, error) {
	d := cfg.Provider.Docker
	hosted := slot.Hosted{}
	for _, h := range cfg.Hosts {
		engine, err := NewEngine(h.Address, cfg.HostTLS(h))
		if err != nil {
			return nil, err
		}
		slots := map[string]*slot.Settings{}
		for _, name := range h.Slots {
			slots[name] = Settings(cfg.HostSlots(h), h.Name, name)
		}
		local, err := slot.NewLocal(slots)
		if err != nil {
			return nil, err
		}
		local.Ping = func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, durationSeconds(d.TimeoutSeconds))
			defer cancel()
			return engine.Ping(ctx)
		}
		if !cfg.Shadow {
			prefix := cfg.RunnerPrefix + h.Name + "-"
			local.Start(func(s *slot.Settings) slot.System {
				return NewSystem(engine, d, s, h.Name, prefix+s.Container, log)
			}, log.With("host", h.Name))
		}
		hosted[h.Name] = local
	}
	return hosted, nil
}

func durationSeconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
