// SPDX-License-Identifier: GPL-3.0-or-later

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
)

// Runner runs a command with standard input and returns its standard output; the SSH
// provider's tests replace it.
type Runner func(ctx context.Context, argv []string, stdin []byte) ([]byte, error)

// SSH reaches every host's `forgejo-berth slot` through ssh(1), as the user whose
// authorized key forces that command.
type SSH struct {
	cfg config.SSH
	run Runner
}

// NewSSH makes the provider; run is nil for ssh(1) itself.
func NewSSH(cfg config.SSH, run Runner) *SSH {
	if run == nil {
		run = execute
	}
	return &SSH{cfg: cfg, run: run}
}

func execute(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 300 {
			message = message[len(message)-300:]
		}
		return nil, fmt.Errorf("%w: %s", err, message)
	}
	return stdout.Bytes(), nil
}

// argv is the ssh command line for a request: no terminal, no password prompt, the host's
// key checked against the known hosts, the configured key alone.
func (s *SSH) argv(host config.Host, command string) []string {
	timeout := int(s.cfg.TimeoutSeconds)
	argv := []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + strconv.Itoa(timeout),
		"-o", "StrictHostKeyChecking=yes"}
	if s.cfg.KeyFile != "" {
		argv = append(argv, "-i", config.Secret(s.cfg.KeyFile), "-o", "IdentitiesOnly=yes")
	}
	if s.cfg.KnownHostsFile != "" {
		argv = append(argv, "-o", "UserKnownHostsFile="+s.cfg.KnownHostsFile)
	}
	if s.cfg.ControlDir != "" {
		argv = append(argv, "-o", "ControlMaster=auto", "-o", "ControlPersist=120",
			"-o", "ControlPath="+s.cfg.ControlDir+"/%h")
	}
	return append(argv, "-p", strconv.Itoa(s.cfg.Port), "-l", s.cfg.User, "--", host.Address, command)
}

func (s *SSH) request(ctx context.Context, host config.Host, command string, stdin []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(3*s.cfg.TimeoutSeconds*float64(time.Second)))
	defer cancel()
	out, err := s.run(ctx, s.argv(host, command), stdin)
	if err != nil {
		return nil, fmt.Errorf("ssh %s %s: %w", host.Name, strings.Fields(command)[0], err)
	}
	return out, nil
}

// StatusDocument is the slot side's answer to `status`.
type StatusDocument struct {
	Slots []SlotStatus `json:"slots"`
}

// SlotStatus is one slot of a status answer.
type SlotStatus struct {
	Container  string    `json:"container"`
	State      string    `json:"state"`
	StateSince int64     `json:"state_since"`
	Assignment string    `json:"assignment"`
	Last       *Finished `json:"last"`
}

// Finished is a slot's last finished assignment.
type Finished struct {
	Assignment string  `json:"assignment,omitempty"`
	Task       int64   `json:"task,omitempty"`
	PeakMiB    int     `json:"peak_mib,omitempty"`
	Seconds    float64 `json:"seconds,omitempty"`
	Unused     bool    `json:"unused,omitempty"`
	// CPUCores and CPUPressure are optional: a slot that does not measure CPU omits them.
	CPUCores    float64 `json:"cpu_cores,omitempty"`
	CPUPressure float64 `json:"cpu_pressure,omitempty"`
}

// ParseStatus reads a status answer of the host; entries out of shape are skipped.
func ParseStatus(host string, data []byte) ([]Slot, error) {
	var raw struct {
		Slots []json.RawMessage `json:"slots"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: its status is no JSON: %w", host, err)
	}
	var doc StatusDocument
	for _, entry := range raw.Slots {
		var st SlotStatus
		if json.Unmarshal(entry, &st) != nil {
			continue
		}
		doc.Slots = append(doc.Slots, st)
	}
	return FromStatus(host, doc), nil
}

// FromStatus turns a slot side's status into the host's slots; a slot without a name is
// skipped.
func FromStatus(host string, doc StatusDocument) []Slot {
	var slots []Slot
	for _, st := range doc.Slots {
		if st.Container == "" {
			continue
		}
		slot := Slot{Host: host, Container: st.Container, State: st.State, Assignment: st.Assignment}
		if st.Last != nil {
			slot.Finished, slot.Task, slot.PeakMiB = st.Last.Assignment, st.Last.Task, st.Last.PeakMiB
			slot.Seconds, slot.Unused = st.Last.Seconds, st.Last.Unused
			slot.CPUCores, slot.CPUPressure = st.Last.CPUCores, st.Last.CPUPressure
		}
		slots = append(slots, slot)
	}
	return slots
}

// Status runs `status` on the host.
func (s *SSH) Status(ctx context.Context, host config.Host) ([]Slot, error) {
	out, err := s.request(ctx, host, "status", nil)
	if err != nil {
		return nil, err
	}
	return ParseStatus(host.Name, out)
}

// Assign runs `assign <container>` with the handoff on standard input.
func (s *SSH) Assign(ctx context.Context, host config.Host, container string, h Handoff) error {
	if !config.Name.MatchString(container) {
		return fmt.Errorf("container %q is not a slot name", container)
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = s.request(ctx, host, "assign "+container, data)
	clear(data)
	return err
}

// Release runs `release <container> <assignment>`.
func (s *SSH) Release(ctx context.Context, host config.Host, container, assignment string) error {
	if !config.Name.MatchString(container) || !AssignmentID.MatchString(assignment) {
		return fmt.Errorf("release %q %q: not a slot or assignment name", container, assignment)
	}
	_, err := s.request(ctx, host, "release "+container+" "+assignment, nil)
	return err
}
