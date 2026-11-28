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
	Models           []string // its cards' (gpures.Models)
}

// qEntry: something waiting in the line — a job, or a DevPod to start.
type qEntry struct {
	Key         string                 // a job's id, or "devpod/<name>"
	Job         *devpodv1alpha1.Job    // nil: a DevPod
	DevPod      *devpodv1alpha1.DevPod // nil: a job
	Owner       string
	Priority    int32
	Since       time.Time     // its place in time
	Nodes       int32         // fixed: how many machines (a DevPod: 1); 0: packed, Tasks on as few as fit
	PerMachine  int32         // fixed: ranks on each (0: one); packed: the most on one (0: any)
	Tasks       int32         // packed: how many ranks
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
		Nodes: j.Spec.Nodes, PerMachine: j.Spec.PerMachine(), Tasks: j.Spec.Ranks(),
		Ask: jobAsk(j), Limit: j.Spec.TimeLimit.Duration, Preemptible: j.Spec.Preemptible, NoVictims: j.Status.GangTimeouts > 0}
}

// ranks: how many ranks e has.
func (e qEntry) ranks() int32 {
	if e.Nodes > 0 {
		return e.Nodes * max(e.PerMachine, 1)
	}
	return e.Tasks
}

// perMachine: the ranks of e one machine takes — fixed, exactly so many;
// packed, at most (0: as many as fit).
func (e qEntry) perMachine() int64 {
	if e.Nodes > 0 {
		return int64(max(e.PerMachine, 1))
	}
	return int64(e.PerMachine)
}

// slots: how many ranks asking ask fit in free.
func slots(free, ask amount) int64 {
	if !ask.fits(free) {
		return 0
	}
	n := int64(1 << 20)
	if ask.GPU > 0 {
		n = min(n, free.GPU/ask.GPU)
	}
	if ask.CPU > 0 {
		n = min(n, free.CPU/ask.CPU)
	}
	return n
}

// times: k of a.
func (a amount) times(k int64) amount { return amount{a.GPU * k, a.CPU * k} }

// count: how many of ms are m (a job's ranks on machine m).
func count(ms []string, m string) int64 {
	var n int64
	for _, x := range ms {
		if x == m {
			n++
		}
	}
	return n
}

// qRun: a job running (or starting) — where, how much on each machine,
// until when at most.
type qRun struct {
	Key         string              // as its entry's
	Job         *devpodv1alpha1.Job // nil: a DevPod started by this plan
	Machines    []string            // each rank's (a machine again for each more rank on it)
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
		if r.Until.After(from) {
			free = free.sub(r.Ask.times(count(r.Machines, m.Name)))
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
		if t, ok := clears[machines[0]]; ok && e.ranks() == 1 {
			st.ClearsAt = &t
		}
		plan.Starts = append(plan.Starts, st)
		planned = append(planned, e)
		runs = append(runs, qRun{Key: e.Key, Job: e.Job, Machines: machines, Ask: e.Ask, Until: e.until(v.Now),
			Preemptible: e.Preemptible, Priority: e.Priority, Started: v.Now})
	}
	var held []string // machines kept for entries ahead held to one machine
	for _, j := range queueOrder(v.Queued) {
		limit := j.Limit
		clears = nil
		// where its ranks would go now, on none of exclude — one with no
		// end, nowhere beside every reservation known: where the first in
		// its way comes latest
		place := func(exclude []string) []string {
			if p := v.choose(j, runs, v.Now, exclude); p != nil {
				return p
			}
			if j.Limit <= 0 && j.ranks() == 1 {
				cands, at := v.soonCleared(j, runs)
				cands = slices.DeleteFunc(cands, func(m string) bool { return slices.Contains(exclude, m) })
				if len(cands) > 0 {
					clears = at
					return cands[:1]
				}
			}
			return nil
		}
		placed := place(held)
		fits := placed != nil
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
			anywhere := len(headOn) == 0 || (promised && limit > 0 && !v.Now.Add(limit).After(promise))
			var away []string
			if fits && quota && !anywhere {
				away = place(append(slices.Clone(held), headOn...))
			}
			switch {
			case fits && quota && anywhere:
				start(j, placed)
				continue
			case away != nil:
				start(j, away)
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
			start(j, placed)
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

// choose: the machine of each of e's ranks from at for its time limit,
// on none of exclude — fixed, Nodes machines taking PerMachine each, the
// snuggest after them first; packed, the snuggest one machine taking them
// all, else the machines taking the most first (as few as can), at most
// PerMachine on one. A machine's ranks are in a row; nil: they do not all
// fit.
func (v queueView) choose(e qEntry, runs []qRun, at time.Time, exclude []string) []string {
	type cand struct {
		name string
		n    int64 // ranks it takes
		free amount
	}
	per := e.perMachine()
	var cs []cand
	for _, m := range v.Machines {
		if !v.mayHave(e, m.Name) || slices.Contains(exclude, m.Name) {
			continue
		}
		f := freeFor(m, runs, at, v.window(e))
		n := slots(f, e.Ask)
		if per > 0 {
			n = min(n, per)
		}
		if n == 0 || (e.Nodes > 0 && n < per) {
			continue
		}
		cs = append(cs, cand{m.Name, n, f})
	}
	snuggest := func(cs []cand, k int64) {
		sort.Slice(cs, func(a, b int) bool {
			la, lb := cs[a].free.sub(e.Ask.times(k)), cs[b].free.sub(e.Ask.times(k))
			if la.GPU != lb.GPU {
				return la.GPU < lb.GPU
			}
			if la.CPU != lb.CPU {
				return la.CPU < lb.CPU
			}
			return cs[a].name < cs[b].name
		})
	}
	var out []string
	put := func(name string, k int64) {
		for range k {
			out = append(out, name)
		}
	}
	if e.Nodes > 0 {
		if len(cs) < int(e.Nodes) {
			return nil
		}
		snuggest(cs, per)
		for _, c := range cs[:e.Nodes] {
			put(c.name, per)
		}
		return out
	}
	t := int64(e.Tasks)
	if t <= 0 {
		return nil
	}
	var whole []cand
	for _, c := range cs {
		if c.n >= t {
			whole = append(whole, c)
		}
	}
	if len(whole) > 0 {
		snuggest(whole, t)
		put(whole[0].name, t)
		return out
	}
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].n != cs[b].n {
			return cs[a].n > cs[b].n
		}
		return cs[a].name < cs[b].name
	})
	for _, c := range cs {
		put(c.name, min(c.n, t-int64(len(out))))
		if int64(len(out)) == t {
			return out
		}
	}
	return nil
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
		if p := v.choose(j, runs, t, nil); p != nil {
			return t, p, true
		}
	}
	return time.Time{}, nil, false
}

// victimsFor: the fewest running jobs, preemptible and of lower priority
// than j, whose stopping lets j's ranks start now — chosen machine by
// machine (those needing the fewest stopped first) — and the machine of
// each rank j would have; none when that is not enough. A machine is made
// room on for its share (fixed); a packed j is packedVictims'.
func (v queueView) victimsFor(j qEntry, runs []qRun) ([]qRun, []string) {
	if j.Nodes == 0 {
		return v.packedVictims(j, runs)
	}
	ask := j.Ask
	per := j.perMachine()
	type option struct {
		machine string
		victims []qRun
		prio    int64
		ranks   int64
	}
	var opts []option
	for _, m := range v.Machines {
		if !v.mayHave(j, m.Name) {
			continue
		}
		want := per
		f := freeFor(m, runs, v.Now, v.window(j))
		have := min(slots(f, ask), want)
		if have >= want {
			opts = append(opts, option{machine: m.Name, ranks: have})
		}
		if have >= want {
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
			if f = f.add(c.Ask.times(count(c.Machines, m.Name))); slots(f, ask) >= want {
				opts = append(opts, option{m.Name, slices.Clone(vs), prio, want})
				break
			}
		}
	}
	byFewest := func(a, b option) bool {
		if len(a.victims) != len(b.victims) {
			return len(a.victims) < len(b.victims)
		}
		if a.prio != b.prio {
			return a.prio < b.prio
		}
		if a.ranks != b.ranks {
			return a.ranks > b.ranks
		}
		return a.machine < b.machine
	}
	sort.SliceStable(opts, func(a, b int) bool { return byFewest(opts[a], opts[b]) })
	// the options taken: a machine's taken again for more ranks (more
	// stopped there) stands for its first
	chosen := map[string]option{}
	var order []string
	var total int64
	need := int64(j.ranks())
	for _, o := range opts {
		if total >= need || (j.Nodes > 0 && len(order) == int(j.Nodes)) {
			break
		}
		if c, ok := chosen[o.machine]; ok {
			if o.ranks <= c.ranks {
				continue
			}
			total += o.ranks - c.ranks
		} else {
			order = append(order, o.machine)
			total += o.ranks
		}
		chosen[o.machine] = o
	}
	if total < need {
		return nil, nil
	}
	var out []qRun
	var on []string
	left := need
	for _, m := range order {
		o := chosen[m]
		k := min(o.ranks, left)
		left -= k
		for range k {
			on = append(on, m)
		}
		for _, r := range o.victims {
			if !slices.ContainsFunc(out, func(x qRun) bool { return x.Key == r.Key }) {
				out = append(out, r)
			}
		}
	}
	return out, on
}

// victimSteps: the preemptible runs of lower priority than j on machine
// m, those to stop first first, as steps — after each, the victims so far
// and the ranks of j m then takes (at most limit) — from none stopped.
func (v queueView) victimSteps(j qEntry, m qMachine, runs []qRun, limit int64) []victimStep {
	f := freeFor(m, runs, v.Now, v.window(j))
	steps := []victimStep{{ranks: min(slots(f, j.Ask), limit)}}
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
		if steps[len(steps)-1].ranks >= limit {
			break
		}
		vs = append(vs, c)
		prio += int64(c.Priority)
		f = f.add(c.Ask.times(count(c.Machines, m.Name)))
		if n := min(slots(f, j.Ask), limit); n > steps[len(steps)-1].ranks {
			steps = append(steps, victimStep{victims: slices.Clone(vs), ranks: n, prio: prio})
		}
	}
	return steps
}

type victimStep struct {
	victims []qRun
	ranks   int64
	prio    int64
}

// packedVictims: victimsFor of a packed j. Each machine's steps (victims
// stopped, ranks it then takes) are advanced one at a time — the step
// that gains the most ranks for each one stopped first — until all of j's
// ranks have room; then each machine is given its ranks (those stopping
// nobody first) and stops only what its share needs.
func (v queueView) packedVictims(j qEntry, runs []qRun) ([]qRun, []string) {
	need := int64(j.Tasks)
	limit := need
	if per := j.perMachine(); per > 0 {
		limit = min(limit, per)
	}
	type machine struct {
		name  string
		steps []victimStep
		at    int
	}
	var ms []*machine
	var total int64
	for _, m := range v.Machines {
		if !v.mayHave(j, m.Name) {
			continue
		}
		steps := v.victimSteps(j, m, runs, limit)
		ms = append(ms, &machine{name: m.Name, steps: steps})
		total += steps[0].ranks
	}
	for total < need {
		var best *machine
		var gain, added int64
		for _, m := range ms {
			if m.at+1 >= len(m.steps) {
				continue
			}
			cur, next := m.steps[m.at], m.steps[m.at+1]
			g, a := next.ranks-cur.ranks, int64(len(next.victims)-len(cur.victims))
			// more ranks for each one stopped; then fewer stopped, the lower
			// their priority, by name
			if best == nil || g*added > gain*a || (g*added == gain*a && (a < added || (a == added && m.name < best.name))) {
				best, gain, added = m, g, a
			}
		}
		if best == nil {
			return nil, nil
		}
		total += gain
		best.at++
	}
	sort.SliceStable(ms, func(a, b int) bool {
		va, vb := len(ms[a].steps[ms[a].at].victims), len(ms[b].steps[ms[b].at].victims)
		if va != vb {
			return va < vb
		}
		if ra, rb := ms[a].steps[ms[a].at].ranks, ms[b].steps[ms[b].at].ranks; ra != rb {
			return ra > rb
		}
		return ms[a].name < ms[b].name
	})
	var out []qRun
	var on []string
	left := need
	for _, m := range ms {
		k := min(m.steps[m.at].ranks, left)
		if k == 0 {
			continue
		}
		left -= k
		for range k {
			on = append(on, m.name)
		}
		// the fewest stopped that give it k
		i := slices.IndexFunc(m.steps, func(s victimStep) bool { return s.ranks >= k })
		for _, r := range m.steps[i].victims {
			if !slices.ContainsFunc(out, func(x qRun) bool { return x.Key == r.Key }) {
				out = append(out, r)
			}
		}
	}
	return out, on
}

// shortfall: what j needs in all, what its machines have free now, and
// how much of it fits (fixed: the machines with room for their share;
// packed: the ranks), for saying why it waits.
func (v queueView) shortfall(j qEntry, runs []qRun) map[string]string {
	ask := j.Ask
	per := j.perMachine()
	var free amount
	var fit int64
	for _, m := range v.Machines {
		if !v.mayHave(j, m.Name) {
			continue
		}
		f := freeFor(m, runs, v.Now, v.window(j))
		free = free.add(f.clamped())
		n := slots(f, ask)
		switch {
		case j.Nodes > 0 && n >= per:
			fit++
		case j.Nodes == 0 && per > 0:
			fit += min(n, per)
		case j.Nodes == 0:
			fit += n
		}
	}
	n := int64(j.ranks())
	return map[string]string{
		"need": strconv.FormatInt(ask.GPU*n, 10), "free": strconv.FormatInt(free.GPU, 10),
		"needCPU": coresOf(ask.CPU * n), "freeCPU": coresOf(free.CPU),
		"fit": strconv.FormatInt(fit, 10),
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

// nearest: the machines e may have that could ever take its share (one
// rank, its PerMachine fixed), those one rank lacks the least on now first
// (cards, then cores) — as many as it needs: what a head no time can be
// promised holds.
func (v queueView) nearest(e qEntry, runs []qRun) []string {
	type cand struct {
		name string
		lack amount
		hold int64 // ranks it holds empty
	}
	per := e.perMachine()
	var cs []cand
	for _, m := range v.Machines {
		hold := slots(m.Cap, e.Ask)
		if per > 0 {
			hold = min(hold, per)
		}
		if !v.mayHave(e, m.Name) || hold == 0 || (e.Nodes > 0 && hold < per) {
			continue
		}
		free := freeFor(m, runs, v.Now, v.window(e)).clamped()
		cs = append(cs, cand{m.Name, e.Ask.sub(free).clamped(), hold})
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
	var held int64
	for _, c := range cs {
		if (e.Nodes > 0 && len(out) >= int(e.Nodes)) || (e.Nodes == 0 && held >= int64(e.Tasks)) {
			break
		}
		out = append(out, c.name)
		held += c.hold
	}
	return out
}

// everPossible: machines e may have could ever take its ranks (load
// aside) — fixed, enough that take its share; packed, room for all its
// ranks between them.
func (v queueView) everPossible(e qEntry) bool {
	per := e.perMachine()
	var machines, ranks int64
	for _, m := range v.Machines {
		if !v.mayHave(e, m.Name) {
			continue
		}
		n := slots(m.Cap, e.Ask)
		if per > 0 {
			n = min(n, per)
		}
		if e.Nodes > 0 && n >= per {
			machines++
		}
		ranks += n
	}
	if e.Nodes > 0 {
		return machines >= int64(e.Nodes)
	}
	return ranks >= int64(e.Tasks)
}
