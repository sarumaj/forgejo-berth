// SPDX-License-Identifier: GPL-3.0-or-later

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
)

// Paths in the containers.
const (
	// runDir is the volume the slot's own Docker puts its socket in, mounted in the runner.
	runDir = "/run/forgejo-berth"
	// filesDir holds the runner's token and configuration.
	filesDir = "/etc/forgejo-berth"
	// exitFile is written by the runner's wrapper when the runner exited.
	exitFile = "/tmp/forgejo-berth.exit"
	// hostSocket is where host-socket mounts the engine's socket in the runner.
	hostSocket = "/var/run/docker.sock"
)

// Labels on the provider's containers and volumes.
const (
	LabelHost       = "org.forgejo-berth.host"
	LabelSlot       = "org.forgejo-berth.slot"
	LabelAssignment = "org.forgejo-berth.assignment"
)

// wrapper runs the runner ("$@") and outlives it: the container stays up after the job, so
// that its cgroup can still be read, and the runner's exit is a file. A stop signal goes on
// to the runner, which then ends as it does when it is stopped.
const wrapper = `"$@" & runner=$!
trap 'kill -TERM "$runner" 2>/dev/null' TERM INT
while :; do
  wait "$runner"; status=$?
  kill -0 "$runner" 2>/dev/null || break
done
echo "$status" > ` + exitFile + `
echo "forgejo-berth: the runner exited ($status)"
trap - TERM INT
while :; do sleep 3600; done
`

// System is one slot of an engine: the runner's container <name>, made for each assignment,
// and with the dind engine the slot's own Docker <name>-docker beside it, sharing the volume
// <name>-run, where that Docker's socket is. Reset removes them all.
type System struct {
	engine   *Engine
	cfg      config.Docker
	settings *slot.Settings
	host     string
	name     string
	log      *slog.Logger
}

// NewSystem makes the system of the host's slot; name is its runner container's.
func NewSystem(engine *Engine, cfg config.Docker, s *slot.Settings, host, name string, log *slog.Logger) *System {
	return &System{engine: engine, cfg: cfg, settings: s, host: host, name: name,
		log: log.With("host", host, "container", s.Container)}
}

func (d *System) engineName() string { return d.name + "-docker" }
func (d *System) volumeName() string { return d.name + "-run" }

func (d *System) ctx(scale float64) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(scale*d.cfg.TimeoutSeconds*float64(time.Second)))
}

func (d *System) labels(assignment string) map[string]string {
	l := map[string]string{LabelHost: d.host, LabelSlot: d.settings.Container}
	if assignment != "" {
		l[LabelAssignment] = assignment
	}
	return l
}

// Reset removes the slot's containers and volume, and pulls the images that are missing.
func (d *System) Reset() error {
	ctx, cancel := d.ctx(3)
	defer cancel()
	for _, name := range []string{d.name, d.engineName()} {
		if err := d.engine.Remove(ctx, name); err != nil {
			return err
		}
	}
	if err := d.engine.RemoveVolume(ctx, d.volumeName()); err != nil {
		return err
	}
	images := []string{d.cfg.Image}
	if d.cfg.JobEngine == config.EngineDinD {
		images = append(images, d.cfg.DinDImage)
	}
	for _, image := range images {
		if _, err := d.engine.Image(ctx, image); err == nil {
			continue
		}
		pull, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		err := d.engine.Pull(pull, image)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

// Apply makes the assignment's containers: the slot's own Docker, booted, and the runner's
// container with its token and configuration, not started yet.
func (d *System) Apply(a provider.Parsed) error {
	boot := max(d.settings.BootTimeout, 10)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(boot+3*d.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	spec := Spec{
		Image: d.cfg.Image, Entrypoint: []string{"sh", "-c", wrapper, "forgejo-berth"},
		Cmd: d.runnerCommand(a), User: d.cfg.User, Labels: d.labels(a.ID),
		HostConfig: HostConfig{Init: true, NetworkMode: d.cfg.Network},
	}
	switch d.cfg.JobEngine {
	case config.EngineDinD:
		if err := d.startDinD(ctx, a.ID, boot); err != nil {
			return err
		}
		spec.Env = append(spec.Env, "DOCKER_HOST=unix://"+runDir+"/docker.sock")
		spec.HostConfig.Mounts = append(spec.HostConfig.Mounts, Mount{Type: "volume", Source: d.volumeName(), Target: runDir})
	case config.EngineHostSocket:
		spec.Env = append(spec.Env, "DOCKER_HOST=unix://"+hostSocket)
		spec.HostConfig.Mounts = append(spec.HostConfig.Mounts, Mount{Type: "bind", Source: d.cfg.HostSocket, Target: hostSocket})
	}
	if volume := d.cfg.CacheVolumes[a.Class]; volume != "" {
		spec.HostConfig.Mounts = append(spec.HostConfig.Mounts, Mount{Type: "volume", Source: volume, Target: d.cfg.CachePath})
	}
	if err := d.engine.Create(ctx, d.name, spec); err != nil {
		return err
	}
	files, err := d.files(ctx, a)
	if err != nil {
		return err
	}
	err = d.engine.Upload(ctx, d.name, "/", files)
	clear(files)
	return err
}

// runnerCommand is the runner for one job, as the wrapper's arguments.
func (d *System) runnerCommand(a provider.Parsed) []string {
	cmd := []string{"forgejo-runner"}
	if d.cfg.ConfigTemplate != "" {
		cmd = append(cmd, "--config", filesDir+"/config.yml")
	}
	cmd = append(cmd, "one-job", "--wait", "--url", d.cfg.ForgejoURL, "--uuid", a.UUID,
		"--token-url", "file://"+filesDir+"/token")
	for _, l := range a.Labels {
		cmd = append(cmd, "--label", l)
	}
	return cmd
}

// startDinD starts the slot's own Docker and waits until it answers on its socket, which
// it then opens to the runner's user (the volume is the slot's alone).
func (d *System) startDinD(ctx context.Context, assignment string, boot float64) error {
	if err := d.engine.CreateVolume(ctx, d.volumeName(), d.labels(assignment)); err != nil {
		return err
	}
	cmd := append([]string{"dockerd", "--host=unix://" + runDir + "/docker.sock"}, d.cfg.DinDArgs...)
	spec := Spec{Image: d.cfg.DinDImage, Cmd: cmd, Labels: d.labels(assignment),
		HostConfig: HostConfig{Privileged: true, NetworkMode: d.cfg.Network,
			Mounts: []Mount{{Type: "volume", Source: d.volumeName(), Target: runDir}}}}
	if err := d.engine.Create(ctx, d.engineName(), spec); err != nil {
		return err
	}
	if err := d.engine.Start(ctx, d.engineName()); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Duration(boot * float64(time.Second)))
	for {
		_, code, err := d.engine.Exec(ctx, d.engineName(), "docker", "-H", "unix://"+runDir+"/docker.sock", "version")
		if err == nil && code == 0 {
			break
		}
		if up, _ := d.engine.Running(ctx, d.engineName()); !up || time.Now().After(deadline) {
			logs, _ := d.engine.Logs(ctx, d.engineName(), 20)
			return fmt.Errorf("the slot's Docker did not start (running %v); its log ends:\n%s", up, logs)
		}
		time.Sleep(time.Second)
	}
	_, code, err := d.engine.Exec(ctx, d.engineName(), "chmod", "0666", runDir+"/docker.sock")
	if err == nil && code != 0 {
		err = fmt.Errorf("chmod of the socket exited %d", code)
	}
	return err
}

// files is the tar of the runner's token and configuration, owned by the runner's user
// where it is a number (else root's, readable by all: the container is the runner's alone).
func (d *System) files(ctx context.Context, a provider.Parsed) ([]byte, error) {
	user := d.cfg.User
	if user == "" {
		var err error
		if user, err = d.engine.Image(ctx, d.cfg.Image); err != nil {
			return nil, err
		}
	}
	uid, gid, mode := owner(user)
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	add := func(name string, data []byte, mode int64) error {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Uid: uid, Gid: gid,
			ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := w.Write(data)
		return err
	}
	dir := strings.TrimPrefix(filesDir, "/")
	if err := w.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()}); err != nil {
		return nil, err
	}
	token := []byte(a.Token)
	err := add(dir+"/token", token, mode)
	clear(token)
	if err != nil {
		return nil, err
	}
	if d.cfg.ConfigTemplate != "" {
		template, err := os.ReadFile(d.cfg.ConfigTemplate)
		if err != nil {
			return nil, err
		}
		labels, _ := json.Marshal(a.Labels)
		text := strings.ReplaceAll(strings.ReplaceAll(string(template), slot.UUIDPlaceholder, a.UUID),
			slot.LabelsPlaceholder, string(labels))
		if err := add(dir+"/config.yml", []byte(text), mode); err != nil {
			return nil, err
		}
	}
	if _, ok := d.cfg.CacheVolumes[a.Class]; ok {
		// The volume's root, the runner's: it is mounted over the image's directory.
		if err := w.WriteHeader(&tar.Header{Name: strings.TrimPrefix(path.Clean(d.cfg.CachePath), "/") + "/",
			Typeflag: tar.TypeDir, Mode: 0o700, Uid: uid, Gid: gid, ModTime: time.Now()}); err != nil {
			return nil, err
		}
	}
	err = w.Close()
	return buf.Bytes(), err
}

// owner is the numeric uid and gid of a container user (uid[:gid]); a name or none is root,
// with files readable by all.
func owner(user string) (int, int, int64) {
	uidText, gidText, _ := strings.Cut(user, ":")
	uid, err := strconv.Atoi(uidText)
	if err != nil {
		return 0, 0, 0o444
	}
	gid, err := strconv.Atoi(gidText)
	if err != nil {
		gid = uid
	}
	return uid, gid, 0o400
}

// StartRunner starts the runner's container.
func (d *System) StartRunner() error {
	ctx, cancel := d.ctx(3)
	defer cancel()
	return d.engine.Start(ctx, d.name)
}

// StopRunner signals the runner to stop, without waiting.
func (d *System) StopRunner() {
	ctx, cancel := d.ctx(1)
	defer cancel()
	if err := d.engine.Kill(ctx, d.name, "SIGTERM"); err != nil {
		d.log.Warn("the runner was not signalled", "error", err)
	}
}

// RunnerActive tells whether the runner runs: its container runs and its wrapper has not
// seen it exit. An engine that does not answer leaves it active: the next look asks again.
func (d *System) RunnerActive() bool {
	ctx, cancel := d.ctx(1)
	defer cancel()
	up, err := d.engine.Running(ctx, d.name)
	if err != nil {
		return true
	}
	if !up {
		return false
	}
	exited, err := d.engine.Exists(ctx, d.name, exitFile)
	return err != nil || !exited
}

func (d *System) task() int64 {
	ctx, cancel := d.ctx(1)
	defer cancel()
	logs, err := d.engine.Logs(ctx, d.name, 0)
	if err != nil {
		return 0
	}
	return slot.ParseTask(logs)
}

// Busy sees the job from the runner's task line: its job's containers run in the slot's own
// Docker, which runs nothing else.
func (d *System) Busy() bool { return d.task() != 0 }

// TookTask tells whether the runner's log shows it took a task.
func (d *System) TookTask() bool { return d.task() != 0 }

// SaveJournal logs the runner's output, its last journal_lines, as one record (debug): the
// container goes at the next reset. A job's own output is Forgejo's, not the runner's.
func (d *System) SaveJournal() {
	ctx, cancel := d.ctx(1)
	defer cancel()
	logs, err := d.engine.Logs(ctx, d.name, d.settings.JournalLines)
	if err != nil {
		d.log.Warn("the runner's log could not be read", "error", err)
		return
	}
	d.log.Debug("the runner's log", "log", strings.TrimRight(logs, "\n"))
}

// containers are the slot's containers whose cgroups count: the runner's, and its Docker's,
// whose cgroup holds the job's containers.
func (d *System) containers() []string {
	if d.cfg.JobEngine == config.EngineDinD {
		return []string{d.name, d.engineName()}
	}
	return []string{d.name}
}

// cgroup reads a container's own cgroup files.
func (d *System) cgroup(ctx context.Context, name string) (slot.CgroupReader, error) {
	out, code, err := d.engine.Exec(ctx, name, "sh", "-c", slot.CgroupScript)
	if err == nil && code != 0 {
		err = fmt.Errorf("reading the cgroup of %s exited %d", name, code)
	}
	return slot.CgroupDump(out), err
}

// Measure returns the task from the runner's log, and the most memory the slot's containers
// used, swap included: the sum of their peaks (a little above the peak of their sum), or
// the stats API's high-water mark where the cgroup has no memory.peak.
func (d *System) Measure() (int64, int) {
	ctx, cancel := d.ctx(3)
	defer cancel()
	var peak int64
	for _, name := range d.containers() {
		files, err := d.cgroup(ctx, name)
		bytes := slot.CgroupPeak(files)
		if bytes == 0 {
			bytes, _ = d.engine.MaxUsage(ctx, name)
		}
		if bytes == 0 {
			d.log.Warn("no memory peak", "of", name, "error", err)
		}
		peak += bytes
	}
	return d.task(), slot.ToMiB(peak)
}

// CPU sums the containers' CPU time and CPU pressure (microseconds).
func (d *System) CPU() (int64, int64, bool) {
	ctx, cancel := d.ctx(3)
	defer cancel()
	var usage, pressure int64
	for _, name := range d.containers() {
		files, err := d.cgroup(ctx, name)
		if err != nil {
			return 0, 0, false
		}
		u, p, ok := slot.CgroupCPU(files)
		if !ok {
			return 0, 0, false
		}
		usage, pressure = usage+u, pressure+p
	}
	return usage, pressure, true
}
