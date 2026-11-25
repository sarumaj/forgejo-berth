// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"encoding/json"
	"maps"
	"math"
	"slices"
	"time"
)

// Backoff is what the broker knows of a waiting job whose runners went unused: Forgejo lists
// it as waiting, but handed it to none of the runners that could take it (a run held by its
// concurrency group, say, which the API berth reads does not show). Kept in the history by
// the job's id, so that a restart neither forgets it nor starts the churn anew.
type Backoff struct {
	// Job is the job as it was last seen waiting.
	Job Job `json:"job"`
	// Unused counts the runners in a row that could take the job and went unused while it
	// waited.
	Unused int `json:"unused"`
	// Until is when the job's last back-off ends (Unix seconds; 0: never backed off).
	Until float64 `json:"until"`
	// Last is the assignment that last counted: a runner counts once, whoever settles it
	// (a shadow broker sees the live instance's settle more than once).
	Last string `json:"last"`
	// Retried tells that the end of the last back-off was seen: the job is scheduled again.
	Retried bool `json:"retried,omitempty"`
}

// active tells whether the job is held back now.
func (e Backoff) active(now float64) bool {
	return e.Until > now
}

// backoffSeconds is the back-off after that many runners went unused in a row: base_minutes
// at the unused_runs-th, doubled for each one more, at most max_minutes.
func (b *Broker) backoffSeconds(unused int) float64 {
	c := b.cfg.Backoff
	k := unused - c.Runs()
	return min(c.BaseMinutes*60*math.Pow(2, float64(k)), c.MaxMinutes*60)
}

// loadBackoffs reads the kept back-offs; one that cannot be read is forgotten. A shadow
// broker keeps its own, in memory: it computes what it would do, and the live instance's
// database may predate the table.
func (b *Broker) loadBackoffs() error {
	if b.shadow {
		return nil
	}
	raw, err := b.history.Backoffs()
	if err != nil {
		return err
	}
	for id, data := range raw {
		var e Backoff
		if err := json.Unmarshal(data, &e); err != nil || e.Job.ID != id {
			b.log.Warn("a kept back-off cannot be read; forgotten", "job_id", id)
			b.forgetBackoff(id)
			continue
		}
		b.backoffs[id] = e
	}
	return nil
}

func (b *Broker) keepBackoff(e Backoff) {
	b.backoffs[e.Job.ID] = e
	if b.shadow {
		return
	}
	data, err := json.Marshal(e)
	if err == nil {
		err = b.history.KeepBackoff(e.Job.ID, data)
	}
	if err != nil {
		b.log.Error("the back-off was not kept", "job_id", e.Job.ID, "error", err)
	}
}

func (b *Broker) forgetBackoff(id int64) {
	delete(b.backoffs, id)
	if b.shadow {
		return
	}
	if err := b.history.ForgetBackoff(id); err != nil {
		b.log.Error("the back-off was not forgotten", "job_id", id, "error", err)
	}
}

// noteUnused notes a runner that went unused for the back-off, once its jobs are read: one
// stopped before unused_minutes (released, its slot paused) says nothing of its jobs.
func (b *Broker) noteUnused(a Assignment, now float64) {
	if b.cfg.Backoff.Runs() > 0 && now-a.Since >= b.cfg.UnusedMinutes*60 {
		b.unused = append(b.unused, a)
	}
}

// ranJob resets the back-off of a job a runner took: Forgejo hands it out again.
func (b *Broker) ranJob(j Job) {
	e, ok := b.backoffs[j.ID]
	if !ok {
		return
	}
	if e.Until > 0 {
		b.log.Info("a backed-off job ran; back-off reset", backoffWhere(e)...)
	}
	b.forgetBackoff(j.ID)
}

func backoffWhere(e Backoff) []any {
	return []any{"repo", e.Job.Repo, "job", e.Job.Name, "job_id", e.Job.ID, "run_id", e.Job.RunID,
		"unused", e.Unused}
}

// updateBackoffs forgets the back-offs of the jobs that wait no more, notes the end of
// those that ended, and counts the runners gone unused against every job they could take
// that still waits: Forgejo handed none of them to a runner that waited unused_minutes for
// one. A job counted unused_runs times in a row is held back.
func (b *Broker) updateBackoffs(jobs []Job, now float64) {
	runs := b.cfg.Backoff.Runs()
	waiting := map[int64]Job{}
	for _, j := range jobs {
		// With the back-off turned off, the kept ones go.
		if runs > 0 {
			waiting[j.ID] = j
		}
	}
	for _, id := range slices.Sorted(maps.Keys(b.backoffs)) {
		e := b.backoffs[id]
		if _, ok := waiting[id]; !ok {
			if e.active(now) {
				b.log.Info("a backed-off job waits no more; back-off forgotten", backoffWhere(e)...)
			}
			b.forgetBackoff(id)
			continue
		}
		if e.Until > 0 && !e.active(now) && !e.Retried {
			e.Retried = true
			b.keepBackoff(e)
			msg := "back-off over; the job is tried again"
			if b.shadow {
				msg = "back-off over; the job would be tried again"
			}
			b.log.Info(msg, backoffWhere(e)...)
		}
	}
	for _, a := range b.unused {
		for _, c := range a.Candidates {
			j, ok := waiting[c.ID]
			if !ok {
				continue
			}
			e := b.backoffs[c.ID]
			if e.Last == a.ID {
				continue
			}
			e.Job, e.Last = j, a.ID
			e.Unused++
			if e.Unused >= runs {
				seconds := b.backoffSeconds(e.Unused)
				e.Until, e.Retried = now+seconds, false
				b.counts["backed_off"]++
				msg := "backed off: its runners go unused while it waits"
				if b.shadow {
					msg = "would back off: its runners go unused while it waits"
				}
				b.log.Warn(msg, append(backoffWhere(e), "assignment", a.ID, "minutes", seconds/60,
					"until", time.Unix(int64(e.Until), 0).UTC().Format(time.RFC3339))...)
			}
			b.keepBackoff(e)
		}
	}
	b.unused = nil
}

// backedOff tells whether a job is held back now.
func (b *Broker) backedOff(id int64, now float64) bool {
	e, ok := b.backoffs[id]
	return ok && e.active(now)
}

// Backoffs returns the jobs' back-offs, by job id.
func (b *Broker) Backoffs() map[int64]Backoff {
	return maps.Clone(b.backoffs)
}
