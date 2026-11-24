// SPDX-License-Identifier: GPL-3.0-or-later

package incus

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
)

// Settings are the cycle's settings of a host's slot (a container of the server): its
// state in <state_dir>/<host>/, its cache volumes' names expanded for it.
func Settings(c config.Incus, host, container string) *slot.Settings {
	s := &slot.Settings{Container: container, StateDir: filepath.Join(c.StateDir, host),
		Snapshot: c.Snapshot, ConfigTemplate: c.ConfigTemplate, ConfigPath: c.ConfigPath,
		TokenPath: c.TokenPath, RunnerService: c.RunnerService, RunnerUser: c.RunnerUser,
		WorkDir: c.WorkDir, IdleHours: c.IdleHours, JobHours: c.JobHours, BootTimeout: c.BootTimeout,
		LabelTables: c.Labels, CacheVolumes: slot.ExpandVolumes(c.CacheVolumes, host, container), CachePool: c.CachePool, CachePath: c.CachePath}
	s.Defaults()
	return s
}

// Slots are the hosts' slot sides, their cycles not started: what pausing and resuming a
// slot changes (their state directories).
func Slots(cfg *config.Config) (slot.Hosted, error) {
	hosted := slot.Hosted{}
	for _, h := range cfg.Hosts {
		// The host's own configuration template and label tables, where it has them.
		hc := cfg.Provider.Incus
		hc.Slots = cfg.HostSlots(h)
		slots := map[string]*slot.Settings{}
		for _, name := range h.Slots {
			slots[name] = Settings(hc, h.Name, name)
		}
		local, err := slot.NewLocal(slots)
		if err != nil {
			return nil, err
		}
		hosted[h.Name] = local
	}
	return hosted, nil
}

// New makes the provider of the configuration's hosts. Its cycles run once made (never in
// shadow mode, which only reads the slots' state); Stop ends them and leaves the containers
// to the next start.
func New(cfg *config.Config, log *slog.Logger) (slot.Hosted, error) {
	c := cfg.Provider.Incus
	timeout := time.Duration(c.TimeoutSeconds * float64(time.Second))
	hosted, err := Slots(cfg)
	if err != nil {
		return nil, err
	}
	for _, h := range cfg.Hosts {
		client, err := NewClient(h.Address, cfg.HostTLS(h), c.Project)
		if err != nil {
			return nil, err
		}
		local := hosted[h.Name]
		local.Ping = func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return client.Ping(ctx)
		}
		if !cfg.Shadow {
			hostLog := log.With("host", h.Name)
			local.Start(func(s *slot.Settings) slot.System {
				system := slot.NewIncusOn(s, NewInstance(client, s.Container, timeout))
				// The runner's journal in the broker's log, as an SSH host's cycle has it in
				// its own: a runner that fails to start is seen without debug logging.
				system.Log = hostLog.With("container", s.Container)
				return system
			}, hostLog)
		}
		hosted[h.Name] = local
	}
	return hosted, nil
}
