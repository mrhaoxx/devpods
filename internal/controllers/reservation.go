// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Node reservations (reservation spec §4, §5): helpers shared by the
// DevPod and Reservation reconcilers.

const hostnameLabel = "kubernetes.io/hostname"

// Messages show wall-clock times in Asia/Shanghai (spec §3.1); a fixed
// zone, so the controller image needs no tzdata.
var reservationZone = time.FixedZone("CST", 8*3600)

func clock(t time.Time) string { return t.In(reservationZone).Format("15:04") }

// PinnedNode is the node a DevPod asks for: the hostname node selector
// of its Pod template ("" = wherever the scheduler puts it).
func PinnedNode(dp *devpodv1alpha1.DevPod) string {
	if dp.Spec.Pod == nil {
		return ""
	}
	return dp.Spec.Pod.Spec.NodeSelector[hostnameLabel]
}

// loses reports whether another live reservation on the same node
// overlaps res and was created first: the webui deletes such a loser
// right after creating it (spec §3.3), so the controller must not act on
// it in the meantime.
func loses(res *devpodv1alpha1.Reservation, all []devpodv1alpha1.Reservation) bool {
	for i := range all {
		o := &all[i]
		if o.Name == res.Name || o.Spec.Node != res.Spec.Node || !o.DeletionTimestamp.IsZero() || !o.Overlaps(res.Spec.Start, res.Spec.End) {
			continue
		}
		if o.CreationTimestamp.Before(&res.CreationTimestamp) || (o.CreationTimestamp.Equal(&res.CreationTimestamp) && o.Name < res.Name) {
			return true
		}
	}
	return false
}

// heldByOthers maps each node someone other than owner holds right now
// to that reservation.
func heldByOthers(all []devpodv1alpha1.Reservation, owner string, now time.Time) map[string]*devpodv1alpha1.Reservation {
	t := metav1.NewTime(now)
	out := map[string]*devpodv1alpha1.Reservation{}
	for i := range all {
		r := &all[i]
		if r.Spec.User != owner && r.DeletionTimestamp.IsZero() && r.HeldAt(t) && !loses(r, all) {
			out[r.Spec.Node] = r
		}
	}
	return out
}

// excludeNodes adds a required node affinity keeping the Pod off nodes
// (spec §4.1). Required terms are ORed, so the rule goes into each.
func excludeNodes(pod *corev1.Pod, nodes []string) {
	if len(nodes) == 0 {
		return
	}
	nodes = append([]string(nil), nodes...)
	sort.Strings(nodes)
	req := corev1.NodeSelectorRequirement{Key: hostnameLabel, Operator: corev1.NodeSelectorOpNotIn, Values: nodes}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	sel := na.RequiredDuringSchedulingIgnoredDuringExecution
	if len(sel.NodeSelectorTerms) == 0 {
		sel.NodeSelectorTerms = []corev1.NodeSelectorTerm{{}}
	}
	for i := range sel.NodeSelectorTerms {
		sel.NodeSelectorTerms[i].MatchExpressions = append(sel.NodeSelectorTerms[i].MatchExpressions, req)
	}
}

// setDevPodCondition turns a DevPod condition on (True, with the message)
// or removes it; it reports whether anything changed.
func setDevPodCondition(ctx context.Context, c client.Client, dp *devpodv1alpha1.DevPod, typ string, on bool, message string) (bool, error) {
	before := dp.DeepCopy()
	changed := false
	if on {
		changed = meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
			Type: typ, Status: metav1.ConditionTrue, Reason: "Reservation", Message: message, ObservedGeneration: dp.Generation,
		})
	} else {
		changed = meta.RemoveStatusCondition(&dp.Status.Conditions, typ)
	}
	if !changed {
		return false, nil
	}
	return true, c.Status().Patch(ctx, dp, client.MergeFrom(before))
}

// nextWake is the next moment something happens to a reservation that is
// not over: the clearing notice, the start, each ending notice, the end.
// Zero = nothing left (now is at or past the end).
func nextWake(start, end, now time.Time, clearNotice time.Duration, endingNotices []time.Duration) time.Time {
	points := []time.Time{start.Add(-clearNotice), start, end}
	for _, d := range endingNotices {
		points = append(points, end.Add(-d))
	}
	var next time.Time
	for _, p := range points {
		if p.After(now) && (next.IsZero() || p.Before(next)) {
			next = p
		}
	}
	return next
}
