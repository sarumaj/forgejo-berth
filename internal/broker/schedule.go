// SPDX-License-Identifier: GPL-3.0-or-later

// Package broker decides which waiting job gets a runner on which slot, and runs the loop
// that learns from finished slots and starts the runners.
package broker

import (
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"

	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
)

// matrix is a matrix entry's part of a job's name: "server (32)".
var matrix = regexp.MustCompile(`\s*\([^()]*\)$`)

// Job is a job waiting for a runner.
type Job struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	RepoID int64    `json:"repo_id"`
	Owner  string   `json:"owner"`
	Repo   string   `json:"repo"`
	Name   string   `json:"name"`
	RunsOn []string `json:"runs_on"`
}

// Family is the job's name without its matrix entry: "server (32)" and "server (36)" are
// one family.
func (j Job) Family() string {
	if f := matrix.ReplaceAllString(j.Name, ""); f != "" {
		return f
	}
	return j.Name
}

// Assignment is a runner the broker registered and handed to a slot. It holds no token.
type Assignment struct {
	ID        string   `json:"id"`
	Host      string   `json:"host"`
	Container string   `json:"container"`
	Scope     string   `json:"scope"`
	RunnerID  int64    `json:"runner_id"`
	Labels    []string `json:"labels"`
	// Candidates are the jobs the runner could take when it was registered.
	Candidates []Job `json:"candidates"`
	ReserveMiB int   `json:"reserve_mib"`
	Exclusive  bool  `json:"exclusive"`
	// ReserveCores are the cores predicted for it (0 without the CPU admission).
	ReserveCores float64 `json:"reserve_cores,omitempty"`
	// Since is when it was registered (Unix seconds).
	Since float64 `json:"since"`
}

// Placement is where the scheduler puts a job, and with which runner.
type Placement struct {
	Job        Job
	Host       string
	Container  string
	Scope      string
	Labels     []string
	Candidates []Job
	ReserveMiB int
	Exclusive  bool
	// ReserveCores are the cores predicted for it (0 without the CPU admission).
	ReserveCores float64
}

// Offer is the runner a job may get.
type Offer struct {
	Scope  string
	Labels []string
}

// GlobalScope is the scope of everyone's runners; an owner's is "user:<owner>".
const GlobalScope = "global"

// Offered returns the scope and labels of a runner for the job, or false when no runner may
// take it. A trusted owner's job gets a runner of its owner with the labels it asks for, if
// they are among the trusted labels or a host's features; anyone else's a global one, with
// the shared labels only.
func Offered(job Job, cfg *config.Config) (Offer, bool) {
	if len(job.RunsOn) == 0 {
		return Offer{}, false
	}
	asked := unique(job.RunsOn)
	if _, ok := cfg.Trusted.Owners[job.Owner]; ok {
		allowed := cfg.Features()
		for _, l := range cfg.Trusted.Labels {
			allowed[l] = true
		}
		if subset(asked, allowed) {
			return Offer{Scope: "user:" + job.Owner, Labels: asked}, true
		}
		return Offer{}, false
	}
	allowed := map[string]bool{}
	for _, l := range cfg.Shared.Labels {
		allowed[l] = true
	}
	if subset(asked, allowed) {
		return Offer{Scope: GlobalScope, Labels: asked}, true
	}
	return Offer{}, false
}

// Covers tells whether Forgejo may hand a runner with the offer's scope and labels the job:
// the labels the job runs on are all among the runner's, and the job is within the runner's
// scope. A global runner sees every repository's jobs, an owner's runner only that owner's.
// Whether berth would start a runner for the job (Offered) does not matter: Forgejo does not
// know.
func Covers(runner Offer, job Job) bool {
	if len(job.RunsOn) == 0 || (runner.Scope != GlobalScope && runner.Scope != "user:"+job.Owner) {
		return false
	}
	return subset(job.RunsOn, set(runner.Labels))
}

// Predict returns the memory to reserve for a job (MiB) from its estimate; without one
// unknown_mib, or false (a host alone) where that is 0.
func Predict(peak int, found bool, source string, e config.Estimate) (int, bool) {
	if !found {
		if e.Unknown() == 0 {
			return 0, false
		}
		return e.Unknown(), true
	}
	margin := e.RelativeMargin
	if source == "job" {
		margin = e.Margin
	}
	return max(e.FloorMiB, int(math.RoundToEven(float64(peak)*margin))), true
}

// ReserveFunc returns a job's reservation, or false when it needs a host alone.
type ReserveFunc func(Job) (int, bool)

// WaitedFunc returns how long a job has waited (seconds).
type WaitedFunc func(Job) float64

// CoresFunc returns the cores predicted for a job; nil when the CPU admission is off.
type CoresFunc func(Job) float64

// Schedule returns the runners to start now: for each waiting job (oldest first) not yet
// served by a runner, a ready slot on a host with its features and the memory it needs free.
//
// The jobs go in the order they arrived (waited). Forgejo hands a runner the oldest waiting
// job it covers (see Covers): a global runner any owner's, an owner's runner only that
// owner's. Which runner asks first is not known (slots boot at different speeds), so the
// runners on their way claim jobs in the order they were registered, which decides only
// which jobs get no new runner; a new runner reserves the most that its job and every older
// job it covers need, claimed or not, as it gets one of them if it asks first. It does not
// reserve for the younger ones, which go to the runners after it. A job that does not fit
// lets younger ones go ahead until it has waited starve_minutes; then it holds the host fit
// for it with the most memory free, where nothing else starts until it does.
func Schedule(jobs []Job, slots []provider.Slot, assignments []Assignment, reserve ReserveFunc,
	cfg *config.Config, waited WaitedFunc, cores CoresFunc) []Placement {
	if waited == nil {
		waited = func(Job) float64 { return 0 }
	}
	waiting := slices.Clone(jobs)
	sort.SliceStable(waiting, func(a, b int) bool {
		wa, wb := waited(waiting[a]), waited(waiting[b])
		if wa != wb {
			return wa > wb
		}
		return waiting[a].ID < waiting[b].ID
	})
	runners := map[int64]*Offer{}
	for _, j := range waiting {
		if o, ok := Offered(j, cfg); ok {
			runners[j.ID] = &o
		} else {
			runners[j.ID] = nil
		}
	}
	hosts := map[string]config.Host{}
	used := map[string]int{}
	busy := map[string]int{}
	usedCores := map[string]float64{}
	for _, h := range cfg.Hosts {
		hosts[h.Name] = h
		used[h.Name] = 0
		busy[h.Name] = 0
	}
	alone := map[string]bool{}
	for _, a := range assignments {
		if _, ok := hosts[a.Host]; ok {
			used[a.Host] += a.ReserveMiB
			usedCores[a.Host] += a.ReserveCores
			busy[a.Host]++
		}
		if a.Exclusive {
			alone[a.Host] = true
		}
	}

	// The jobs a runner is on its way to: each runner still waiting for its job claims the
	// oldest unclaimed one it covers, as Forgejo will hand it that if the runners ask in the
	// order they were registered (assignments come oldest first). A global runner may thus
	// claim an owner's job before that owner's runner registered after it.
	claimed := map[int64]bool{}
	states := map[[2]string]string{}
	for _, s := range slots {
		states[[2]string{s.Host, s.Container}] = s.State
	}
	for _, a := range assignments {
		if states[[2]string{a.Host, a.Container}] == provider.Running {
			continue
		}
		key := Offer{Scope: a.Scope, Labels: a.Labels}
		for _, j := range waiting {
			if !claimed[j.ID] && Covers(key, j) {
				claimed[j.ID] = true
				break
			}
		}
	}
	common := map[string]bool{}
	for _, l := range cfg.Trusted.Labels {
		common[l] = true
	}
	for _, l := range cfg.Shared.Labels {
		common[l] = true
	}
	taken := map[[2]string]bool{}
	for _, a := range assignments {
		taken[[2]string{a.Host, a.Container}] = true
	}
	var ready []provider.Slot
	for _, s := range slots {
		_, known := hosts[s.Host]
		if s.State == provider.Ready && s.Assignment == "" && !taken[[2]string{s.Host, s.Container}] && known {
			ready = append(ready, s)
		}
	}
	sort.Slice(ready, func(a, b int) bool {
		if ready[a].Host != ready[b].Host {
			return ready[a].Host < ready[b].Host
		}
		return ready[a].Container < ready[b].Container
	})
	// What a job needs of a host beyond the common labels: its features.
	featuresOK := func(labels []string, h config.Host) bool {
		var extra []string
		for _, l := range labels {
			if !common[l] {
				extra = append(extra, l)
			}
		}
		return subset(extra, set(h.Features))
	}

	var placements []Placement
	// Host -> the starving job it is held for.
	held := map[string]int64{}
	starve := cfg.StarveMinutes * 60
	for _, job := range waiting {
		runner := runners[job.ID]
		if runner == nil || claimed[job.ID] {
			continue
		}
		var candidates []Job
		for _, other := range waiting {
			if Covers(*runner, other) {
				candidates = append(candidates, other)
			}
		}
		// The jobs this runner may get: this one and the older ones, oldest first. Those that
		// other runners claimed count too: a runner on its way, or one started in this pass,
		// may ask after this one, which then gets the oldest. It claims the oldest unclaimed.
		var ahead []Job
		first := job
		for _, other := range candidates {
			ahead = append(ahead, other)
			if other.ID == job.ID {
				break
			}
		}
		for _, other := range ahead {
			if !claimed[other.ID] {
				first = other
				break
			}
		}
		exclusive := false
		need := -1
		for _, other := range ahead {
			if n, ok := reserve(other); ok {
				need = max(need, n)
			} else {
				exclusive = true
			}
		}
		if need < 0 {
			need = cfg.Estimate.FloorMiB
		}
		// The cores, as the memory: the most of the jobs this runner may get.
		needCores := 0.0
		if cores != nil {
			for _, other := range ahead {
				needCores = max(needCores, cores(other))
			}
		}
		type fit struct {
			free  int
			slot  provider.Slot
			whole bool
		}
		var fitting []fit
		for _, slot := range ready {
			h := hosts[slot.Host]
			if !featuresOK(runner.Labels, h) {
				continue
			}
			if id, ok := held[slot.Host]; ok && id != job.ID {
				continue
			}
			// A job expected to need all of a host's memory or more gets the host alone: it
			// may still fit (the estimate has its margin, and swap), and it is never shut out.
			whole := exclusive || need >= h.MemoryMiB
			if alone[slot.Host] || (whole && busy[slot.Host] > 0) {
				continue
			}
			if h.MaxJobs > 0 && busy[slot.Host] >= h.MaxJobs {
				continue
			}
			// An idle host takes any job, as with memory: a job never waits for cores it
			// cannot have.
			if cores != nil && busy[slot.Host] > 0 &&
				usedCores[slot.Host]+needCores > h.CPUCores*cfg.CPU.Overcommit {
				continue
			}
			free := h.MemoryMiB - used[slot.Host]
			if whole || need <= free {
				fitting = append(fitting, fit{free, slot, whole})
			}
		}
		if len(fitting) == 0 {
			if waited(job) >= starve {
				best, bestFree := "", math.MinInt
				for _, h := range cfg.Hosts {
					if _, ok := held[h.Name]; ok || !featuresOK(runner.Labels, h) {
						continue
					}
					free := h.MemoryMiB - used[h.Name]
					if free > bestFree || (free == bestFree && h.Name > best) {
						best, bestFree = h.Name, free
					}
				}
				if best != "" {
					held[best] = job.ID
				}
			}
			continue
		}
		// The host with the most memory free: the load spreads over the hosts. Ties go to
		// the less busy host, then to the first slot.
		chosen := fitting[0]
		for _, f := range fitting[1:] {
			if f.free > chosen.free || (f.free == chosen.free && busy[f.slot.Host] < busy[chosen.slot.Host]) {
				chosen = f
			}
		}
		reserved := need
		if chosen.whole {
			reserved = hosts[chosen.slot.Host].MemoryMiB
		}
		ready = slices.DeleteFunc(ready, func(s provider.Slot) bool {
			return s.Host == chosen.slot.Host && s.Container == chosen.slot.Container
		})
		claimed[first.ID] = true
		used[chosen.slot.Host] += reserved
		usedCores[chosen.slot.Host] += needCores
		busy[chosen.slot.Host]++
		if chosen.whole {
			alone[chosen.slot.Host] = true
		}
		placements = append(placements, Placement{
			Job:          job,
			Host:         chosen.slot.Host,
			Container:    chosen.slot.Container,
			Scope:        runner.Scope,
			Labels:       runner.Labels,
			Candidates:   candidates,
			ReserveMiB:   reserved,
			Exclusive:    chosen.whole,
			ReserveCores: needCores,
		})
	}
	return placements
}

// Class is the slot class of a scope: an owner's runners are trusted, global ones shared.
func Class(scope string) string {
	if strings.HasPrefix(scope, "user:") {
		return "trusted"
	}
	return "shared"
}

func set(values []string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

func subset(values []string, of map[string]bool) bool {
	for _, v := range values {
		if !of[v] {
			return false
		}
	}
	return true
}

// unique returns the values sorted, each once.
func unique(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}
