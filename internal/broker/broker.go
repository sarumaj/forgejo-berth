// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/history"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

const (
	day = 86400.0
	// pruneEvery is the time between two prunings of the history (seconds).
	pruneEvery = day
	// releaseAfter and releaseLooks: a runner none of whose jobs waits any more (its run
	// deleted or cancelled, another runner took them) is released once it is that old
	// (seconds) and still idle at that many looks in a row.
	releaseAfter = 60.0
	releaseLooks = 2
	// LostAfter is how long an assignment no slot reports is kept (seconds): a slot takes a
	// few seconds to pick an assignment up, and a look may miss it.
	LostAfter = 90.0
)

// learnedEnds are the jobs' ends the history learns from: a cancelled job stopped early.
var learnedEnds = []string{"success", "failure"}

// Events are the counters the broker keeps.
var Events = []string{"assigned", "learned", "unlearned", "unused", "failed", "underestimated",
	"released", "would_assign"}

// Runner is a runner of one of the broker's scopes.
type Runner struct {
	Scope string
	ID    int64
	Name  string
}

// Registered is a runner Forgejo just registered; Token is secret.
type Registered struct {
	ID    int64
	UUID  string
	Token string
}

// Forge is what the broker asks Forgejo.
type Forge interface {
	// WaitingJobs returns the jobs waiting for a runner.
	WaitingJobs(ctx context.Context) ([]Job, error)
	// Register registers an ephemeral runner of the scope.
	Register(ctx context.Context, scope, name string) (Registered, error)
	// Delete removes a runner; one gone already is fine.
	Delete(ctx context.Context, scope string, id int64) error
	// Runners lists the runners of the broker's scopes.
	Runners(ctx context.Context) ([]Runner, error)
	// TaskJob tells which of the candidates ran as the task and how it ended (success,
	// failure, cancelled, ...); found false when none did; an error when a run could not be
	// read (deleted).
	TaskJob(ctx context.Context, task int64, candidates []Job) (job Job, end string, found bool, err error)
}

// Broker is one look at the jobs and the slots after another.
type Broker struct {
	cfg      *config.Config
	forge    Forge
	slots    provider.Provider
	history  *history.History
	log      *slog.Logger
	now      func() time.Time
	shadow   bool
	mu       sync.Mutex
	state    State
	assigned map[string]Assignment
	counts   map[string]int
	sources  map[string]int
	prunedAt float64
	// reachable is whether each host answered its last status.
	reachable map[string]bool
	// orphaned counts the looks in a row an assignment's jobs did not wait; released are
	// those asked to release.
	orphaned map[string]int
	released map[string]bool
	// firstSeen is when each waiting job was first seen (Forgejo does not say since when).
	firstSeen map[int64]float64
}

// Options are the broker's collaborators besides the configuration.
type Options struct {
	Forge    Forge
	Provider provider.Provider
	History  *history.History
	Log      *slog.Logger
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// New makes a broker; it takes over the assignments kept in the history.
func New(cfg *config.Config, o Options) (*Broker, error) {
	b := &Broker{
		cfg:       cfg,
		forge:     o.Forge,
		slots:     o.Provider,
		history:   o.History,
		log:       o.Log,
		now:       o.Now,
		shadow:    cfg.Shadow || o.History.ReadOnly(),
		counts:    map[string]int{},
		sources:   map[string]int{},
		reachable: map[string]bool{},
		orphaned:  map[string]int{},
		released:  map[string]bool{},
		firstSeen: map[int64]float64{},
	}
	if b.log == nil {
		b.log = slog.Default()
	}
	if b.now == nil {
		b.now = time.Now
	}
	for _, e := range Events {
		b.counts[e] = 0
	}
	held, err := b.loadHeld()
	if err != nil {
		return nil, err
	}
	b.assigned = held
	return b, nil
}

// Shadow tells whether the broker only computes and logs its decisions.
func (b *Broker) Shadow() bool {
	return b.shadow
}

func (b *Broker) clock() float64 {
	return float64(b.now().UnixNano()) / 1e9
}

// loadHeld reads the kept assignments; one that cannot be read is forgotten.
func (b *Broker) loadHeld() (map[string]Assignment, error) {
	raw, err := b.history.Held()
	if err != nil {
		return nil, fmt.Errorf("the kept assignments: %w", err)
	}
	found := map[string]Assignment{}
	for id, data := range raw {
		var a Assignment
		if err := json.Unmarshal(data, &a); err != nil || a.ID != id {
			b.log.Warn("a kept assignment cannot be read; forgotten", "assignment", id)
			b.drop(id)
			continue
		}
		found[id] = a
	}
	return found, nil
}

func (b *Broker) drop(id string) {
	if b.shadow {
		return
	}
	if err := b.history.Drop(id); err != nil {
		b.log.Error("the history did not drop an assignment", "assignment", id, "error", err)
	}
}

// Tick learns from the finished slots, drops unused runners and starts runners for the
// waiting jobs.
func (b *Broker) Tick(ctx context.Context) error {
	now := b.clock()
	if b.shadow {
		// The live instance's assignments, as it keeps them now.
		held, err := b.loadHeld()
		if err != nil {
			return err
		}
		b.assigned = held
	}
	var seen []provider.Slot
	for _, h := range b.cfg.Hosts {
		slots, err := b.slots.Status(ctx, h)
		if err != nil {
			if was, ok := b.reachable[h.Name]; !ok || was {
				b.log.Warn("no status", "host", h.Name, "error", err)
			}
			b.reachable[h.Name] = false
			continue
		}
		b.reachable[h.Name] = true
		seen = append(seen, slots...)
	}
	for _, s := range seen {
		b.settle(ctx, s, now)
	}
	b.forgetLost(ctx, seen, now)
	jobs, err := b.forge.WaitingJobs(ctx)
	if err != nil {
		return fmt.Errorf("the waiting jobs: %w", err)
	}
	unservable := 0
	sources := map[string]int{}
	for _, j := range jobs {
		if _, ok := Offered(j, b.cfg); !ok {
			unservable++
		}
		_, source, _ := b.estimate(j, now)
		sources[source]++
	}
	if now-b.prunedAt > pruneEvery {
		forgotten, err := b.history.Prune(now - b.cfg.Estimate.RetentionDays*day)
		if err != nil {
			b.log.Error("the history was not pruned", "error", err)
		} else if forgotten > 0 {
			b.log.Info("forgot old runs", "runs", forgotten, "older_than_days", b.cfg.Estimate.RetentionDays)
		}
		b.prunedAt = now
	}
	firstSeen := map[int64]float64{}
	for _, j := range jobs {
		if t, ok := b.firstSeen[j.ID]; ok {
			firstSeen[j.ID] = t
		} else {
			firstSeen[j.ID] = now
		}
	}
	b.firstSeen = firstSeen
	b.releaseOrphans(ctx, seen, jobs, now)
	// The hosts that did not answer get no runner: their slots are unknown.
	var answered []provider.Slot
	for _, s := range seen {
		if b.reachable[s.Host] {
			answered = append(answered, s)
		}
	}
	waited := func(j Job) float64 {
		if t, ok := b.firstSeen[j.ID]; ok {
			return now - t
		}
		return 0
	}
	reserve := func(j Job) (int, bool) {
		peak, source, found := b.estimate(j, now)
		return Predict(peak, found, source, b.cfg.Estimate)
	}
	var cores CoresFunc
	if b.cfg.CPU.Overcommit > 0 {
		cores = func(j Job) float64 { return b.cores(j, now) }
	}
	for _, p := range Schedule(jobs, answered, b.assignments(), reserve, b.cfg, waited, cores) {
		b.start(ctx, p, now)
	}
	b.publish(now, len(jobs), unservable, sources)
	return nil
}

// assignments returns the active assignments in a stable order (oldest first).
func (b *Broker) assignments() []Assignment {
	list := slices.Collect(maps.Values(b.assigned))
	slices.SortFunc(list, func(x, y Assignment) int {
		if x.Since != y.Since {
			if x.Since < y.Since {
				return -1
			}
			return 1
		}
		return strings.Compare(x.ID, y.ID)
	})
	return list
}

func (b *Broker) estimate(j Job, now float64) (int, string, bool) {
	e := b.cfg.Estimate
	peak, source, found, err := b.history.Estimate(j.Repo, j.Name, j.Family(), now-e.SpikeDays*day, e.HistoryRuns)
	if err != nil {
		b.log.Error("no estimate", "repo", j.Repo, "job", j.Name, "error", err)
		return 0, "none", false
	}
	return peak, source, found
}

// cores returns the cores predicted for a job: the most of its last runs, as for memory,
// without a margin (an average already lies below the job's busiest moments' needs, and the
// overcommit factor is the margin), else unknown_cores.
func (b *Broker) cores(j Job, now float64) float64 {
	e := b.cfg.Estimate
	c, _, found, err := b.history.EstimateCores(j.Repo, j.Name, j.Family(), now-e.SpikeDays*day, e.HistoryRuns)
	if err != nil || !found {
		return b.cfg.CPU.UnknownCores
	}
	return c
}

// settle learns from a slot's finished assignment, or deletes its unused runner.
func (b *Broker) settle(ctx context.Context, s provider.Slot, now float64) {
	if s.Finished == "" {
		return
	}
	a, ok := b.assigned[s.Finished]
	if !ok {
		return
	}
	delete(b.assigned, a.ID)
	b.drop(a.ID)
	where := []any{"host", s.Host, "container", s.Container, "assignment", a.ID}
	if s.Unused {
		b.counts["unused"]++
		b.delete(ctx, a)
		return
	}
	if s.Task == 0 {
		// It ran a job, but its journal had no task line: the runner's log may have changed.
		b.counts["unlearned"]++
		b.log.Warn("ran a job, no task line in its journal; not learned", where...)
		b.delete(ctx, a)
		return
	}
	delete(b.orphaned, a.ID)
	delete(b.released, a.ID)
	job, end, found, err := b.forge.TaskJob(ctx, s.Task, a.Candidates)
	if err != nil {
		// Its run was deleted meanwhile: nothing to learn, the look goes on.
		b.log.Warn("task not looked up; not learned", append(where, "task", s.Task, "error", err)...)
		b.counts["unlearned"]++
		return
	}
	if !found {
		b.log.Warn("the task is none of its jobs; not learned", append(where, "task", s.Task)...)
		b.counts["unlearned"]++
		return
	}
	where = append(where, "repo", job.Repo, "job", job.Name)
	if !slices.Contains(learnedEnds, end) {
		if end == "" {
			end = "unknown"
		}
		b.log.Info("not learned", append(where, "ended", end)...)
		return
	}
	reserved := a.ReserveMiB
	if a.Exclusive {
		reserved = 0
	}
	if b.shadow {
		b.log.Info("would learn", append(where, "peak_mib", s.PeakMiB)...)
		return
	}
	err = b.history.Record(history.Run{
		Repo: job.Repo, Name: job.Name, Family: job.Family(), At: now,
		PeakMiB: s.PeakMiB, ReservedMiB: reserved, Seconds: s.Seconds, Host: s.Host,
		CPUCores: s.CPUCores, CPUPressure: s.CPUPressure,
	})
	if err != nil {
		b.log.Error("not recorded", append(where, "error", err)...)
		return
	}
	b.counts["learned"]++
	if reserved > 0 && s.PeakMiB > reserved {
		// Learned for the next run already; counted, as many mean the margins are too small.
		b.counts["underestimated"]++
		b.log.Warn("needed more than reserved", append(where, "peak_mib", s.PeakMiB, "reserved_mib", reserved)...)
	}
	b.log.Info("learned", append(where, "peak_mib", s.PeakMiB, "seconds", s.Seconds,
		"cpu_cores", s.CPUCores, "cpu_pressure", s.CPUPressure,
		"reserved_mib", a.ReserveMiB, "exclusive", a.Exclusive)...)
}

// releaseOrphans releases the runners still idle whose jobs no longer wait (deleted,
// cancelled, taken by another runner): the slot is free again in a minute instead of after
// unused_minutes. The slot's cycle decides: a runner that took a job meanwhile goes on.
func (b *Broker) releaseOrphans(ctx context.Context, seen []provider.Slot, jobs []Job, now float64) {
	waiting := map[int64]bool{}
	for _, j := range jobs {
		waiting[j.ID] = true
	}
	idle := map[[2]string]bool{}
	for _, s := range seen {
		if s.State == provider.Assigned {
			idle[[2]string{s.Host, s.Container}] = true
		}
	}
	for _, a := range b.assignments() {
		stillWaits := slices.ContainsFunc(a.Candidates, func(j Job) bool { return waiting[j.ID] })
		if !idle[[2]string{a.Host, a.Container}] || now-a.Since < releaseAfter || stillWaits {
			delete(b.orphaned, a.ID)
			continue
		}
		b.orphaned[a.ID]++
		if b.orphaned[a.ID] < releaseLooks || b.released[a.ID] {
			continue
		}
		h, ok := b.cfg.Host(a.Host)
		if !ok {
			continue
		}
		where := []any{"host", a.Host, "container", a.Container, "assignment", a.ID}
		if b.shadow {
			b.log.Info("would release, its jobs wait no more", where...)
			b.released[a.ID] = true
			continue
		}
		if err := b.slots.Release(ctx, h, a.Container, a.ID); err != nil {
			b.log.Warn("not released", append(where, "error", err)...)
			continue
		}
		b.released[a.ID] = true
		b.counts["released"]++
		b.log.Info("released, its jobs wait no more", where...)
	}
}

// forgetLost forgets the assignments whose slot was reset without reporting them (its cycle
// restarted), and deletes their runners, as no slot holds them any more. Not before
// LostAfter: a slot reports an assignment from when it is written (pending), but a look may
// come between. A host taken out of the configuration never reports its slots: its
// assignments are lost at once.
func (b *Broker) forgetLost(ctx context.Context, seen []provider.Slot, now float64) {
	holding := map[string]bool{}
	for _, s := range seen {
		if s.Assignment != "" {
			holding[s.Assignment] = true
		}
		if s.Finished != "" {
			holding[s.Finished] = true
		}
	}
	for _, a := range b.assignments() {
		_, configured := b.cfg.Host(a.Host)
		if configured && (!b.reachable[a.Host] || now-a.Since < LostAfter) {
			continue
		}
		if !configured || !holding[a.ID] {
			delete(b.assigned, a.ID)
			b.drop(a.ID)
			b.log.Warn("lost its runner", "host", a.Host, "container", a.Container, "assignment", a.ID)
			b.delete(ctx, a)
		}
	}
}

// start registers a runner for the placement and hands it to the slot.
func (b *Broker) start(ctx context.Context, p Placement, now float64) {
	id := fmt.Sprintf("%s-%s-%d-%d", p.Host, p.Container, int64(now), p.Job.ID)
	where := []any{"host", p.Host, "container", p.Container, "repo", p.Job.Repo, "job", p.Job.Name,
		"scope", p.Scope, "labels", strings.Join(p.Labels, ","), "reserve_mib", p.ReserveMiB, "exclusive", p.Exclusive}
	if b.shadow {
		b.counts["would_assign"]++
		b.log.Info("would assign a runner", where...)
		return
	}
	h, ok := b.cfg.Host(p.Host)
	if !ok {
		return
	}
	runner, err := b.forge.Register(ctx, p.Scope, b.cfg.RunnerPrefix+id)
	if err != nil {
		b.counts["failed"]++
		b.log.Error("no runner registered", append(where, "error", err)...)
		return
	}
	a := Assignment{
		ID: id, Host: p.Host, Container: p.Container, Scope: p.Scope, RunnerID: runner.ID,
		Labels: p.Labels, Candidates: p.Candidates, ReserveMiB: p.ReserveMiB, Exclusive: p.Exclusive,
		ReserveCores: p.ReserveCores, Since: now,
	}
	// Kept before the slot has it: a broker that stops in between does not take it for a
	// leftover.
	data, err := json.Marshal(a)
	if err == nil {
		err = b.history.Hold(a.ID, data)
	}
	if err != nil {
		b.counts["failed"]++
		b.log.Error("the assignment was not kept", append(where, "error", err)...)
		b.delete(ctx, a)
		return
	}
	handoff := provider.Handoff{
		Assignment: id, UUID: runner.UUID, Token: runner.Token, Labels: p.Labels,
		Class: Class(p.Scope), UnusedMinutes: b.cfg.UnusedMinutes,
	}
	err = b.slots.Assign(ctx, h, p.Container, handoff)
	// The token is no longer needed: the slot has it, or the runner goes.
	runner.Token, handoff.Token = "", ""
	if err != nil {
		b.counts["failed"]++
		b.log.Error("not assigned", append(where, "error", err)...)
		b.drop(a.ID)
		b.delete(ctx, a)
		return
	}
	b.assigned[id] = a
	b.counts["assigned"]++
	b.log.Info("assigned a runner", append(where, "assignment", id)...)
}

func (b *Broker) delete(ctx context.Context, a Assignment) {
	if b.shadow {
		return
	}
	if err := b.forge.Delete(ctx, a.Scope, a.RunnerID); err != nil {
		b.log.Warn("runner not deleted", "runner", a.RunnerID, "scope", a.Scope, "error", err)
	}
}

// Sweep deletes the broker's runners that none of its assignments holds (left when the
// broker stopped between registering a runner and keeping its assignment).
func (b *Broker) Sweep(ctx context.Context) error {
	if b.shadow {
		return nil
	}
	runners, err := b.forge.Runners(ctx)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, a := range b.assigned {
		held[fmt.Sprintf("%s/%d", a.Scope, a.RunnerID)] = true
	}
	var failed []error
	for _, r := range runners {
		if !strings.HasPrefix(r.Name, b.cfg.RunnerPrefix) || held[fmt.Sprintf("%s/%d", r.Scope, r.ID)] {
			continue
		}
		if err := b.forge.Delete(ctx, r.Scope, r.ID); err != nil {
			failed = append(failed, fmt.Errorf("runner %s (%s): %w", r.Name, r.Scope, err))
			continue
		}
		b.log.Info("deleted a leftover runner", "runner", r.Name, "scope", r.Scope)
	}
	return errors.Join(failed...)
}

// Counts returns a copy of the event counters.
func (b *Broker) Counts() map[string]int {
	return maps.Clone(b.counts)
}

// Assignments returns the active assignments.
func (b *Broker) Assignments() []Assignment {
	return b.assignments()
}
