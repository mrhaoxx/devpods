// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
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
