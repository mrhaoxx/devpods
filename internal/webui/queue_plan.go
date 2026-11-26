// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// The planner (design 2026-11-26): one cluster's queue against its
// reservable machines, as they are now and as their reservations and
// running jobs say they will be. Higher priority first, then who
// submitted first; a job's ranks start all together or not at all. The
// first job that cannot start (the head) is promised the earliest time
// enough will be free; a job behind it starts only if it will have ended
// by then (EASY backfill), so it never delays the head. A head blocked
// by lower-priority preemptible jobs has the fewest of them stopped.

// qMachine: a machine — what it has, what interactive DevPods (and other
// workloads) take of it outside their owners' reservations, its
// reservations, and whether it is reservable (jobs run only on those).
type qMachine struct {
	Name             string
	Cap, Interactive amount
	Rs               []*devpodv1alpha1.NodeReservation
	Reservable       bool
}

// qEntry: something waiting in the line — a job, or a DevPod to start.
type qEntry struct {
	Key         string                 // a job's id, or "devpod/<name>"
	Job         *devpodv1alpha1.Job    // nil: a DevPod
	DevPod      *devpodv1alpha1.DevPod // nil: a job
	Owner       string
	Priority    int32
	Since       time.Time     // its place in time
	Nodes       int32         // ranks (a DevPod: 1)
	Ask         amount        // each rank's
	Limit       time.Duration // 0: no end
	Preemptible bool
	NoVictims   bool   // nobody is stopped for it (a job whose ranks once did not all start)
	Only        string // the one machine it may have ("": any)
	Avoid       string // a machine it is not given now ("": none)
}

// jobEntry: job j as an entry of the line.
func jobEntry(j *devpodv1alpha1.Job) qEntry {
	return qEntry{Key: j.Name, Job: j, Owner: j.Spec.Owner, Priority: j.Spec.Priority, Since: j.CreationTimestamp.Time,
		Nodes: j.Spec.Nodes, Ask: jobAsk(j), Limit: j.Spec.TimeLimit.Duration, Preemptible: j.Spec.Preemptible, NoVictims: j.Status.GangTimeouts > 0}
}

// qRun: a job running (or starting) — where, how much on each machine,
// until when at most.
type qRun struct {
	Key         string              // as its entry's
	Job         *devpodv1alpha1.Job // nil: a DevPod started by this plan
	Machines    []string
	Ask         amount
	Until       time.Time
	Preemptible bool
	Priority    int32
	Started     time.Time
}

// queueView: what the planner plans from.
type queueView struct {
	Now      time.Time
	Machines []qMachine
	Running  []qRun
	Queued   []qEntry
	// Allowed: the access rules (and its own placement) let the entry's
	// ranks run on machine.
	Allowed func(e qEntry, machine string) bool
	// QuotaOK: the entry's ranks fit its owner's quota now, beside planned
	// (what this plan starts before it).
	QuotaOK func(e qEntry, planned []qEntry) bool
	// Horizon: how far ahead a promise is looked for.
	Horizon time.Duration
}

type qStart struct {
	Entry    qEntry
	Machines []string
	// ClearsAt: an entry with no end, started where a reservation will
	// want its room — when (nil: none in sight)
	ClearsAt *time.Time
}

// noEnd: how long an entry with no end is taken to run.
const noEnd = 100 * 365 * 24 * time.Hour

// until: when e, started at from, has ended at the latest.
func (e qEntry) until(from time.Time) time.Time {
	if e.Limit <= 0 {
		return from.Add(noEnd)
	}
	return from.Add(e.Limit)
}

// window: how far ahead e must fit — its time limit, or (no end) all the
// planner sees.
func (v queueView) window(e qEntry) time.Duration {
	if e.Limit <= 0 {
		return v.Horizon
	}
	return e.Limit
}

// mayHave: e may be given machine m — the access rules, and its own one
// machine or the one it avoids.
func (v queueView) mayHave(e qEntry, m string) bool {
	if (e.Only != "" && m != e.Only) || (e.Avoid != "" && m == e.Avoid) {
		return false
	}
	return v.Allowed == nil || v.Allowed(e, m)
}

// qWait: why a job waits, and its place.
type qWait struct {
	Position int32
	Reason   string // capacity | behind | quota | preempting
	Detail   map[string]string
	Expected *time.Time
}

type queuePlan struct {
	Starts  []qStart
	Preempt []*devpodv1alpha1.Job
	Waits   map[string]qWait
}

// freeFor: what machine m has for [from, from+d) — what it has, less its
// reservations' peak then, the interactive DevPods and the jobs still
// running at from.
func freeFor(m qMachine, running []qRun, from time.Time, d time.Duration) amount {
	free := m.Cap.sub(peakIn(m.Rs, m.Cap, from, from.Add(d))).sub(m.Interactive)
	for _, r := range running {
		if r.Until.After(from) && slices.Contains(r.Machines, m.Name) {
			free = free.sub(r.Ask)
		}
	}
	return free
}

// queueOrder: priority, then who came first.
func queueOrder(es []qEntry) []qEntry {
	out := slices.Clone(es)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		return a.Key < b.Key
	})
	return out
}

func planQueue(v queueView) queuePlan {
	plan := queuePlan{Waits: map[string]qWait{}}
	runs := slices.Clone(v.Running)
	var head *qEntry
	var promise time.Time
	promised := false
	var headOn []string // the machines the head is promised
	var planned []qEntry
	var position int32
	wait := func(e qEntry, w qWait) {
		position++
		w.Position = position
		plan.Waits[e.Key] = w
	}
	var clears map[string]time.Time // where the entry at hand would be cleared, when
	start := func(e qEntry, machines []string) {
		st := qStart{Entry: e, Machines: machines}
		if t, ok := clears[machines[0]]; ok && e.Nodes == 1 {
			st.ClearsAt = &t
		}
		plan.Starts = append(plan.Starts, st)
		planned = append(planned, e)
		runs = append(runs, qRun{Key: e.Key, Job: e.Job, Machines: machines, Ask: e.Ask, Until: e.until(v.Now),
			Preemptible: e.Preemptible, Priority: e.Priority, Started: v.Now})
	}
	var held []string // machines kept for entries ahead held to one machine
	notHeld := func(ms []string) []string {
		return slices.DeleteFunc(ms, func(m string) bool { return slices.Contains(held, m) })
	}
	for _, j := range queueOrder(v.Queued) {
		n := int(j.Nodes)
		limit := j.Limit
		cands := notHeld(v.candidates(j, runs, v.Now))
		clears = nil
		if len(cands) < n && j.Limit <= 0 {
			// no end, and nowhere beside every reservation known: where the
			// first in its way comes latest
			cands, clears = v.soonCleared(j, runs)
			cands = notHeld(cands)
		}
		fits := len(cands) >= n
		quota := v.QuotaOK == nil || v.QuotaOK(j, planned)
		if !fits && !v.everPossible(j) {
			// no machine it may have could ever take it (too big, its
			// machine gone or cordoned): it waits, holding nobody up
			wait(j, qWait{Reason: "never"})
			continue
		}
		if head != nil {
			// behind the head (EASY backfill): a job may start now on
			// machines the head holds nothing on, or on any if it has ended
			// by the promise — either way the head is not put off (a head
			// holding machines with no promise has no time to end by)
			away := slices.DeleteFunc(slices.Clone(cands), func(m string) bool { return slices.Contains(headOn, m) })
			anywhere := len(headOn) == 0 || (promised && limit > 0 && !v.Now.Add(limit).After(promise))
			switch {
			case fits && quota && anywhere:
				start(j, cands[:n])
				continue
			case fits && quota && len(away) >= n:
				start(j, away[:n])
				continue
			}
			if fits && !quota {
				wait(j, qWait{Reason: "quota"})
				continue
			}
			wait(j, qWait{Reason: "behind", Detail: map[string]string{"head": head.Key}})
			continue
		}
		switch {
		case fits && quota:
			start(j, cands[:n])
			continue
		case fits:
			wait(j, qWait{Reason: "quota"}) // holds up nobody
			continue
		}
		if j.Only != "" {
			// held to one machine (pinned, or its home's): it keeps that one
			// for itself, and is not the head — the next that cannot start is
			held = append(held, j.Only)
			w := qWait{Reason: "capacity", Detail: v.shortfall(j, runs)}
			if t, _, ok := v.earliest(j, runs); ok {
				w.Expected = &t
			}
			wait(j, w)
			continue
		}
		head = &j
		// one whose ranks once did not all start has nobody stopped for
		// it: it may not start this time either
		victims, on := []qRun(nil), []string(nil)
		if !j.NoVictims {
			victims, on = v.victimsFor(j, runs)
		}
		if len(victims) > 0 {
			ids := make([]string, 0, len(victims))
			for _, r := range victims {
				plan.Preempt = append(plan.Preempt, r.Job)
				ids = append(ids, r.Key)
			}
			runs = slices.DeleteFunc(runs, func(r qRun) bool { return slices.Contains(ids, r.Key) })
			promise, promised, headOn = v.Now, true, on // it takes what they leave: nobody slips in there
			wait(j, qWait{Reason: "preempting", Detail: map[string]string{"jobs": strings.Join(ids, ",")}})
			continue
		}
		w := qWait{Reason: "capacity", Detail: v.shortfall(j, runs)}
		if t, on, ok := v.earliest(j, runs); ok {
			promise, promised, headOn = t, true, on
			w.Expected = &t
		} else {
			// no time in sight (what is in its way has no end): it holds
			// the machines nearest to taking it all the same
			headOn = v.nearest(j, runs)
		}
		wait(j, w)
	}
	return plan
}

// candidates: the machines one of j's ranks could run on from at for its
// time limit, the snuggest first.
func (v queueView) candidates(j qEntry, runs []qRun, at time.Time) []string {
	ask := j.Ask
	type cand struct {
		name string
		left amount
	}
	var cs []cand
	for _, m := range v.Machines {
		if !v.mayHave(j, m.Name) {
			continue
		}
		if f := freeFor(m, runs, at, v.window(j)); ask.fits(f) {
			cs = append(cs, cand{m.Name, f.sub(ask)})
		}
	}
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].left.GPU != cs[b].left.GPU {
			return cs[a].left.GPU < cs[b].left.GPU
		}
		if cs[a].left.CPU != cs[b].left.CPU {
			return cs[a].left.CPU < cs[b].left.CPU
		}
		return cs[a].name < cs[b].name
	})
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.name
	}
	return out
}

// earliest: the first time (a running job's end, a reservation's start or
// end) within the horizon from which j's ranks would all fit, and the
// machines they would take then.
func (v queueView) earliest(j qEntry, runs []qRun) (time.Time, []string, bool) {
	end := v.Now.Add(v.Horizon)
	var events []time.Time
	add := func(t time.Time) {
		if t.After(v.Now) && !t.After(end) {
			events = append(events, t)
		}
	}
	for _, r := range runs {
		add(r.Until)
	}
	for _, m := range v.Machines {
		for _, nr := range m.Rs {
			add(nr.Spec.Start.Time)
			add(overAt(nr))
		}
	}
	slices.SortFunc(events, func(a, b time.Time) int { return a.Compare(b) })
	for _, t := range slices.Compact(events) {
		if cs := v.candidates(j, runs, t); len(cs) >= int(j.Nodes) {
			return t, cs[:j.Nodes], true
		}
	}
	return time.Time{}, nil, false
}

// victimsFor: the fewest running jobs, preemptible and of lower priority
// than j, whose stopping lets j's ranks start now — chosen machine by
// machine (those needing the fewest stopped first) — and the machines j
// would take; none when that is not enough.
func (v queueView) victimsFor(j qEntry, runs []qRun) ([]qRun, []string) {
	ask := j.Ask
	type option struct {
		machine string
		victims []qRun
		prio    int64
	}
	var opts []option
	for _, m := range v.Machines {
		if !v.mayHave(j, m.Name) {
			continue
		}
		f := freeFor(m, runs, v.Now, v.window(j))
		if ask.fits(f) {
			opts = append(opts, option{machine: m.Name})
			continue
		}
		var cands []qRun
		for _, r := range runs {
			if r.Preemptible && r.Job != nil && r.Priority < j.Priority && slices.Contains(r.Machines, m.Name) {
				cands = append(cands, r)
			}
		}
		sort.SliceStable(cands, func(a, b int) bool {
			if cands[a].Priority != cands[b].Priority {
				return cands[a].Priority < cands[b].Priority
			}
			if cands[a].Ask.GPU != cands[b].Ask.GPU {
				return cands[a].Ask.GPU > cands[b].Ask.GPU
			}
			return cands[a].Started.After(cands[b].Started)
		})
		var vs []qRun
		var prio int64
		for _, c := range cands {
			vs = append(vs, c)
			prio += int64(c.Priority)
			if f = f.add(c.Ask); ask.fits(f) {
				opts = append(opts, option{m.Name, vs, prio})
				break
			}
		}
	}
	if len(opts) < int(j.Nodes) {
		return nil, nil
	}
	sort.SliceStable(opts, func(a, b int) bool {
		if len(opts[a].victims) != len(opts[b].victims) {
			return len(opts[a].victims) < len(opts[b].victims)
		}
		if opts[a].prio != opts[b].prio {
			return opts[a].prio < opts[b].prio
		}
		return opts[a].machine < opts[b].machine
	})
	var out []qRun
	var on []string
	for _, o := range opts[:j.Nodes] {
		on = append(on, o.machine)
		for _, r := range o.victims {
			if !slices.ContainsFunc(out, func(x qRun) bool { return x.Key == r.Key }) {
				out = append(out, r)
			}
		}
	}
	return out, on
}

// shortfall: what j needs in all, what its machines have free now, and
// how many of them have room for one rank (free cards spread thin show),
// for saying why it waits.
func (v queueView) shortfall(j qEntry, runs []qRun) map[string]string {
	ask := j.Ask
	var free amount
	for _, m := range v.Machines {
		if !v.mayHave(j, m.Name) {
			continue
		}
		free = free.add(freeFor(m, runs, v.Now, v.window(j)).clamped())
	}
	n := int64(j.Nodes)
	return map[string]string{
		"need": strconv.FormatInt(ask.GPU*n, 10), "free": strconv.FormatInt(free.GPU, 10),
		"needCPU": coresOf(ask.CPU * n), "freeCPU": coresOf(free.CPU),
		"fit": strconv.Itoa(len(v.candidates(j, runs, v.Now))),
	}
}

// soonCleared: for e with no end, the machines it fits on now though a
// reservation known will want its room — the one whose first such
// reservation comes latest first, then the snuggest — and when each
// would clear it.
func (v queueView) soonCleared(e qEntry, runs []qRun) ([]string, map[string]time.Time) {
	type cand struct {
		name string
		at   time.Time
		left amount
	}
	var cs []cand
	for _, m := range v.Machines {
		if !v.mayHave(e, m.Name) {
			continue
		}
		now := freeFor(m, runs, v.Now, time.Millisecond)
		if !e.Ask.fits(now) {
			continue
		}
		var starts []time.Time
		for _, nr := range m.Rs {
			if t := nr.Spec.Start.Time; t.After(v.Now) && !t.After(v.Now.Add(v.Horizon)) {
				starts = append(starts, t)
			}
		}
		slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
		for _, t := range starts {
			if !e.Ask.fits(freeFor(m, runs, t, time.Millisecond)) {
				cs = append(cs, cand{m.Name, t, now.sub(e.Ask)})
				break
			}
		}
	}
	sort.Slice(cs, func(a, b int) bool {
		if !cs[a].at.Equal(cs[b].at) {
			return cs[a].at.After(cs[b].at)
		}
		if cs[a].left.GPU != cs[b].left.GPU {
			return cs[a].left.GPU < cs[b].left.GPU
		}
		return cs[a].name < cs[b].name
	})
	names := make([]string, len(cs))
	at := map[string]time.Time{}
	for i, c := range cs {
		names[i], at[c.name] = c.name, c.at
	}
	return names, at
}

// nearest: the machines e may have that could ever take one of its ranks,
// those one rank lacks the least on now first (cards, then cores), as
// many as it has ranks — what a head no time can be promised holds.
func (v queueView) nearest(e qEntry, runs []qRun) []string {
	type cand struct {
		name string
		lack amount
	}
	var cs []cand
	for _, m := range v.Machines {
		if !v.mayHave(e, m.Name) || !e.Ask.fits(m.Cap) {
			continue
		}
		free := freeFor(m, runs, v.Now, v.window(e)).clamped()
		cs = append(cs, cand{m.Name, e.Ask.sub(free).clamped()})
	}
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].lack.GPU != cs[b].lack.GPU {
			return cs[a].lack.GPU < cs[b].lack.GPU
		}
		if cs[a].lack.CPU != cs[b].lack.CPU {
			return cs[a].lack.CPU < cs[b].lack.CPU
		}
		return cs[a].name < cs[b].name
	})
	var out []string
	for i := 0; i < len(cs) && i < int(e.Nodes); i++ {
		out = append(out, cs[i].name)
	}
	return out
}

// everPossible: some machine e may have could ever take one of its ranks
// (load aside), and as many as it has ranks.
func (v queueView) everPossible(e qEntry) bool {
	n := 0
	for _, m := range v.Machines {
		if v.mayHave(e, m.Name) && e.Ask.fits(m.Cap) {
			n++
		}
	}
	return n >= int(e.Nodes)
}
