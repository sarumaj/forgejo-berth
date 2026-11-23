// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// writeFile writes standard input to $1 as root:$2, 0640, at once (the runner never reads
// half of it), from the shell's own standard input: reopening it (install /dev/stdin) is
// refused in an unprivileged container, where the pipe Incus passes in belongs to a host
// user the container does not map.
const writeFile = `umask 027 && cat > "$1.tmp" && chgrp "$2" "$1.tmp" && mv -f "$1.tmp" "$1"`

// IncusCommand is a container on this host, reached through the incus command; the runner
// and the container's counters are read from its cgroup, from the host.
type IncusCommand struct {
	container string
	cgroup    string
	// run runs incus with its arguments; tests replace it.
	run func(ctx context.Context, stdin []byte, args ...string) (stdout []byte, stderr []byte, err error)
}

// NewIncusCommand makes the slot's container's instance.
func NewIncusCommand(s *Settings) *IncusCommand {
	return &IncusCommand{container: s.Container, cgroup: filepath.Join(s.CgroupRoot, "lxc.payload."+s.Container),
		run: runIncus}
}

func runIncus(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "incus", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (c *IncusCommand) must(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	out, errOut, err := c.run(ctx, stdin, args...)
	if err != nil {
		n := min(2, len(args))
		return nil, fmt.Errorf("incus %s: %v: %s", strings.Join(args[:n], " "), err, strings.TrimSpace(string(errOut)))
	}
	return out, nil
}

// Running asks Incus for the container's state.
func (c *IncusCommand) Running(ctx context.Context) (bool, error) {
	out, err := c.must(ctx, nil, "query", "/1.0/instances/"+c.container+"/state")
	if err != nil {
		return false, err
	}
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return false, err
	}
	return state.Status == "Running", nil
}

// Stop stops the container at once.
func (c *IncusCommand) Stop(ctx context.Context) error {
	_, err := c.must(ctx, nil, "stop", "--force", c.container)
	return err
}

// Restore restores the container's snapshot.
func (c *IncusCommand) Restore(ctx context.Context, snapshot string) error {
	_, err := c.must(ctx, nil, "snapshot", "restore", c.container, snapshot)
	return err
}

// Start starts the container.
func (c *IncusCommand) Start(ctx context.Context) error {
	_, err := c.must(ctx, nil, "start", c.container)
	return err
}

// Exec runs a command in the container.
func (c *IncusCommand) Exec(ctx context.Context, args ...string) ([]byte, []byte, error) {
	return c.run(ctx, nil, append([]string{"exec", c.container, "--"}, args...)...)
}

// WriteFile writes the file through a shell in the container, from its standard input.
func (c *IncusCommand) WriteFile(ctx context.Context, path, group string, data []byte) error {
	out, errOut, err := c.run(ctx, data, "exec", c.container, "--", "sh", "-c", writeFile, "write", path, group)
	if err != nil {
		return fmt.Errorf("writing %s: %w: %s%s", path, err, out, strings.TrimSpace(string(errOut)))
	}
	return nil
}

// SetCache removes the device cache, then adds the volume's, made when it is not there.
func (c *IncusCommand) SetCache(ctx context.Context, pool, volume, path string) error {
	_, _, _ = c.run(ctx, nil, "config", "device", "remove", c.container, "cache")
	if volume == "" {
		return nil
	}
	if _, _, err := c.run(ctx, nil, "storage", "volume", "show", pool, volume); err != nil {
		// Another slot's cycle may make the same volume at once: it is there either way.
		if _, createErr := c.must(ctx, nil, "storage", "volume", "create", pool, volume); createErr != nil {
			if _, _, err := c.run(ctx, nil, "storage", "volume", "show", pool, volume); err != nil {
				return createErr
			}
		}
	}
	_, err := c.must(ctx, nil, "config", "device", "add", c.container, "cache", "disk",
		"pool="+pool, "source="+volume, "path="+path)
	return err
}

// Log returns the container's log from incus info --show-log.
func (c *IncusCommand) Log(ctx context.Context) (string, error) {
	out, err := c.must(ctx, nil, "info", c.container, "--show-log")
	if err != nil {
		return "", err
	}
	text := string(out)
	if _, log, ok := strings.Cut(text, "\nLog:\n"); ok {
		return log, nil
	}
	if log, ok := strings.CutPrefix(text, "Log:\n"); ok {
		return log, nil
	}
	return "", errors.New("no log in incus info")
}

// runnerCgroup is the runner service's cgroup in the container: a system service, or one
// of a user's.
func (c *IncusCommand) runnerCgroup(unit string) string {
	candidates := []string{filepath.Join(c.cgroup, "system.slice", unit)}
	found, _ := filepath.Glob(filepath.Join(c.cgroup, "user.slice", "user-*.slice", unit))
	candidates = append(candidates, found...)
	for _, cg := range candidates {
		if st, err := os.Stat(cg); err == nil && st.IsDir() {
			return cg
		}
	}
	return ""
}

// Runner reads the runner's unit from the container's cgroups, from the host: its processes,
// a host-mode job's directory through the runner's root, and the Docker scopes beside it.
func (c *IncusCommand) Runner(unit, workDir string) (bool, bool) {
	cg := c.runnerCgroup(unit)
	if cg == "" {
		return false, false
	}
	data, err := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
	if err != nil {
		return false, false
	}
	pids := strings.Fields(string(data))
	if len(pids) == 0 {
		return false, false
	}
	if len(pids) > 1 {
		return true, true
	}
	work := filepath.Join("/proc", pids[0], "root", strings.TrimPrefix(workDir, "/"))
	if found, _ := filepath.Glob(filepath.Join(work, "*", "hostexecutor")); len(found) > 0 {
		return true, true
	}
	busy := false
	_ = filepath.WalkDir(filepath.Dir(cg), func(path string, d os.DirEntry, err error) error {
		if err != nil || busy || !d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), "docker-") && strings.HasSuffix(d.Name(), ".scope") {
			if events, err := os.ReadFile(filepath.Join(path, "cgroup.events")); err == nil &&
				strings.Contains(string(events), "populated 1") {
				busy = true
			}
		}
		return nil
	})
	return true, busy
}

// PeakMiB reads the container cgroup's memory.peak and memory.swap.peak.
func (c *IncusCommand) PeakMiB() int {
	return PeakMiB(c.cgroup)
}

// CPU reads the container's cgroup: cpu.stat's usage_usec and cpu.pressure's "some" total.
func (c *IncusCommand) CPU() (int64, int64, bool) {
	return CPUCounters(c.cgroup)
}
