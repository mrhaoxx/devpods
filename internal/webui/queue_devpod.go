// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

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
func (s *Server) homeNode(ctx context.Context, cc *clusters.ClusterClients, dp *devpodv1alpha1.DevPod) string {
	if dp.Spec.Persistence == nil {
		return ""
	}
	var pvc corev1.PersistentVolumeClaim
	if err := cc.Client.Get(ctx, types.NamespacedName{Name: render.HomePVCName(dp), Namespace: s.NS}, &pvc); err != nil || pvc.Spec.VolumeName == "" {
		return ""
	}
	var pv corev1.PersistentVolume
	if err := cc.Client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil || pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	terms := pv.Spec.NodeAffinity.Required.NodeSelectorTerms
	if len(terms) != 1 {
		return ""
	}
	for _, r := range terms[0].MatchExpressions {
		if r.Key == hostnameLabel && r.Operator == corev1.NodeSelectorOpIn && len(r.Values) == 1 {
			return r.Values[0]
		}
	}
	return ""
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

// setQueueState writes st on dp; false when it says so already.
func setQueueState(dp *devpodv1alpha1.DevPod, st queueState) bool {
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
// in line (to keep its place should it go back).
func setAutoPin(dp *devpodv1alpha1.DevPod, node string, since time.Time) {
	if dp.Spec.Pod.Spec.NodeSelector == nil {
		dp.Spec.Pod.Spec.NodeSelector = map[string]string{}
	}
	dp.Spec.Pod.Spec.NodeSelector[hostnameLabel] = node
	annotate(dp, devpodv1alpha1.AutoPinAnnotation, node+"@"+since.UTC().Format(time.RFC3339Nano))
}

// autoPin: the node the queue pinned dp to and since when it was in line
// — ok only while that is still dp's pin (a pin its user set is theirs).
func autoPin(dp *devpodv1alpha1.DevPod) (string, time.Time, bool) {
	node, at, found := strings.Cut(dp.Annotations[devpodv1alpha1.AutoPinAnnotation], "@")
	if !found || node == "" || node != pinnedNode(dp) {
		return "", time.Time{}, false
	}
	t, _ := time.Parse(time.RFC3339Nano, at)
	return node, t, true
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

// nudgeQueue asks the queue for a round soon (a start waits in line).
func (s *Server) nudgeQueue() {}

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
	if s.Queue == nil {
		if pinnedNode(dp) != "" {
			return false, nil
		}
		known, ever := s.machinesFor(ctx, dp)
		if !known {
			return false, nil // no machine known to place it on: as before
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
		// inside its owner's own reservation there: that room is theirs
		if j.reserved[node] {
			if rm, err := s.roomsIn(ctx, []corev1.Node{n}); err == nil && rm.refusal(&n, owner, true, dp.Name, askOfPod(spec)) == nil {
				return false, nil
			}
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
	e := devPodEntry(dp, now, 0, s.homeNode(ctx, kc, dp), "")
	v, _, err := s.queueViewFor(ctx, kc, nil, []qEntry{e}, now)
	if err != nil {
		return &serverErr{http.StatusServiceUnavailable, "CLUSTER_UNAVAILABLE", err.Error(), nil}
	}
	plan := planQueue(v)
	for _, st := range plan.Starts {
		if st.Entry.Key == e.Key {
			setAutoPin(dp, st.Machines[0], now)
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
