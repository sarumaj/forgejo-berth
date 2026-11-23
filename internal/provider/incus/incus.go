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

// New makes the provider of the configuration's hosts. Its cycles run once made (never in
// shadow mode, which only reads the slots' state); Stop ends them and leaves the containers
// to the next start.
func New(cfg *config.Config, log *slog.Logger) (slot.Hosted, error) {
	c := cfg.Provider.Incus
	timeout := time.Duration(c.TimeoutSeconds * float64(time.Second))
	hosted := slot.Hosted{}
	for _, h := range cfg.Hosts {
		client, err := NewClient(h.Address, cfg.HostTLS(h), c.Project)
		if err != nil {
			return nil, err
		}
		// The host's own configuration template and label tables, where it has them.
		hc := c
		hc.Slots = cfg.HostSlots(h)
		slots := map[string]*slot.Settings{}
		for _, name := range h.Slots {
			slots[name] = Settings(hc, h.Name, name)
		}
		local, err := slot.NewLocal(slots)
		if err != nil {
			return nil, err
		}
		local.Ping = func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return client.Ping(ctx)
		}
		if !cfg.Shadow {
			hostLog := log.With("host", h.Name)
			local.Start(func(s *slot.Settings) slot.System {
				system := slot.NewIncusOn(s, NewInstance(client, s.Container, timeout))
				system.Out = journal{log: hostLog.With("container", s.Container)}
				return system
			}, hostLog)
		}
		hosted[h.Name] = local
	}
	return hosted, nil
}

// journal puts the runner's journal, as the cycle prints it, into the broker's log.
type journal struct{ log *slog.Logger }

func (j journal) Write(p []byte) (int, error) {
	j.log.Debug("runner", "line", string(trimNewline(p)))
	return len(p), nil
}

func trimNewline(p []byte) []byte {
	if n := len(p); n > 0 && p[n-1] == '\n' {
		return p[:n-1]
	}
	return p
}
