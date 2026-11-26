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

// qMachine: a reservable machine — what it has, what interactive DevPods
// take of it outside their owners' reservations, its reservations.
type qMachine struct {
	Name             string
	Cap, Interactive amount
	Rs               []*devpodv1alpha1.NodeReservation
}

// qRun: a job running (or starting) — where, how much on each machine,
// until when at most.
type qRun struct {
	Job         *devpodv1alpha1.Job
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
	Queued   []*devpodv1alpha1.Job
	// Allowed: the access rules let job's owner run its ranks on machine.
	Allowed func(job *devpodv1alpha1.Job, machine string) bool
	// QuotaOK: the job's ranks fit its owner's quota now.
	QuotaOK func(job *devpodv1alpha1.Job) bool
	// Horizon: how far ahead a promise is looked for.
	Horizon time.Duration
}

type qStart struct {
	Job      *devpodv1alpha1.Job
	Machines []string
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

// queueOrder: priority, then who submitted first.
func queueOrder(jobs []*devpodv1alpha1.Job) []*devpodv1alpha1.Job {
	out := slices.Clone(jobs)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Spec.Priority != b.Spec.Priority {
			return a.Spec.Priority > b.Spec.Priority
		}
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Name < b.Name
	})
	return out
}

func planQueue(v queueView) queuePlan {
	plan := queuePlan{Waits: map[string]qWait{}}
	runs := slices.Clone(v.Running)
	var head *devpodv1alpha1.Job
	var promise time.Time
	promised := false
	var position int32
	wait := func(j *devpodv1alpha1.Job, w qWait) {
		position++
		w.Position = position
		plan.Waits[j.Name] = w
	}
	start := func(j *devpodv1alpha1.Job, machines []string) {
		plan.Starts = append(plan.Starts, qStart{Job: j, Machines: machines})
		runs = append(runs, qRun{Job: j, Machines: machines, Ask: jobAsk(j), Until: v.Now.Add(j.Spec.TimeLimit.Duration),
			Preemptible: j.Spec.Preemptible, Priority: j.Spec.Priority, Started: v.Now})
	}
	for _, j := range queueOrder(v.Queued) {
		n := int(j.Spec.Nodes)
		limit := j.Spec.TimeLimit.Duration
		cands := v.candidates(j, runs, v.Now)
		fits := len(cands) >= n
		quota := v.QuotaOK == nil || v.QuotaOK(j)
		if head != nil {
			if fits && quota && (!promised || !v.Now.Add(limit).After(promise)) {
				start(j, cands[:n])
				continue
			}
			if fits && !quota {
				wait(j, qWait{Reason: "quota"})
				continue
			}
			wait(j, qWait{Reason: "behind", Detail: map[string]string{"head": head.Name}})
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
		head = j
		if victims := v.victimsFor(j, runs); len(victims) > 0 {
			ids := make([]string, 0, len(victims))
			for _, r := range victims {
				plan.Preempt = append(plan.Preempt, r.Job)
				ids = append(ids, r.Job.Name)
			}
			runs = slices.DeleteFunc(runs, func(r qRun) bool { return slices.Contains(ids, r.Job.Name) })
			promise, promised = v.Now, true // it takes what they leave: nobody slips in
			wait(j, qWait{Reason: "preempting", Detail: map[string]string{"jobs": strings.Join(ids, ",")}})
			continue
		}
		w := qWait{Reason: "capacity", Detail: v.shortfall(j, runs)}
		if t, ok := v.earliest(j, runs); ok {
			promise, promised = t, true
			w.Expected = &t
		}
		wait(j, w)
	}
	return plan
}

// candidates: the machines one of j's ranks could run on from at for its
// time limit, the snuggest first.
func (v queueView) candidates(j *devpodv1alpha1.Job, runs []qRun, at time.Time) []string {
	ask := jobAsk(j)
	type cand struct {
		name string
		left amount
	}
	var cs []cand
	for _, m := range v.Machines {
		if v.Allowed != nil && !v.Allowed(j, m.Name) {
			continue
		}
		if f := freeFor(m, runs, at, j.Spec.TimeLimit.Duration); ask.fits(f) {
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
// end) within the horizon from which j's ranks would all fit.
func (v queueView) earliest(j *devpodv1alpha1.Job, runs []qRun) (time.Time, bool) {
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
		if len(v.candidates(j, runs, t)) >= int(j.Spec.Nodes) {
			return t, true
		}
	}
	return time.Time{}, false
}

// victimsFor: the fewest running jobs, preemptible and of lower priority
// than j, whose stopping lets j's ranks start now — chosen machine by
// machine (those needing the fewest stopped first); none when that is
// not enough.
func (v queueView) victimsFor(j *devpodv1alpha1.Job, runs []qRun) []qRun {
	ask := jobAsk(j)
	type option struct {
		machine string
		victims []qRun
		prio    int64
	}
	var opts []option
	for _, m := range v.Machines {
		if v.Allowed != nil && !v.Allowed(j, m.Name) {
			continue
		}
		f := freeFor(m, runs, v.Now, j.Spec.TimeLimit.Duration)
		if ask.fits(f) {
			opts = append(opts, option{machine: m.Name})
			continue
		}
		var cands []qRun
		for _, r := range runs {
			if r.Preemptible && r.Priority < j.Spec.Priority && slices.Contains(r.Machines, m.Name) {
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
	if len(opts) < int(j.Spec.Nodes) {
		return nil
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
	for _, o := range opts[:j.Spec.Nodes] {
		for _, r := range o.victims {
			if !slices.ContainsFunc(out, func(x qRun) bool { return x.Job.Name == r.Job.Name }) {
				out = append(out, r)
			}
		}
	}
	return out
}

// shortfall: what j needs in all and what its machines have free now, for
// saying why it waits.
func (v queueView) shortfall(j *devpodv1alpha1.Job, runs []qRun) map[string]string {
	ask := jobAsk(j)
	var free amount
	for _, m := range v.Machines {
		if v.Allowed != nil && !v.Allowed(j, m.Name) {
			continue
		}
		free = free.add(freeFor(m, runs, v.Now, j.Spec.TimeLimit.Duration).clamped())
	}
	n := int64(j.Spec.Nodes)
	return map[string]string{
		"need": strconv.FormatInt(ask.GPU*n, 10), "free": strconv.FormatInt(free.GPU, 10),
		"needCPU": coresOf(ask.CPU * n), "freeCPU": coresOf(free.CPU),
	}
}
