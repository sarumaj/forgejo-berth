// SPDX-License-Identifier: GPL-3.0-or-later

package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
)

// runnerScript is the runner's look from inside the container, as the incus command's
// instance reads it from the host: the unit's cgroup (a system service, or one of a
// user's), its processes, a host-mode job's directory, a populated Docker scope beside it.
// It prints inactive, active or busy.
const runnerScript = `unit=$1 work=$2 cg=/sys/fs/cgroup
d=$cg/system.slice/$unit
if [ ! -d "$d" ]; then
  for x in "$cg"/user.slice/user-*.slice/"$unit"; do [ -d "$x" ] && d=$x && break; done
fi
[ -d "$d" ] || { echo inactive; exit 0; }
n=$(grep -c . "$d/cgroup.procs" 2>/dev/null)
[ "${n:-0}" -gt 0 ] || { echo inactive; exit 0; }
[ "$n" -gt 1 ] && { echo busy; exit 0; }
for x in "$work"/*/hostexecutor; do [ -e "$x" ] && { echo busy; exit 0; }; done
for e in $(find "$(dirname "$d")" -path '*/docker-*.scope/cgroup.events' 2>/dev/null); do
  grep -q 'populated 1' "$e" && { echo busy; exit 0; }
done
echo active`

// finishFile moves a file written as path.tmp into place as root:$2, 0640.
const finishFile = `chgrp "$2" "$1.tmp" && chmod 0640 "$1.tmp" && mv -f "$1.tmp" "$1"`

// lookFor is how long a look at the runner is kept: the cycle asks whether it is active
// and then whether it is busy, one exec answers both.
const lookFor = 2 * time.Second

// Instance is a container of an Incus server, reached through the API.
type Instance struct {
	client *Client
	name   string
	// timeout bounds a request that has no deadline of its own.
	timeout time.Duration

	mu       sync.Mutex
	look     [2]bool
	lookedAt time.Time
}

// NewInstance makes the instance of the container name.
func NewInstance(client *Client, name string, timeout time.Duration) *Instance {
	return &Instance{client: client, name: name, timeout: timeout}
}

func (i *Instance) path(rest string) string {
	return "/1.0/instances/" + url.PathEscape(i.name) + rest
}

// state is the part of GET /1.0/instances/{name}/state used here.
type state struct {
	Status string `json:"status"`
	CPU    struct {
		// Usage is in nanoseconds.
		Usage int64 `json:"usage"`
	} `json:"cpu"`
	Memory struct {
		UsagePeak     int64 `json:"usage_peak"`
		SwapUsagePeak int64 `json:"swap_usage_peak"`
	} `json:"memory"`
}

func (i *Instance) state(ctx context.Context) (state, error) {
	var s state
	_, err := i.client.get(ctx, i.path("/state"), &s)
	return s, err
}

// Running tells whether the container runs.
func (i *Instance) Running(ctx context.Context) (bool, error) {
	s, err := i.state(ctx)
	return s.Status == "Running", err
}

// Stop stops the container at once.
func (i *Instance) Stop(ctx context.Context) error {
	_, err := i.client.do(ctx, http.MethodPut, i.path("/state"), map[string]any{"action": "stop", "force": true}, nil)
	return err
}

// Start starts the container.
func (i *Instance) Start(ctx context.Context) error {
	_, err := i.client.do(ctx, http.MethodPut, i.path("/state"), map[string]any{"action": "start"}, nil)
	return err
}

// Restore restores the container's snapshot (PUT of the instance with restore).
func (i *Instance) Restore(ctx context.Context, snapshot string) error {
	_, err := i.client.do(ctx, http.MethodPut, i.path(""), map[string]any{"restore": snapshot}, nil)
	return err
}

// ExitError is a command's exit code other than 0.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Exec runs a command with its output recorded, then reads the records and removes them.
func (i *Instance) Exec(ctx context.Context, args ...string) ([]byte, []byte, error) {
	meta, err := i.client.do(ctx, http.MethodPost, i.path("/exec"), map[string]any{
		"command": args, "environment": map[string]string{}, "interactive": false,
		"wait-for-websocket": false, "record-output": true,
	}, nil)
	var result struct {
		Return *int              `json:"return"`
		Output map[string]string `json:"output"`
	}
	if meta != nil {
		_ = json.Unmarshal(meta, &result)
	}
	var out [2][]byte
	for n, fd := range []string{"1", "2"} {
		log := result.Output[fd]
		if !strings.HasPrefix(log, i.path("/logs/")) {
			continue
		}
		data, _, readErr := i.client.raw(ctx, http.MethodGet, log, nil, nil)
		if readErr == nil {
			out[n] = data
		}
		_, _, _ = i.client.raw(ctx, http.MethodDelete, log, nil, nil)
	}
	if err == nil && result.Return == nil {
		err = errors.New("exec: no exit code in the answer")
	}
	if err == nil && *result.Return != 0 {
		err = &ExitError{Code: *result.Return}
	}
	return out[0], out[1], err
}

// WriteFile writes path.tmp through the file API (root's, 0600), then gives it to the group
// and moves it into place.
func (i *Instance) WriteFile(ctx context.Context, path, group string, data []byte) error {
	header := http.Header{}
	for k, v := range map[string]string{"X-Incus-Uid": "0", "X-Incus-Gid": "0", "X-Incus-Mode": "0600",
		"X-Incus-Type": "file", "X-Incus-Write": "overwrite", "Content-Type": "application/octet-stream"} {
		header.Set(k, v)
	}
	target := i.path("/files?path=" + url.QueryEscape(path+".tmp"))
	if _, _, err := i.client.raw(ctx, http.MethodPost, target, bytes.NewReader(data), header); err != nil {
		return err
	}
	_, errOut, err := i.Exec(ctx, "sh", "-c", finishFile, "write", path, group)
	if err != nil {
		return fmt.Errorf("writing %s: %w: %s", path, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

// SetCache replaces the device cache: the instance is read and put back (If-Match: a
// change in between fails the put) with its devices changed. A volume that is not in the
// pool is made first.
func (i *Instance) SetCache(ctx context.Context, pool, volume, path string) error {
	if volume != "" {
		if err := i.ensureVolume(ctx, pool, volume); err != nil {
			return err
		}
	}
	var current map[string]json.RawMessage
	header, err := i.client.get(ctx, i.path(""), &current)
	if err != nil {
		return err
	}
	var devices map[string]map[string]string
	if raw, ok := current["devices"]; ok {
		if err := json.Unmarshal(raw, &devices); err != nil {
			return fmt.Errorf("the instance's devices: %w", err)
		}
	}
	if devices == nil {
		devices = map[string]map[string]string{}
	}
	_, had := devices["cache"]
	if !had && volume == "" {
		return nil
	}
	delete(devices, "cache")
	if volume != "" {
		devices["cache"] = map[string]string{"type": "disk", "pool": pool, "source": volume, "path": path}
	}
	put := map[string]any{"devices": devices}
	for _, field := range []string{"architecture", "config", "ephemeral", "profiles", "stateful", "description"} {
		if raw, ok := current[field]; ok {
			put[field] = raw
		}
	}
	var ifMatch http.Header
	if etag := header.Get("ETag"); etag != "" {
		ifMatch = http.Header{"If-Match": {etag}}
	}
	_, err = i.client.do(ctx, http.MethodPut, i.path(""), put, ifMatch)
	return err
}

// ensureVolume makes the custom volume (a file system) in the pool unless it is there.
// Another slot's cycle may make it at the same moment: it is there either way.
func (i *Instance) ensureVolume(ctx context.Context, pool, volume string) error {
	path := "/1.0/storage-pools/" + url.PathEscape(pool) + "/volumes/custom/" + url.PathEscape(volume)
	var found map[string]any
	_, err := i.client.get(ctx, path, &found)
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = i.client.do(ctx, http.MethodPost, "/1.0/storage-pools/"+url.PathEscape(pool)+"/volumes",
		map[string]any{"name": volume, "type": "custom", "content_type": "filesystem"}, nil)
	if err != nil {
		if _, again := i.client.get(ctx, path, &found); again == nil {
			return nil
		}
		return fmt.Errorf("the cache volume %s: %w", volume, err)
	}
	return nil
}

// Log returns the container's LXC log.
func (i *Instance) Log(ctx context.Context) (string, error) {
	data, _, err := i.client.raw(ctx, http.MethodGet, i.path("/logs/lxc.log"), nil, nil)
	return string(data), err
}

// Runner looks at the runner from inside the container; an answer that does not come says
// active, not busy, and is not kept.
func (i *Instance) Runner(unit, workDir string) (bool, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if time.Since(i.lookedAt) < lookFor {
		return i.look[0], i.look[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), i.timeout)
	defer cancel()
	out, _, err := i.Exec(ctx, "sh", "-c", runnerScript, "look", unit, workDir)
	if err != nil {
		return true, false
	}
	switch strings.TrimSpace(string(out)) {
	case "inactive":
		i.look = [2]bool{false, false}
	case "busy":
		i.look = [2]bool{true, true}
	case "active":
		i.look = [2]bool{true, false}
	default:
		return true, false
	}
	i.lookedAt = time.Now()
	return i.look[0], i.look[1]
}

// cgroup reads the container's own cgroup files from inside it.
func (i *Instance) cgroup(ctx context.Context) slot.CgroupReader {
	out, _, _ := i.Exec(ctx, "sh", "-c", slot.CgroupScript)
	return slot.CgroupDump(string(out))
}

// PeakMiB is the state's memory and swap peaks; where the server reports none (a cgroup
// without memory.peak), the container's own memory.peak read from inside.
func (i *Instance) PeakMiB() int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*i.timeout)
	defer cancel()
	s, err := i.state(ctx)
	peak := s.Memory.UsagePeak + s.Memory.SwapUsagePeak
	if err != nil || s.Memory.UsagePeak == 0 {
		peak = slot.CgroupPeak(i.cgroup(ctx))
	}
	return slot.ToMiB(peak)
}

// CPU reads the container's cpu.stat and cpu.pressure from inside it (the state's CPU usage
// alone has no pressure, and the two must be read together).
func (i *Instance) CPU() (int64, int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*i.timeout)
	defer cancel()
	return slot.CgroupCPU(i.cgroup(ctx))
}
