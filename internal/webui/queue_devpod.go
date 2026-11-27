// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mrhaoxx/devpod/internal/access"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// DevPods started through the queue (design 2026-11-27): a DevPod to
// start is an entry of the line — one rank, asking what its Pod asks,
// with no end — held to one machine when it has one (pinned, or a home
// on a machine's own disk), kept off another for a while.

// devPodEntry: dp, queued since since at priority prio, as an entry of
// the line; only and avoid as qEntry's.
func devPodEntry(dp *devpodv1alpha1.DevPod, since time.Time, prio int32, only, avoid string) qEntry {
	e := qEntry{Key: "devpod/" + dp.Name, DevPod: dp, Owner: dp.Spec.Owner, Priority: prio, Since: since, Nodes: 1, Only: only, Avoid: avoid}
	if dp.Spec.Pod != nil {
		e.Ask = askOfPod(&dp.Spec.Pod.Spec)
	}
	return e
}

// homeNode: the one machine dp's home volume is bound to — a PV with a
// required node affinity naming one hostname (a local disk) — or "".
// Read straight from the cluster, not a cache: one the console may not
// read (an older role) fails at once rather than waiting on a cache that
// never fills.
func (s *Server) homeNode(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod) string {
	n, _ := s.homeNodeOf(ctx, cc, dp)
	return n
}

// homeNodeOf is homeNode, and whether it could be told: false when the
// volume could not be read (not "it is not there" — a role that may not
// read it, the cluster away), which is logged once per cluster.
func (s *Server) homeNodeOf(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod) (string, bool) {
	if dp.Spec.Persistence == nil || cc.Reader == nil {
		return "", true
	}
	unread := func(what string, err error) (string, bool) {
		if apierrors.IsNotFound(err) {
			return "", true
		}
		if _, said := s.homeUnread.LoadOrStore(cc.Name, true); !said {
			slog.Warn("queue_home_unread", "cluster", cc.Name, "what", what, "err", err, "fix", "the console's (or agent's) role needs get on persistentvolumeclaims and persistentvolumes")
		}
		return "", false
	}
	var pvc corev1.PersistentVolumeClaim
	if err := cc.Reader.Get(ctx, types.NamespacedName{Name: render.HomePVCName(dp), Namespace: s.NS}, &pvc); err != nil {
		return unread("pvc", err)
	}
	if pvc.Spec.VolumeName == "" {
		return "", true
	}
	var pv corev1.PersistentVolume
	if err := cc.Reader.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return unread("pv", err)
	}
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return "", true
	}
	terms := pv.Spec.NodeAffinity.Required.NodeSelectorTerms
	if len(terms) != 1 {
		return "", true
	}
	for _, r := range terms[0].MatchExpressions {
		if r.Key == hostnameLabel && r.Operator == corev1.NodeSelectorOpIn && len(r.Values) == 1 {
			return r.Values[0], true
		}
	}
	return "", true
}

// devPodQuotaOK: dp, started, keeps its owner within their compute quota
// beside their other DevPods (of all) and what planned starts before it.
func (s *Server) devPodQuotaOK(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod, all []devpodv1alpha1.DevPod, planned []qEntry) bool {
	var u devpodv1alpha1.User
	var up *devpodv1alpha1.User
	if s.Client.Get(ctx, types.NamespacedName{Name: dp.Spec.Owner}, &u) == nil {
		up = &u
	}
	var existing []devpodv1alpha1.DevPod
	for _, d := range all {
		if d.Spec.Owner == dp.Spec.Owner && d.Name != dp.Name && d.DeletionTimestamp == nil {
			existing = append(existing, d)
		}
	}
	for _, pe := range planned {
		switch {
		case pe.DevPod != nil && pe.Owner == dp.Spec.Owner:
			started := pe.DevPod.DeepCopy()
			started.Spec.Running = true
			existing = append(existing, *started)
		case pe.Job != nil && pe.Owner == dp.Spec.Owner:
			for r := int32(0); r < pe.Job.Spec.Nodes; r++ {
				existing = append(existing, *rankDevPod(pe.Job, r, "x", s.NS, ""))
			}
		}
	}
	started := dp.DeepCopy()
	started.Spec.Running = true
	return CheckQuota(EffectiveQuota(up, cc.Name, s.clusterQuota(ctx, cc)), existing, started) == nil
}

// annotate sets dp's annotation k to v ("": takes it off).
func annotate(dp *devpodv1alpha1.DevPod, k, v string) {
	if v == "" {
		delete(dp.Annotations, k)
		return
	}
	if dp.Annotations == nil {
		dp.Annotations = map[string]string{}
	}
	dp.Annotations[k] = v
}

// markQueued puts dp in the line, since since, at priority prio.
func markQueued(dp *devpodv1alpha1.DevPod, since time.Time, prio int32) {
	annotate(dp, devpodv1alpha1.QueuedAtAnnotation, since.UTC().Format(time.RFC3339Nano))
	annotate(dp, devpodv1alpha1.QueuePriorityAnnotation, strconv.Itoa(int(prio)))
}

// clearQueued takes dp out of the line: its mark, priority and state.
func clearQueued(dp *devpodv1alpha1.DevPod) {
	for _, k := range []string{devpodv1alpha1.QueuedAtAnnotation, devpodv1alpha1.QueuePriorityAnnotation, devpodv1alpha1.QueueStateAnnotation} {
		delete(dp.Annotations, k)
	}
}

// queuedAt: since when dp is in the line (false: it is not).
func queuedAt(dp *devpodv1alpha1.DevPod) (time.Time, bool) {
	v, ok := dp.Annotations[devpodv1alpha1.QueuedAtAnnotation]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, true // in line, its time lost: at the front of its priority
	}
	return t, true
}

// queuePriority: dp's priority in the line (0 when it says none).
func queuePriority(dp *devpodv1alpha1.DevPod) int32 {
	n, _ := strconv.Atoi(dp.Annotations[devpodv1alpha1.QueuePriorityAnnotation])
	return int32(n)
}

// queueState: where a queued DevPod stands, as a job's status says.
type queueState struct {
	Position int32             `json:"position"`
	Reason   string            `json:"reason"`
	Detail   map[string]string `json:"detail,omitempty"`
	Expected *time.Time        `json:"expected,omitempty"`
}

// queueStateOf: dp's state in the line (zero: none written).
func queueStateOf(dp *devpodv1alpha1.DevPod) queueState {
	var st queueState
	_ = json.Unmarshal([]byte(dp.Annotations[devpodv1alpha1.QueueStateAnnotation]), &st)
	return st
}

// setQueueState writes st on dp; false when it says so already. Times are
// kept to the second, as a job's status keeps them: a promise worked out
// afresh each round does not rewrite it for less.
func setQueueState(dp *devpodv1alpha1.DevPod, st queueState) bool {
	if st.Expected != nil {
		t := st.Expected.UTC().Truncate(time.Second)
		st.Expected = &t
	}
	cur := queueStateOf(dp)
	same := cur.Position == st.Position && cur.Reason == st.Reason && maps.Equal(cur.Detail, st.Detail) &&
		reflect.DeepEqual(timeUTC(cur.Expected), timeUTC(st.Expected))
	if same && dp.Annotations[devpodv1alpha1.QueueStateAnnotation] != "" {
		return false
	}
	raw, _ := json.Marshal(st)
	annotate(dp, devpodv1alpha1.QueueStateAnnotation, string(raw))
	return true
}

func timeUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// setAutoPin pins dp to node for the queue, recording since when it was
// in line and at what priority (to keep its place should it go back):
// <node>@<since>[@<priority>, when not 0].
func setAutoPin(dp *devpodv1alpha1.DevPod, node string, since time.Time, prio int32) {
	if dp.Spec.Pod.Spec.NodeSelector == nil {
		dp.Spec.Pod.Spec.NodeSelector = map[string]string{}
	}
	dp.Spec.Pod.Spec.NodeSelector[hostnameLabel] = node
	v := node + "@" + since.UTC().Format(time.RFC3339Nano)
	if prio != 0 {
		v += "@" + strconv.Itoa(int(prio))
	}
	annotate(dp, devpodv1alpha1.AutoPinAnnotation, v)
}

// autoPin: the node the queue pinned dp to and since when it was in line
// — ok only while that is still dp's pin (a pin its user set is theirs).
func autoPin(dp *devpodv1alpha1.DevPod) (string, time.Time, bool) {
	node, rest, found := strings.Cut(dp.Annotations[devpodv1alpha1.AutoPinAnnotation], "@")
	if !found || node == "" || node != pinnedNode(dp) {
		return "", time.Time{}, false
	}
	at, _, _ := strings.Cut(rest, "@")
	t, _ := time.Parse(time.RFC3339Nano, at)
	return node, t, true
}

// autoPinPriority: the priority dp waited at before the queue pinned it
// (0: none recorded).
func autoPinPriority(dp *devpodv1alpha1.DevPod) int32 {
	_, rest, _ := strings.Cut(dp.Annotations[devpodv1alpha1.AutoPinAnnotation], "@")
	_, p, _ := strings.Cut(rest, "@")
	n, _ := strconv.Atoi(p)
	return int32(n)
}

// avoidFor keeps the queue from giving dp node until until.
func avoidFor(dp *devpodv1alpha1.DevPod, node string, until time.Time) {
	annotate(dp, devpodv1alpha1.QueueAvoidAnnotation, node+" "+until.UTC().Format(time.RFC3339))
}

// avoiding: the machine dp is not to be given at now ("": none).
func avoiding(dp *devpodv1alpha1.DevPod, now time.Time) string {
	node, until, ok := strings.Cut(dp.Annotations[devpodv1alpha1.QueueAvoidAnnotation], " ")
	if !ok {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, until); err != nil || !now.Before(t) {
		return ""
	}
	return node
}

// nudges: the channel a round is asked for on.
func (s *Server) nudges() chan struct{} {
	s.nudgeOnce.Do(func() { s.nudge = make(chan struct{}, 1) })
	return s.nudge
}

// nudgeQueue asks this replica's queue for a round soon (a start waits
// in line); asked already, once is enough.
func (s *Server) nudgeQueue() {
	select {
	case s.nudges() <- struct{}{}:
	default:
	}
}

// queuedPoll: how often the leader looks in its caches for DevPods put in
// line (by another replica, whose nudge it does not hear).
var queuedPoll = time.Second

// watchQueued nudges the queue whenever, while this replica leads, a
// DevPod appears in line in any cluster's cache that it has not seen in
// line before — a start another replica took.
func (s *Server) watchQueued(ctx context.Context, leads func() bool) {
	seen := map[string]bool{}
	t := time.NewTicker(queuedPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if leads != nil && !leads() {
			continue
		}
		now := map[string]bool{}
		fresh := false
		for _, cluster := range s.knownClusters(ctx) {
			cc := s.clientsOf(cluster)
			if cc == nil {
				continue
			}
			var dps devpodv1alpha1.DevPodList
			if err := cc.Client.List(ctx, &dps, client.InNamespace(s.NS)); err != nil {
				continue
			}
			for i := range dps.Items {
				if v, ok := dps.Items[i].Annotations[devpodv1alpha1.QueuedAtAnnotation]; ok && !dps.Items[i].Spec.Running {
					key := cluster + "/" + dps.Items[i].Name + "@" + v
					now[key] = true
					if !seen[key] {
						fresh = true
					}
				}
			}
		}
		seen = now
		if fresh {
			s.nudgeQueue()
		}
	}
}

// admitStart: dp is asked to start (its spec says running). A VM, or a
// DevPod the queue has no say over, starts as before (false). With the
// queue on: refused at once for what a wait would not mend (its pin's
// access rules, a priority over the owner's cap, a size no machine could
// ever take); started at once inside its owner's own reservation in force
// on its pinned machine (false); else stopped and put in line (true).
// With the queue off, an unpinned one is pinned now where it fits
// (placeNow; false), or refused.
func (s *Server) admitStart(ctx context.Context, dp *devpodv1alpha1.DevPod, prio int32) (bool, *serverErr) {
	if dp.Spec.Pod == nil {
		return false, nil
	}
	if _, _, ok := autoPin(dp); ok {
		unpinAuto(dp) // the queue's pin of a former run: placed anew
	}
	delete(dp.Annotations, devpodv1alpha1.ExpectClearAnnotation) // a former run's
	if s.Queue == nil {
		if pinnedNode(dp) != "" {
			return false, nil
		}
		known, ever := s.machinesFor(ctx, dp)
		if _, told := s.homeNodeOf(ctx, s.kc(ctx), dp); !known || !told {
			return false, nil // no machine known to place it on, or its home's unknown: as before
		}
		if !ever {
			return false, s.neverFits(ctx, dp)
		}
		return false, s.placeNow(ctx, dp)
	}
	owner := dp.Spec.Owner
	if prio < 0 {
		return false, badRequest("a priority is 0 or more")
	}
	if prio > 0 {
		_, groups, err := s.accessFor(ctx, owner)
		if err != nil {
			return false, &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
		}
		c, unlimited, err := s.priorityCap(ctx, owner, groups, s.resCluster(ctx))
		if err != nil {
			return false, &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
		}
		if !unlimited && prio > c {
			return false, &serverErr{http.StatusForbidden, "PRIORITY_TOO_HIGH", fmt.Sprintf("%s may give at most priority %d here", owner, c), map[string]any{"max": c}}
		}
	}
	spec := &dp.Spec.Pod.Spec
	if node := pinnedNode(dp); node != "" {
		j, err := s.judgeFor(ctx, owner)
		if err != nil {
			return false, &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
		}
		var n corev1.Node
		if err := s.kc(ctx).Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
			return false, badRequest("node %q not found", node)
		}
		needs := access.NeedsOf(spec)
		if e := j.refusal(&n, &needs, true); e != nil {
			return false, e
		}
		// inside its owner's own reservation there: that room is theirs,
		// now; one who may have the machine only under a reservation and
		// does not fit in theirs is refused (no wait would mend it)
		if inside, e := s.insideOwnReservation(ctx, j, &n, dp); inside {
			return false, nil
		} else if e != nil {
			return false, e
		}
	} else {
		known, ever := s.machinesFor(ctx, dp)
		if !known {
			return false, nil // no machine known to place it on: as before
		}
		if !ever {
			return false, s.neverFits(ctx, dp)
		}
	}
	dp.Spec.Running = false
	markQueued(dp, s.Queue.now(), prio)
	s.nudgeQueue()
	return true, nil
}

// machinesFor: whether the console knows the cluster's machines at all —
// one that reports what it has (none: the cluster cannot be read, nor its
// machines' capacity, and the queue has no say where a DevPod goes) — and
// whether one could ever take dp: its owner may use it, its spec fits it,
// it is big enough, load aside.
func (s *Server) machinesFor(ctx context.Context, dp *devpodv1alpha1.DevPod) (known, ever bool) {
	e := devPodEntry(dp, time.Now(), 0, "", "")
	v, _, err := s.queueViewFor(ctx, s.kc(ctx), nil, []qEntry{e}, time.Now())
	if err != nil {
		return false, false
	}
	for _, m := range v.Machines {
		if m.Cap.GPU == 0 && m.Cap.CPU == 0 {
			continue
		}
		known = true
		if v.Allowed(e, m.Name) && e.Ask.fits(m.Cap) {
			ever = true
		}
	}
	return known, ever
}

// neverFits: why dp, which no machine could ever take, is refused — the
// access rules' refusal when it is theirs (as placement always said),
// else NO_MACHINE (too big, or its own placement fits nowhere).
func (s *Server) neverFits(ctx context.Context, dp *devpodv1alpha1.DevPod) *serverErr {
	if e := s.placeDevPod(ctx, dp.DeepCopy()); e != nil {
		return e
	}
	return &serverErr{http.StatusConflict, "NO_MACHINE",
		fmt.Sprintf("no machine in this cluster could ever take this DevPod of %s's (what it asks, the access rules, its own placement)", dp.Spec.Owner), nil}
}

// unpinAuto takes off dp's pin the queue set (and what came with it).
func unpinAuto(dp *devpodv1alpha1.DevPod) {
	if dp.Spec.Pod != nil {
		delete(dp.Spec.Pod.Spec.NodeSelector, hostnameLabel)
		if len(dp.Spec.Pod.Spec.NodeSelector) == 0 {
			dp.Spec.Pod.Spec.NodeSelector = nil
		}
	}
	delete(dp.Annotations, devpodv1alpha1.AutoPinAnnotation)
	delete(dp.Annotations, devpodv1alpha1.ExpectClearAnnotation)
}

// placeNow pins dp, unpinned, where the queue's planner would start it
// now (the queue off: no line) — beside the reservations and what runs —
// or refuses it, saying when there may be room.
func (s *Server) placeNow(ctx context.Context, dp *devpodv1alpha1.DevPod) *serverErr {
	kc := s.kc(ctx)
	now := time.Now()
	if s.Reservations != nil {
		now = s.Reservations.now()
	}
	e := devPodEntry(dp, now, 0, s.homeNode(ctx, kc, dp), avoiding(dp, now))
	v, _, err := s.queueViewFor(ctx, kc, nil, []qEntry{e}, now)
	if err != nil {
		return &serverErr{http.StatusServiceUnavailable, "CLUSTER_UNAVAILABLE", err.Error(), nil}
	}
	plan := planQueue(v)
	for _, st := range plan.Starts {
		if st.Entry.Key == e.Key {
			setAutoPin(dp, st.Machines[0], now, 0)
			if st.ClearsAt != nil {
				annotate(dp, devpodv1alpha1.ExpectClearAnnotation, st.ClearsAt.UTC().Format(time.RFC3339))
			}
			return nil
		}
	}
	if _, ever := s.machinesFor(ctx, dp); !ever {
		return s.neverFits(ctx, dp)
	}
	detail := map[string]any{}
	msg := fmt.Sprintf("no machine in this cluster has room for this DevPod of %s's now", dp.Spec.Owner)
	if w, ok := plan.Waits[e.Key]; ok && w.Expected != nil {
		detail["expected"] = w.Expected.UTC()
		msg += "; there may be at " + w.Expected.UTC().Format(time.RFC3339)
	}
	return &serverErr{http.StatusConflict, "NO_ROOM", msg, detail}
}

// startQueuedDevPod starts planned (as the plan read it) on machine, its
// turn come: pinned there (the queue's pin) unless its user pinned it,
// out of line, told when a reservation will clear it (clearsAt). Not if
// it changed meanwhile — out of line, started, being deleted, its spec
// edited: the next round sees it as it is.
func (s *Server) startQueuedDevPod(ctx context.Context, cc *clusters.ClusterClients, planned *devpodv1alpha1.DevPod, machine string, clearsAt *time.Time) bool {
	var dp devpodv1alpha1.DevPod
	if err := cc.Reader.Get(ctx, types.NamespacedName{Name: planned.Name, Namespace: planned.Namespace}, &dp); err != nil {
		return false
	}
	since, inLine := queuedAt(&dp)
	if !inLine || dp.Spec.Running || dp.DeletionTimestamp != nil || dp.Generation != planned.Generation || dp.Spec.Pod == nil {
		return false
	}
	auto := false
	if _, _, former := autoPin(&dp); former || pinnedNode(&dp) == "" {
		setAutoPin(&dp, machine, since, queuePriority(&dp))
		auto = true
	} else if pinnedNode(&dp) != machine {
		return false
	}
	// pinned now: an allow-list of a former start (from before the queue
	// placed DevPods) would keep it off the machine it was given
	access.SetAllowed(&dp, nil)
	dp.Spec.Running = true
	clearQueued(&dp)
	delete(dp.Annotations, devpodv1alpha1.QueueAvoidAnnotation)
	detail := "pinned"
	if auto {
		detail = "auto"
	}
	if clearsAt != nil {
		annotate(&dp, devpodv1alpha1.ExpectClearAnnotation, clearsAt.UTC().Format(time.RFC3339))
		detail += "; a reservation clears it at " + clearsAt.UTC().Format(time.RFC3339)
	} else {
		delete(dp.Annotations, devpodv1alpha1.ExpectClearAnnotation)
	}
	if err := cc.Client.Update(ctx, &dp); err != nil {
		if !apierrors.IsConflict(err) {
			slog.Warn("queue_devpod_start", "devpod", dp.Name, "err", err)
		}
		return false
	}
	ev := systemEvent("system:queue")
	ev.Node = machine
	s.recordDevPod(ctx, s.clusterLabel(cc), ev, &dp, "queue-start", detail)
	slog.Info("queue_devpod_start", "devpod", dp.Name, "owner", dp.Spec.Owner, "machine", machine, "auto", auto)
	waitCached(ctx, cc, &dp)
	return true
}

// waitCached waits (2 s at most) for cc's cache to hold dp as written: the
// next round plans from the cache, and must not see dp still waiting, its
// room free.
func waitCached(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if err := cc.Client.Get(ctx, types.NamespacedName{Name: dp.Name, Namespace: dp.Namespace}, &got); err == nil &&
			(got.ResourceVersion == dp.ResourceVersion || got.Spec.Running) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// unschedulableFor: how long a DevPod's Pod may stay unschedulable on the
// machine the queue gave it before it goes back in line.
const unschedulableFor = 5 * time.Minute

// avoidPeriod: how long a machine its Pod would not schedule on is kept
// from a DevPod.
const avoidPeriod = time.Hour

// sweepAutoPins carries out the queue's pins' lifetime, each round: a
// DevPod found stopped (whoever stopped it) loses the pin the queue gave
// it; one running whose Pod has not scheduled on that machine for
// unschedulableFor goes back in line — at its old place — kept off that
// machine for avoidPeriod. A pin its user set is never touched.
func (s *Server) sweepAutoPins(ctx context.Context, cc *clusters.ClusterClients, dps []devpodv1alpha1.DevPod, pods map[string]*corev1.Pod, now time.Time) {
	for i := range dps {
		dp := &dps[i]
		node, since, ok := autoPin(dp)
		if !ok || dp.DeletionTimestamp != nil {
			continue
		}
		cp := dp.DeepCopy()
		action, reason := "", ""
		switch {
		case !dp.Spec.Running:
			unpinAuto(cp)
			action = "auto-unpin"
		default:
			why, stuck := unschedulableSince(pods[render.PodName(dp)], now)
			if !stuck {
				continue
			}
			unpinAuto(cp)
			avoidFor(cp, node, now.Add(avoidPeriod))
			action, reason = "requeue-unschedulable", why
			if s.Queue == nil {
				// no line: pinned again now, elsewhere (or left as it is)
				if e := s.placeNow(ctx, cp); e != nil {
					continue
				}
				action = "replace-unschedulable"
				break
			}
			cp.Spec.Running = false
			markQueued(cp, since, autoPinPriority(dp))
		}
		if err := cc.Client.Update(ctx, cp); err != nil {
			if !apierrors.IsConflict(err) {
				slog.Warn("queue_auto_pin", "devpod", dp.Name, "err", err)
			}
			continue
		}
		ev := systemEvent("system:queue")
		ev.Node, ev.Reason = node, reason
		s.recordDevPod(ctx, s.clusterLabel(cc), ev, cp, action, node)
		slog.Info("queue_auto_pin", "devpod", dp.Name, "action", action, "node", node, "reason", reason)
	}
}

// unschedulableSince: pod has been unschedulable for unschedulableFor at
// now — and the scheduler's word for why.
func unschedulableSince(pod *corev1.Pod, now time.Time) (string, bool) {
	if pod == nil || pod.Status.Phase != corev1.PodPending {
		return "", false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return c.Message, now.Sub(c.LastTransitionTime.Time) >= unschedulableFor
		}
	}
	return "", false
}

// fitsWhereItIs: dp, running on the machine the queue gave it, may be
// remade as after only if after fits there now — the machine's free room
// now (a reservation later is the clearing's business) with what before
// takes given back. An edit asking no more cards or cores always may
// (nil: it fits, or what the machine has cannot be told).
func (s *Server) fitsWhereItIs(ctx context.Context, before, after *devpodv1alpha1.DevPod) *serverErr {
	node, _, ok := autoPin(after)
	if !ok || before.Spec.Pod == nil || after.Spec.Pod == nil {
		return nil
	}
	if askOfPod(&after.Spec.Pod.Spec).fits(askOfPod(&before.Spec.Pod.Spec)) {
		return nil
	}
	now := time.Now()
	if s.Reservations != nil {
		now = s.Reservations.now()
	}
	e := devPodEntry(after, now, 0, node, "")
	v, _, err := s.queueViewFor(ctx, s.kc(ctx), nil, []qEntry{e}, now)
	if err != nil {
		return nil
	}
	for _, m := range v.Machines {
		if m.Name != node || (m.Cap.GPU == 0 && m.Cap.CPU == 0) {
			continue
		}
		free := freeFor(m, v.Running, now, time.Millisecond).add(askOfPod(&before.Spec.Pod.Spec))
		if !askOfPod(&after.Spec.Pod.Spec).fits(free) {
			return &serverErr{http.StatusConflict, "NO_ROOM_HERE",
				fmt.Sprintf("it does not fit on %s as it is now: stop it and start it again to wait for room", node), map[string]any{"node": node}}
		}
	}
	return nil
}

// consoleMarks: the annotations only the console writes on a DevPod — its
// place and priority in the queue's line and where it stands, the queue's
// pin and what came with it, why it was hibernated, a reservation's
// clearing mark, the access rules' allow-list.
var consoleMarks = []string{
	devpodv1alpha1.QueuedAtAnnotation, devpodv1alpha1.QueuePriorityAnnotation, devpodv1alpha1.QueueStateAnnotation,
	devpodv1alpha1.AutoPinAnnotation, devpodv1alpha1.QueueAvoidAnnotation, devpodv1alpha1.ExpectClearAnnotation,
	devpodv1alpha1.HibernatedByAnnotation, devpodv1alpha1.ClearAtAnnotation, devpodv1alpha1.ClearForAnnotation,
	access.AnnAllowed,
}

// dropConsoleMarks takes off a new DevPod what only the console writes
// (a YAML create could carry them — a place in line made up, a priority
// over one's cap).
func dropConsoleMarks(dp *devpodv1alpha1.DevPod) {
	for _, k := range consoleMarks {
		delete(dp.Annotations, k)
	}
}

// insideOwnReservation: dp, pinned to n, fits inside its owner's own
// reservation in force there (j: the owner's judge) — and, when it does
// not though its owner may have n only under a reservation, why it may
// not run there at all.
func (s *Server) insideOwnReservation(ctx context.Context, j *judge, n *corev1.Node, dp *devpodv1alpha1.DevPod) (bool, *serverErr) {
	needsRes := !j.admin && j.on(n).NeedsReservation
	if !j.reserved[n.Name] && !needsRes {
		return false, nil
	}
	rm, err := s.roomsIn(ctx, []corev1.Node{*n})
	if err != nil || rm == nil {
		return false, nil
	}
	e := rm.refusal(n, dp.Spec.Owner, true, dp.Name, askOfPod(&dp.Spec.Pod.Spec))
	if e == nil && j.reserved[n.Name] {
		return true, nil
	}
	if needsRes {
		return false, e
	}
	return false, nil
}

// startInOwnReservation starts dp (waiting in line, pinned by its user) at
// once when it fits inside its owner's reservation in force on its
// machine — begun while it waited; true when it did.
func (s *Server) startInOwnReservation(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod) bool {
	node := pinnedNode(dp)
	if _, _, auto := autoPin(dp); node == "" || auto || dp.Spec.Pod == nil {
		return false
	}
	j, err := s.judgeFor(ctx, dp.Spec.Owner)
	if err != nil || !j.reserved[node] {
		return false
	}
	var n corev1.Node
	if err := cc.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return false
	}
	if inside, _ := s.insideOwnReservation(ctx, j, &n, dp); !inside {
		return false
	}
	return s.startQueuedDevPod(ctx, cc, dp, node, nil)
}

// pinSweepTick: how often, with the queue off, the queue's pins are swept.
const pinSweepTick = 30 * time.Second

// RunPinSweep sweeps the queue's pins (sweepAutoPins) every pinSweepTick
// while leads says this replica leads (nil: it always does) — with the
// queue off, where no round does it; with it on, it returns at once.
func (s *Server) RunPinSweep(ctx context.Context, leads func() bool) {
	if s.Queue != nil {
		return
	}
	t := time.NewTicker(pinSweepTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if leads == nil || leads() {
			s.pinSweepRound(ctx)
		}
	}
}

// pinSweepRound sweeps the queue's pins in every cluster once.
func (s *Server) pinSweepRound(ctx context.Context) {
	for _, cluster := range s.knownClusters(ctx) {
		cc := s.clientsOf(cluster)
		if cc == nil {
			continue
		}
		cctx := s.in(ctx, cc)
		var dps devpodv1alpha1.DevPodList
		var pods corev1.PodList
		if cc.Client.List(cctx, &dps, client.InNamespace(s.NS)) != nil || cc.Client.List(cctx, &pods, client.InNamespace(s.NS)) != nil {
			continue
		}
		podOf := map[string]*corev1.Pod{}
		for i := range pods.Items {
			podOf[pods.Items[i].Name] = &pods.Items[i]
		}
		s.sweepAutoPins(cctx, cc, dps.Items, podOf, time.Now())
	}
}
