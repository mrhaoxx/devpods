// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

var planNow = time.Date(2026, 11, 27, 2, 0, 0, 0, time.UTC)

// qjob: a job named name of priority prio — nodes × gpus cards (24 cores
// each), at most limit, submitted at planNow+after.
func qjob(name string, prio, nodes, gpus int32, limit, after time.Duration) *devpodv1alpha1.Job {
	return &devpodv1alpha1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(planNow.Add(after))},
		Spec: devpodv1alpha1.JobSpec{Owner: "u", Nodes: nodes, GPUsPerNode: gpus, CPUsPerNode: resource.MustParse("24"),
			TimeLimit: metav1.Duration{Duration: limit}, Priority: prio},
	}
}

func machine8(name string) qMachine { return qMachine{Name: name, Cap: amount{8, 192000}} }

func qrun(name string, prio int32, preemptible bool, gpus int64, until time.Duration, started time.Duration, machines ...string) qRun {
	j := qjob(name, prio, int32(len(machines)), int32(gpus), until, started)
	j.Spec.Preemptible = preemptible
	return qRun{Key: name, Job: j, Machines: machines, Ask: amount{gpus, 24000}, Until: planNow.Add(until), Preemptible: preemptible, Priority: prio, Started: planNow.Add(started)}
}

func view(ms []qMachine, runs []qRun, queued ...*devpodv1alpha1.Job) queueView {
	es := make([]qEntry, len(queued))
	for i, j := range queued {
		es[i] = jobEntry(j)
	}
	return queueView{Now: planNow, Machines: ms, Running: runs, Queued: es, Horizon: 14 * 24 * time.Hour,
		Allowed: func(qEntry, string) bool { return true }, QuotaOK: func(qEntry, []qEntry) bool { return true }}
}

func started(p queuePlan, key string) []string {
	for _, s := range p.Starts {
		if s.Entry.Key == key {
			return s.Machines
		}
	}
	return nil
}

// All the ranks start together, or none.
func TestPlanGangAllOrNone(t *testing.T) {
	a := machine8("a")
	a.Interactive = amount{4, 0}
	p := planQueue(view([]qMachine{a, machine8("b")}, nil, qjob("x", 0, 2, 8, time.Hour, 0), qjob("y", 0, 2, 4, time.Hour, time.Minute)))
	if started(p, "x") != nil || p.Waits["x"].Reason != "capacity" || p.Waits["x"].Detail["need"] != "16" || p.Waits["x"].Detail["free"] != "12" || p.Waits["x"].Detail["fit"] != "1" {
		t.Fatalf("x: %v %+v", started(p, "x"), p.Waits["x"])
	}
	if m := started(p, "y"); len(m) != 2 {
		t.Fatalf("y: %v", m)
	}
}

// Higher priority first, though submitted later.
func TestPlanPriorityOrder(t *testing.T) {
	p := planQueue(view([]qMachine{machine8("a")}, nil, qjob("low", 0, 1, 8, time.Hour, 0), qjob("high", 100, 1, 8, time.Hour, time.Minute)))
	if started(p, "high") == nil || started(p, "low") != nil || p.Waits["low"].Position != 1 {
		t.Fatalf("starts %+v waits %+v", p.Starts, p.Waits)
	}
}

// A job behind the head starts only if it ends before the head's promise.
func TestPlanBackfill(t *testing.T) {
	busy := []qRun{qrun("r", 50, false, 8, 2*time.Hour, -time.Hour, "b")}
	ms := []qMachine{machine8("a"), machine8("b")}
	head := qjob("head", 10, 2, 8, time.Hour, 0)
	p := planQueue(view(ms, busy, head, qjob("short", 0, 1, 8, time.Hour, time.Minute)))
	if started(p, "short") == nil || p.Waits["head"].Expected == nil || !p.Waits["head"].Expected.Equal(planNow.Add(2*time.Hour)) {
		t.Fatalf("short starts by the promise: starts %+v head %+v", p.Starts, p.Waits["head"])
	}
	p = planQueue(view(ms, busy, head, qjob("long", 0, 1, 8, 3*time.Hour, time.Minute)))
	if started(p, "long") != nil || p.Waits["long"].Reason != "behind" || p.Waits["long"].Detail["head"] != "head" {
		t.Fatalf("long would delay the head: starts %+v wait %+v", p.Starts, p.Waits["long"])
	}
}

// A reservation ahead takes what it holds from then on.
func TestPlanReservationAhead(t *testing.T) {
	a := machine8("a")
	a.Rs = []*devpodv1alpha1.NodeReservation{{Spec: devpodv1alpha1.NodeReservationSpec{Node: "a", Start: metav1.NewTime(planNow.Add(time.Hour)), End: metav1.NewTime(planNow.Add(3 * time.Hour))}}}
	p := planQueue(view([]qMachine{a}, nil, qjob("two", 0, 1, 8, 2*time.Hour, 0)))
	if started(p, "two") != nil || p.Waits["two"].Expected == nil || !p.Waits["two"].Expected.Equal(planNow.Add(3*time.Hour)) {
		t.Fatalf("2h over a reservation from +1h: %+v", p.Waits["two"])
	}
	if p := planQueue(view([]qMachine{a}, nil, qjob("half", 0, 1, 8, 30*time.Minute, 0))); started(p, "half") == nil {
		t.Fatal("30 minutes before the reservation did not start")
	}
}

// Room is made by stopping as few lower, preemptible jobs as will do —
// never one that is not preemptible.
func TestPlanPreempt(t *testing.T) {
	runs := []qRun{
		qrun("p1", 0, true, 8, 5*time.Hour, -3*time.Hour, "a"),
		qrun("p2", 0, true, 4, 5*time.Hour, -2*time.Hour, "b"),
		qrun("p3", 0, true, 4, 5*time.Hour, -time.Hour, "b"),
		qrun("n1", 0, false, 8, 5*time.Hour, -time.Hour, "c"),
	}
	p := planQueue(view([]qMachine{machine8("a"), machine8("b"), machine8("c")}, runs, qjob("urgent", 100, 1, 8, time.Hour, 0)))
	var names []string
	for _, j := range p.Preempt {
		names = append(names, j.Name)
	}
	if !slices.Equal(names, []string{"p1"}) || p.Waits["urgent"].Reason != "preempting" {
		t.Fatalf("preempt %v wait %+v", names, p.Waits["urgent"])
	}
	// nothing lower and preemptible enough: it waits
	p = planQueue(view([]qMachine{machine8("c")}, runs[3:], qjob("urgent", 100, 1, 8, time.Hour, 0)))
	if len(p.Preempt) != 0 || p.Waits["urgent"].Reason != "capacity" {
		t.Fatalf("only a non-preemptible in the way: %v %+v", p.Preempt, p.Waits["urgent"])
	}
}

// A job over its owner's quota waits, and holds up nobody.
func TestPlanQuota(t *testing.T) {
	v := view([]qMachine{machine8("a")}, nil, qjob("q", 10, 1, 4, time.Hour, 0), qjob("r", 0, 1, 4, time.Hour, time.Minute))
	v.QuotaOK = func(e qEntry, _ []qEntry) bool { return e.Key != "q" }
	p := planQueue(v)
	if p.Waits["q"].Reason != "quota" || started(p, "r") == nil {
		t.Fatalf("quota: %+v starts %+v", p.Waits["q"], p.Starts)
	}
}

// The head's expected start is when enough is free.
func TestPlanExpected(t *testing.T) {
	p := planQueue(view([]qMachine{machine8("a")}, []qRun{qrun("r", 0, false, 8, 90*time.Minute, -time.Hour, "a")}, qjob("next", 0, 1, 8, time.Hour, 0)))
	if w := p.Waits["next"]; w.Expected == nil || !w.Expected.Equal(planNow.Add(90*time.Minute)) || w.Position != 1 {
		t.Fatalf("expected: %+v", w)
	}
}

// Behind a head that waits for one machine, jobs start on the others,
// however long they run: the head is not put off (EASY backfill by
// machine).
func TestPlanBackfillAroundHead(t *testing.T) {
	a, b := machine8("a"), machine8("b")
	runs := []qRun{qrun("r1", 50, false, 8, 2*time.Hour, -time.Hour, "a"), qrun("r2", 50, false, 4, 10*time.Hour, -time.Hour, "b")}
	head := qjob("head", 10, 1, 8, time.Hour, 0)
	long := qjob("long", 0, 1, 2, 24*time.Hour, time.Minute)
	p := planQueue(view([]qMachine{a, b}, runs, head, long))
	if w := p.Waits["head"]; w.Expected == nil || !w.Expected.Equal(planNow.Add(2*time.Hour)) {
		t.Fatalf("head: %+v", w)
	}
	if m := started(p, "long"); !slices.Equal(m, []string{"b"}) {
		t.Fatalf("long runs past the promise on b, which the head does not need: %v (waits %+v)", m, p.Waits["long"])
	}
	// where it fits only on the head's machine, it would put the head off:
	// it waits (a has 4 free now, the head wants all 8 there at 2h)
	half := qrun("r1", 50, false, 4, 2*time.Hour, -time.Hour, "a")
	p = planQueue(view([]qMachine{a, machine8("c")}, []qRun{half, qrun("r3", 50, false, 8, 10*time.Hour, -time.Hour, "c")}, head, long))
	if w := p.Waits["head"]; w.Expected == nil || !w.Expected.Equal(planNow.Add(2*time.Hour)) {
		t.Fatalf("head on a: %+v", w)
	}
	if started(p, "long") != nil || p.Waits["long"].Reason != "behind" {
		t.Fatalf("long on the head's machine: %v %+v", started(p, "long"), p.Waits["long"])
	}
}

// The quota counts the jobs the same plan starts before.
func TestPlanQuotaCountsPlanned(t *testing.T) {
	v := view([]qMachine{machine8("a"), machine8("b")}, nil, qjob("one", 0, 1, 8, time.Hour, 0), qjob("two", 0, 1, 8, time.Hour, time.Minute))
	v.QuotaOK = func(_ qEntry, planned []qEntry) bool { return len(planned) == 0 } // room for one
	p := planQueue(v)
	if started(p, "one") == nil || started(p, "two") != nil || p.Waits["two"].Reason != "quota" {
		t.Fatalf("starts %+v waits %+v", p.Starts, p.Waits)
	}
}

// A job whose ranks once did not all start has nobody stopped for it.
func TestPlanNoPreemptAfterGangTimeout(t *testing.T) {
	victim := qrun("low", 0, true, 8, 10*time.Hour, -time.Hour, "a")
	high := qjob("high", 100, 1, 8, time.Hour, 0)
	if p := planQueue(view([]qMachine{machine8("a")}, []qRun{victim}, high)); len(p.Preempt) != 1 {
		t.Fatalf("a first try preempts: %+v", p.Preempt)
	}
	high.Status.GangTimeouts = 1
	p := planQueue(view([]qMachine{machine8("a")}, []qRun{victim}, high))
	if len(p.Preempt) != 0 || p.Waits["high"].Reason != "capacity" {
		t.Fatalf("after a gang timeout: preempt %+v wait %+v", p.Preempt, p.Waits["high"])
	}
}

// qdev: a DevPod to start, of priority prio, asking gpus cards (4 cores),
// queued at planNow+after; it has no end.
func qdev(name string, prio int32, gpus int64, after time.Duration) qEntry {
	return qEntry{Key: "devpod/" + name, Owner: "u", Priority: prio, Since: planNow.Add(after), Nodes: 1, Ask: amount{gpus, 4000}}
}

// entriesView: a view of entries as they are.
func entriesView(ms []qMachine, runs []qRun, es ...qEntry) queueView {
	v := view(ms, runs)
	v.Queued = es
	return v
}

// wholeFrom: a's whole reservation from planNow+at for an hour.
func wholeFrom(m *qMachine, at time.Duration) {
	m.Rs = append(m.Rs, &devpodv1alpha1.NodeReservation{Spec: devpodv1alpha1.NodeReservationSpec{Node: m.Name,
		Start: metav1.NewTime(planNow.Add(at)), End: metav1.NewTime(planNow.Add(at + time.Hour))}})
}

// A DevPod goes to the snuggest machine that has room.
func TestPlanDevPodSnuggest(t *testing.T) {
	b := machine8("b")
	b.Interactive = amount{6, 0}
	p := planQueue(entriesView([]qMachine{machine8("a"), b}, nil, qdev("d", 0, 1, 0)))
	if m := started(p, "devpod/d"); !slices.Equal(m, []string{"b"}) {
		t.Fatalf("started on %v", m)
	}
}

// With no end, it fits beside every reservation known, if it can.
func TestPlanDevPodFitsBesideReservations(t *testing.T) {
	a := machine8("a")
	wholeFrom(&a, 3*24*time.Hour)
	p := planQueue(entriesView([]qMachine{a, machine8("b")}, nil, qdev("d", 0, 1, 0)))
	if m := started(p, "devpod/d"); !slices.Equal(m, []string{"b"}) || p.Starts[0].ClearsAt != nil {
		t.Fatalf("started on %v, clears at %v", m, p.Starts[0].ClearsAt)
	}
}

// Where none can, it goes where the first reservation in its way comes
// latest, and is told when it will be cleared.
func TestPlanDevPodLatestConflict(t *testing.T) {
	a, b := machine8("a"), machine8("b")
	wholeFrom(&a, 3*time.Hour)
	wholeFrom(&b, 2*24*time.Hour)
	p := planQueue(entriesView([]qMachine{a, b}, nil, qdev("d", 0, 1, 0)))
	if m := started(p, "devpod/d"); !slices.Equal(m, []string{"b"}) || p.Starts[0].ClearsAt == nil || !p.Starts[0].ClearsAt.Equal(planNow.Add(2*24*time.Hour)) {
		t.Fatalf("started on %v, clears at %v", m, p.Starts)
	}
}

// One machine of its own: it waits for that one.
func TestPlanDevPodOnlyItsMachine(t *testing.T) {
	a := machine8("a")
	a.Interactive = amount{8, 0}
	d := qdev("d", 0, 1, 0)
	d.Only = "a"
	p := planQueue(entriesView([]qMachine{a, machine8("b")}, nil, d))
	if started(p, "devpod/d") != nil || p.Waits["devpod/d"].Reason != "capacity" {
		t.Fatalf("starts %v wait %+v", p.Starts, p.Waits["devpod/d"])
	}
}

// A machine to avoid is not given, snug as it is.
func TestPlanDevPodAvoid(t *testing.T) {
	b := machine8("b")
	b.Interactive = amount{6, 0}
	d := qdev("d", 0, 1, 0)
	d.Avoid = "b"
	if m := started(planQueue(entriesView([]qMachine{machine8("a"), b}, nil, d)), "devpod/d"); !slices.Equal(m, []string{"a"}) {
		t.Fatalf("started on %v", m)
	}
}

// Behind a head promised a machine, a DevPod (no end: it would never be
// gone by then) starts only elsewhere.
func TestPlanNoEndBehindPromisedHead(t *testing.T) {
	busy := []qRun{qrun("r", 50, false, 4, time.Hour, -time.Hour, "a")} // a: 4 cards till +1h
	head := qjob("head", 10, 1, 8, time.Hour, 0)
	es := []qEntry{jobEntry(head), qdev("d", 0, 1, time.Minute)}
	p := planQueue(entriesView([]qMachine{machine8("a"), machine8("b")}, busy, es...))
	// the head takes b now; with b gone, the DevPod would fit only on a,
	// promised to nobody: it starts there
	if started(p, "head") == nil {
		t.Fatalf("head: %+v", p.Waits["head"])
	}
	b, c := machine8("b"), machine8("c")
	b.Interactive, c.Interactive = amount{8, 0}, amount{2, 0}
	p = planQueue(entriesView([]qMachine{machine8("a"), b, c}, busy, es...))
	// the head fits nowhere now and is promised a (+1h); the DevPod, which
	// a would suit better (4 free to c's 6), starts on c, never on a
	if m := started(p, "devpod/d"); !slices.Equal(m, []string{"c"}) {
		t.Fatalf("DevPod on %v; head %+v", m, p.Waits["head"])
	}
}

// A DevPod at the head stops a lower, preemptible job for its room.
func TestPlanDevPodHeadPreempts(t *testing.T) {
	runs := []qRun{qrun("low", 0, true, 8, 5*time.Hour, -time.Hour, "a")}
	p := planQueue(entriesView([]qMachine{machine8("a")}, runs, qdev("d", 5, 1, 0)))
	if len(p.Preempt) != 1 || p.Preempt[0].Name != "low" {
		t.Fatalf("preempt %v wait %+v", p.Preempt, p.Waits["devpod/d"])
	}
}
