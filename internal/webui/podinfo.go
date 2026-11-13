// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"

	"github.com/mrhaoxx/devpod/internal/webui/clusters"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

// PodInfo is the live Pod state the list and detail pages show.
type PodInfo struct {
	Node string `json:"node"`
	IP   string `json:"ip"`
	// StartedAt is when the target container last started (resets on a
	// container restart); nil unless it is running.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
}

// targetContainerName mirrors render: persistence.targetContainer, else
// the first container.
func targetContainerName(dp *devpodv1alpha1.DevPod) string {
	if dp.Spec.Persistence != nil && dp.Spec.Persistence.TargetContainer != "" {
		return dp.Spec.Persistence.TargetContainer
	}
	if dp.Spec.Pod != nil && len(dp.Spec.Pod.Spec.Containers) > 0 {
		return dp.Spec.Pod.Spec.Containers[0].Name
	}
	return ""
}

// PodInfoFor extracts PodInfo for dp from its Pod (nil pod → nil).
func PodInfoFor(dp *devpodv1alpha1.DevPod, pod *corev1.Pod) *PodInfo {
	if pod == nil {
		return nil
	}
	info := &PodInfo{Node: pod.Spec.NodeName, IP: pod.Status.PodIP}
	target := targetContainerName(dp)
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == target && cs.State.Running != nil {
			t := cs.State.Running.StartedAt
			info.StartedAt = &t
		}
	}
	return info
}

// podInfo reads dp's Pod from the cache.
func (s *Server) podInfo(ctx context.Context, dp *devpodv1alpha1.DevPod) *PodInfo {
	var pod corev1.Pod
	if err := s.kc(ctx).Client.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: render.PodName(dp)}, &pod); err != nil {
		return nil
	}
	return PodInfoFor(dp, &pod)
}

// podInfos maps DevPod name → PodInfo for the DevPods that have a Pod. In
// a console of several clusters the key is "<cluster>/<name>".
func (s *Server) podInfos(ctx context.Context, dps []InCluster) map[string]PodInfo {
	out := map[string]PodInfo{}
	byName := map[string]*clusters.ClusterClients{}
	for _, cc := range s.ksc(ctx) {
		byName[cc.Name] = cc
	}
	for i := range dps {
		cc := byName[dps[i].Cluster]
		if cc == nil {
			continue
		}
		if info := s.podInfo(s.in(ctx, cc), &dps[i].DevPod); info != nil {
			key := dps[i].Name
			if dps[i].Cluster != "" {
				key = dps[i].Cluster + "/" + key
			}
			out[key] = *info
		}
	}
	return out
}
