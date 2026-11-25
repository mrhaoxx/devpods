// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

// Reservations hold shares (design 2026-11-26, reservations v2): a
// reservation is some of a reservable machine's cards and cores, or all
// of it, and a machine's reservations never add up to more than it has.
// This is the arithmetic of it — what is booked when, what is free, who
// is in a reservation's way. Memory is not counted.

// amount: GPU cards and CPU millicores.
type amount struct{ GPU, CPU int64 }

func (a amount) add(b amount) amount { return amount{a.GPU + b.GPU, a.CPU + b.CPU} }
func (a amount) sub(b amount) amount { return amount{a.GPU - b.GPU, a.CPU - b.CPU} }

// fits: neither is more than c's.
func (a amount) fits(c amount) bool { return a.GPU <= c.GPU && a.CPU <= c.CPU }

// fitsAsked: fits where a asks anything (a resource a does not ask for
// is not refused for being short).
func (a amount) fitsAsked(c amount) bool {
	return (a.GPU <= 0 || a.GPU <= c.GPU) && (a.CPU <= 0 || a.CPU <= c.CPU)
}

func (a amount) clamped() amount { return amount{max(a.GPU, 0), max(a.CPU, 0)} }

func higher(a, b amount) amount { return amount{max(a.GPU, b.GPU), max(a.CPU, b.CPU)} }

// coresOf: millicores as cores, without trailing zeros.
func coresOf(m int64) string { return strconv.FormatFloat(float64(m)/1000, 'f', -1, 64) }

func (a amount) String() string {
	var parts []string
	switch {
	case a.GPU == 1:
		parts = append(parts, "1 GPU")
	case a.GPU > 1:
		parts = append(parts, fmt.Sprintf("%d GPUs", a.GPU))
	}
	if a.CPU > 0 || a.GPU <= 0 {
		unit := " cores"
		if a.CPU == 1000 {
			unit = " core"
		}
		parts = append(parts, coresOf(a.CPU)+unit)
	}
	return strings.Join(parts, " + ")
}

// view: as the API says amounts — cards, and cores.
func (a amount) view() map[string]any {
	return map[string]any{"gpu": a.GPU, "cpu": float64(a.CPU) / 1000}
}

// capacityOf: what a machine has to reserve — its allocatable.
func capacityOf(n *corev1.Node) amount {
	return amount{int64(gpures.Count(n.Status.Allocatable)), n.Status.Allocatable.Cpu().MilliValue()}
}

// isWhole: the reservation is the whole machine (neither amount said).
func isWhole(nr *devpodv1alpha1.NodeReservation) bool { return nr.Spec.GPU == nil && nr.Spec.CPU == nil }

// reservedOf: what nr holds of a machine with capacity c.
func reservedOf(nr *devpodv1alpha1.NodeReservation, c amount) amount {
	a := c
	if nr.Spec.GPU != nil {
		a.GPU = int64(*nr.Spec.GPU)
	}
	if nr.Spec.CPU != nil {
		a.CPU = nr.Spec.CPU.MilliValue()
	}
	return a
}

// askOfPod: what a Pod takes of a machine, as the scheduler counts it —
// its containers' together, or its largest init container's if more.
// GPUs by their limits; CPU by its requests (a limit with no request: the
// API server makes the request the limit).
func askOfPod(spec *corev1.PodSpec) amount {
	one := func(c *corev1.Container) amount {
		cpu, ok := c.Resources.Requests[corev1.ResourceCPU]
		if !ok {
			cpu = c.Resources.Limits[corev1.ResourceCPU]
		}
		return amount{int64(gpures.Count(c.Resources.Limits)), cpu.MilliValue()}
	}
	var a amount
	for i := range spec.Containers {
		a = a.add(one(&spec.Containers[i]))
	}
	for i := range spec.InitContainers {
		a = higher(a, one(&spec.InitContainers[i]))
	}
	return a
}

// inForce: nr holds its share at t.
func inForce(nr *devpodv1alpha1.NodeReservation, t time.Time) bool {
	return !t.Before(nr.Spec.Start.Time) && t.Before(overAt(nr))
}

// overlaps: nr holds its share at some time in [start, end).
func overlaps(nr *devpodv1alpha1.NodeReservation, start, end time.Time) bool {
	over := overAt(nr)
	return nr.Spec.Start.Time.Before(over) && nr.Spec.Start.Time.Before(end) && start.Before(over)
}

// bookedAt: what rs hold together at t.
func bookedAt(rs []*devpodv1alpha1.NodeReservation, c amount, t time.Time) amount {
	var sum amount
	for _, nr := range rs {
		if inForce(nr, t) {
			sum = sum.add(reservedOf(nr, c))
		}
	}
	return sum
}

// peakIn: the most rs hold together at any time in [from, to) — at from,
// or when one of them starts (a total only grows then).
func peakIn(rs []*devpodv1alpha1.NodeReservation, c amount, from, to time.Time) amount {
	peak := bookedAt(rs, c, from)
	for _, nr := range rs {
		if s := nr.Spec.Start.Time; s.After(from) && s.Before(to) {
			peak = higher(peak, bookedAt(rs, c, s))
		}
	}
	return peak
}

// fitsIn: may a reservation of ask (whole: the whole machine) be had in
// [start, end) beside others — and what is free there, at the least.
func fitsIn(others []*devpodv1alpha1.NodeReservation, c, ask amount, whole bool, start, end time.Time) (bool, amount) {
	if !start.Before(end) {
		return true, c // no time at all (over before from): nothing to fit
	}
	var over []*devpodv1alpha1.NodeReservation
	for _, o := range others {
		if overlaps(o, start, end) {
			if isWhole(o) {
				return false, amount{}
			}
			over = append(over, o)
		}
	}
	peak := peakIn(over, c, start, end)
	free := c.sub(peak).clamped()
	if whole && len(over) > 0 {
		return false, free
	}
	return peak.add(ask).fits(c), free
}

// roomRefusal: why a reservation cannot be had — the most that is free
// in its time, and the earliest start (zero: none ahead) when what it
// asks would be.
type roomRefusal struct {
	Free     amount
	Earliest time.Time
}

// checkRoom: nil when a reservation of ask (whole: the whole machine) for
// [start, end) fits beside others; else what is free, and the earliest
// start, in steps from start, before start+horizon, where it would.
func checkRoom(others []*devpodv1alpha1.NodeReservation, c, ask amount, whole bool, start, end time.Time, step, horizon time.Duration) *roomRefusal {
	ok, free := fitsIn(others, c, ask, whole, start, end)
	if ok {
		return nil
	}
	r := &roomRefusal{Free: free}
	if step <= 0 {
		return r
	}
	d := end.Sub(start)
	for s := start.Add(step); s.Before(start.Add(horizon)); s = s.Add(step) {
		if ok, _ := fitsIn(others, c, ask, whole, s, s.Add(d)); ok {
			r.Earliest = s
			break
		}
	}
	return r
}

// madeBefore: a was made before b (by the API server's clock, then by name).
func madeBefore(a, b *devpodv1alpha1.NodeReservation) bool {
	if ta, tb := a.CreationTimestamp.Time, b.CreationTimestamp.Time; !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return a.Name < b.Name
}

// admitted: nr fits beside those of rs made before it, from from on (what
// is past does not matter) — of two made at once that do not fit
// together, the later gives way.
func admitted(rs []*devpodv1alpha1.NodeReservation, c amount, nr *devpodv1alpha1.NodeReservation, from time.Time) bool {
	var before []*devpodv1alpha1.NodeReservation
	for _, o := range rs {
		if o.Name != nr.Name && madeBefore(o, nr) {
			before = append(before, o)
		}
	}
	ok, _ := fitsIn(before, c, reservedOf(nr, c), isWhole(nr), later(nr.Spec.Start.Time, from), overAt(nr))
	return ok
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// overbooked: by name, those of rs (one machine's) that do not fit beside
// the others from from on — made before reservations held shares.
func overbooked(rs []*devpodv1alpha1.NodeReservation, c amount, from time.Time) map[string]bool {
	out := map[string]bool{}
	for _, nr := range rs {
		var others []*devpodv1alpha1.NodeReservation
		for _, o := range rs {
			if o.Name != nr.Name {
				others = append(others, o)
			}
		}
		if ok, _ := fitsIn(others, c, reservedOf(nr, c), isWhole(nr), later(nr.Spec.Start.Time, from), overAt(nr)); !ok {
			out[nr.Name] = true
		}
	}
	return out
}

// placed: a running DevPod on a machine — what it takes, and since when.
type placed struct {
	DP    *devpodv1alpha1.DevPod
	Ask   amount
	Since time.Time
}

// nodeLoad: one machine at one time — its capacity, what its reservations
// in force hold (together, and by holder), what each owner's running
// DevPods take.
type nodeLoad struct {
	Cap, Booked amount
	Held, Use   map[string]amount
}

// loadAt: the machine (capacity c, reservations rs, DevPods on) at t,
// leaving out the DevPod skip.
func loadAt(c amount, rs []*devpodv1alpha1.NodeReservation, on []placed, t time.Time, skip string) nodeLoad {
	l := nodeLoad{Cap: c, Held: map[string]amount{}, Use: map[string]amount{}}
	for _, nr := range rs {
		if inForce(nr, t) {
			a := reservedOf(nr, c)
			l.Booked = l.Booked.add(a)
			l.Held[nr.Spec.User] = l.Held[nr.Spec.User].add(a)
		}
	}
	for _, p := range on {
		if p.DP.Name != skip {
			l.Use[p.DP.Spec.Owner] = l.Use[p.DP.Spec.Owner].add(p.Ask)
		}
	}
	return l
}

// outside: what DevPods take beyond their owners' reservations.
func (l nodeLoad) outside() amount {
	var o amount
	for u, use := range l.Use {
		o = o.add(use.sub(l.Held[u]).clamped())
	}
	return o
}

// refusal: why owner may not add a DevPod taking ask (nil: may). needsRes:
// the rules let them on the machine only under a reservation — it must
// fit theirs; anyone else's, past their own reservations, must fit what
// no reservation holds and no one else takes.
func (l nodeLoad) refusal(node, owner string, needsRes bool, ask amount) *serverErr {
	use, held := l.Use[owner], l.Held[owner]
	if needsRes {
		// only what it asks for is judged: an old overrun of another
		// resource is not this DevPod's
		over := use.add(ask).sub(held)
		if (ask.GPU <= 0 || over.GPU <= 0) && (ask.CPU <= 0 || over.CPU <= 0) {
			return nil
		}
		return &serverErr{http.StatusConflict, "RESERVATION_TOO_SMALL",
			fmt.Sprintf("%s holds %s of node %s and uses %s: this DevPod takes %s more", owner, held, node, use, ask),
			map[string]any{"node": node, "held": held.view(), "used": use.view(), "ask": ask.view()}}
	}
	added := use.add(ask).sub(held).clamped().sub(use.sub(held).clamped())
	free := l.Cap.sub(l.Booked).sub(l.outside())
	if added.fitsAsked(free) {
		return nil
	}
	return &serverErr{http.StatusConflict, "NO_ROOM",
		fmt.Sprintf("node %s has %s free outside reservations: this DevPod takes %s", node, free.clamped(), ask),
		map[string]any{"node": node, "free": free.clamped().view(), "ask": ask.view()}}
}

// toClear: the DevPods to hibernate so that the reservations in force at
// t get their shares — of those whose owner holds none of them, the
// newest first (then the later name), only while one makes a shortage
// smaller.
func toClear(c amount, rs []*devpodv1alpha1.NodeReservation, on []placed, t time.Time) []placed {
	l := loadAt(c, rs, on, t, "")
	short := l.outside().sub(c.sub(l.Booked))
	if short.GPU <= 0 && short.CPU <= 0 {
		return nil
	}
	var cand []placed
	for _, p := range on {
		if _, holds := l.Held[p.DP.Spec.Owner]; !holds {
			cand = append(cand, p)
		}
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if !cand[i].Since.Equal(cand[j].Since) {
			return cand[i].Since.After(cand[j].Since)
		}
		return cand[i].DP.Name > cand[j].DP.Name
	})
	var out []placed
	for _, p := range cand {
		if short.GPU <= 0 && short.CPU <= 0 {
			break
		}
		if (short.GPU > 0 && p.Ask.GPU > 0) || (short.CPU > 0 && p.Ask.CPU > 0) {
			out = append(out, p)
			short = short.sub(p.Ask)
		}
	}
	return out
}

// rooms: the reservable machines of one cluster now — their capacity,
// reservations and running DevPods — for judging starts there.
type rooms struct {
	cap map[string]amount
	rs  map[string][]*devpodv1alpha1.NodeReservation
	on  map[string][]placed
	now time.Time
}

// roomsIn: ctx's cluster's reservable machines among nodes; nil when
// reservations are off.
func (s *Server) roomsIn(ctx context.Context, nodes []corev1.Node) (*rooms, error) {
	if s.Reservations == nil {
		return nil, nil
	}
	r := &rooms{cap: map[string]amount{}, rs: map[string][]*devpodv1alpha1.NodeReservation{}, now: s.Reservations.now()}
	for i := range nodes {
		if nodes[i].Labels[devpodv1alpha1.ReservableNodeLabel] == "true" {
			r.cap[nodes[i].Name] = capacityOf(&nodes[i])
		}
	}
	if len(r.cap) == 0 {
		return r, nil
	}
	all, err := s.nodeReservations(ctx)
	if err != nil {
		return nil, err
	}
	cluster := s.resCluster(ctx)
	for i := range all {
		if nr := &all[i]; nr.Spec.Cluster == cluster {
			if _, ok := r.cap[nr.Spec.Node]; ok {
				r.rs[nr.Spec.Node] = append(r.rs[nr.Spec.Node], nr)
			}
		}
	}
	if r.on, err = s.devPodsByNode(ctx, s.kc(ctx)); err != nil {
		return nil, err
	}
	return r, nil
}

// refusal: why owner may not add a DevPod (named skip, if it is there
// already) taking ask on n now — nil on a machine that is not reservable.
func (r *rooms) refusal(n *corev1.Node, owner string, needsRes bool, skip string, ask amount) *serverErr {
	if r == nil {
		return nil
	}
	c, ok := r.cap[n.Name]
	if !ok {
		return nil
	}
	return loadAt(c, r.rs[n.Name], r.on[n.Name], r.now, skip).refusal(n.Name, owner, needsRes, ask)
}

// reservedNow: a reservation of n is in force.
func (r *rooms) reservedNow(node string) bool {
	if r == nil {
		return false
	}
	for _, nr := range r.rs[node] {
		if inForce(nr, r.now) {
			return true
		}
	}
	return false
}
