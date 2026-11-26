// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

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
