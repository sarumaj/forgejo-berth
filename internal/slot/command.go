// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// MaxAssignment is the largest assignment taken: one is a few hundred bytes.
const MaxAssignment = 16384

// Refusal is a request the slot refuses; its message goes to the broker.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// cycleState is the part of a cycle's state file the command reads.
type cycleState struct {
	State      string             `json:"state"`
	StateSince int64              `json:"state_since"`
	Assignment string             `json:"assignment"`
	Last       *provider.Finished `json:"last"`
}

func readState(s *Settings) cycleState {
	var st cycleState
	data, err := os.ReadFile(s.path(".json"))
	if err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

// pending returns the id of an assignment written for the slot that its cycle has not
// taken yet ("" for none).
func pending(s *Settings) string {
	data, err := os.ReadFile(s.path(".assignment"))
	if err != nil {
		return ""
	}
	var a struct {
		Assignment string `json:"assignment"`
	}
	if json.Unmarshal(data, &a) != nil || !provider.AssignmentID.MatchString(a.Assignment) {
		return ""
	}
	return a.Assignment
}

// Status reports each slot's state, assignment and last finished assignment. A slot whose
// assignment waits for its cycle is pending, holding it: the broker neither takes it for
// ready nor loses it. No token is part of it.
func Status(slots map[string]*Settings) provider.StatusDocument {
	doc := provider.StatusDocument{Slots: []provider.SlotStatus{}}
	for _, name := range slices.Sorted(maps.Keys(slots)) {
		s := slots[name]
		st := readState(s)
		entry := provider.SlotStatus{Container: name, State: st.State, StateSince: st.StateSince,
			Assignment: st.Assignment, Last: st.Last}
		if entry.State == "" {
			entry.State = "unknown"
		}
		if entry.Last == nil {
			entry.Last = &provider.Finished{}
		}
		if p := pending(s); p != "" {
			entry.State, entry.Assignment = provider.Pending, p
		}
		entry.Paused = paused(s)
		doc.Slots = append(doc.Slots, entry)
	}
	return doc
}

// Assign checks an assignment and writes it for the slot's cycle (0600); it returns its id.
func Assign(slots map[string]*Settings, container string, data []byte) (string, error) {
	s, ok := slots[container]
	if !ok {
		return "", refuse("no slot %s", container)
	}
	if paused(s) {
		return "", refuse("%s is paused", container)
	}
	if readState(s).State != provider.Ready {
		return "", refuse("%s is not ready", container)
	}
	target := s.path(".assignment")
	if _, err := os.Stat(target); err == nil {
		return "", refuse("%s has an assignment waiting", container)
	}
	parsed, err := provider.ParseHandoff(data, s)
	if err != nil {
		return "", refuse("the assignment is refused: %v", err)
	}
	if err := writePrivate(target, data); err != nil {
		return "", err
	}
	return parsed.ID, nil
}

// Release asks the slot's cycle to stop the assignment's runner, if it still has no job.
func Release(slots map[string]*Settings, container, id string) error {
	s, ok := slots[container]
	if !ok {
		return refuse("no slot %s", container)
	}
	if readState(s).Assignment != id {
		return refuse("%s does not hold %s", container, id)
	}
	return writePrivate(s.path(".release"), []byte(id))
}

// How a slot pauses, as its pause file says: once its job ended, or at once.
const (
	PauseFinish = "finish"
	PauseStop   = "stop"
)

// paused tells whether the slot is paused (or pausing).
func paused(s *Settings) bool {
	_, err := os.Stat(s.path(".pause"))
	return err == nil
}

// PauseSlot pauses a slot: its cycle takes no assignment once its job ended, or with stop at
// once, its job stopped. The pause is a file in the state directory: it lasts until
// ResumeSlot, across restarts of the cycle. It returns the slot's state.
func PauseSlot(slots map[string]*Settings, container string, stop bool) (string, error) {
	s, ok := slots[container]
	if !ok {
		return "", refuse("no slot %s", container)
	}
	mode := PauseFinish
	if stop {
		mode = PauseStop
	}
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return "", err
	}
	if err := replace(s.path(".pause"), []byte(mode+"\n")); err != nil {
		return "", err
	}
	return stateOf(s), nil
}

// ResumeSlot lets a paused slot take assignments again (its cycle resets the container
// first); it returns the slot's state.
func ResumeSlot(slots map[string]*Settings, container string) (string, error) {
	s, ok := slots[container]
	if !ok {
		return "", refuse("no slot %s", container)
	}
	if err := os.Remove(s.path(".pause")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return stateOf(s), nil
}

func stateOf(s *Settings) string {
	if state := readState(s).State; state != "" {
		return state
	}
	return "unknown"
}

// Command runs the broker's request (its whole SSH command, as one string) against the
// slots of the settings directory; it returns the exit code.
func Command(request string, settingsDir string, stdin io.Reader, stdout, stderr io.Writer) int {
	words := strings.Fields(request)
	slots := LoadSlots(settingsDir)
	err := func() error {
		switch {
		case len(words) == 1 && words[0] == "status":
			return json.NewEncoder(stdout).Encode(Status(slots))
		case len(words) == 2 && words[0] == "assign" && config.Name.MatchString(words[1]):
			data, err := io.ReadAll(io.LimitReader(stdin, MaxAssignment+1))
			if err != nil {
				return err
			}
			if len(data) > MaxAssignment {
				return refuse("the assignment is too large")
			}
			id, err := Assign(slots, words[1], data)
			clear(data)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, id)
			return err
		case len(words) == 3 && words[0] == "release" && config.Name.MatchString(words[1]) &&
			provider.AssignmentID.MatchString(words[2]):
			return Release(slots, words[1], words[2])
		case (len(words) == 2 || len(words) == 3 && words[2] == PauseStop) && words[0] == "pause" &&
			config.Name.MatchString(words[1]):
			state, err := PauseSlot(slots, words[1], len(words) == 3)
			if err == nil {
				_, err = fmt.Fprintln(stdout, state)
			}
			return err
		case len(words) == 2 && words[0] == "resume" && config.Name.MatchString(words[1]):
			state, err := ResumeSlot(slots, words[1])
			if err == nil {
				_, err = fmt.Fprintln(stdout, state)
			}
			return err
		}
		return refuse("usage: status | assign <container> | release <container> <assignment> | " +
			"pause <container> [stop] | resume <container>")
	}()
	if err == nil {
		return 0
	}
	var r *Refusal
	if !errors.As(err, &r) {
		err = fmt.Errorf("failed: %w", err)
	}
	fmt.Fprintln(stderr, err)
	return 1
}

// writePrivate writes a file of root's alone, at once.
func writePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
