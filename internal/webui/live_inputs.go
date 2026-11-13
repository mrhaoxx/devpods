// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

// gpuProductLabel is set by the NVIDIA GPU feature discovery.
const gpuProductLabel = "nvidia.com/gpu.product"

// keepNodeLabel limits the labels streamed every second to the few the
// UI shows (feature-discovery nodes carry hundreds).
func keepNodeLabel(k string) bool {
	return strings.HasPrefix(k, "node-role.kubernetes.io/") || strings.HasPrefix(k, "devpod.io/") ||
		strings.HasPrefix(k, "nvidia.com/gpu.") || k == "kubernetes.io/arch" || k == "topology.kubernetes.io/zone"
}

// LiveInputs assembles live.Inputs from the informer caches. Refresh
// adds audit presence and recent alerts; SetIdle takes the leader's
// idle job result.
type LiveInputs struct {
	S *Server

	mu       sync.Mutex
	idle     map[string]bool
	alerts   []live.AlertRef
	presence map[string][]string
}

// SetIdle replaces the idle DevPod set (by UID).
func (li *LiveInputs) SetIdle(m map[string]bool) {
	li.mu.Lock()
	li.idle = m
	li.mu.Unlock()
}

// Refresh reloads presence and the last hour's alerts from the audit
// store; it keeps the previous values when the store is unavailable.
func (li *LiveInputs) Refresh(ctx context.Context) error {
	if li.S.Audit == nil || li.S.Audit.Store == nil {
		return nil
	}
	all, err := li.UIDs(ctx)
	if err != nil {
		return err
	}
	uids := make([]string, 0, len(all))
	for uid := range all {
		uids = append(uids, uid)
	}
	pres, err := li.S.Audit.Store.Presence(ctx, uids)
	if err != nil {
		return err
	}
	now := time.Now()
	rows, _, err := li.S.Audit.Store.ListAlerts(ctx, store.TimeFilter{From: now.Add(-live.RecentAlerts), To: now, Limit: 200})
	if err != nil {
		return err
	}
	alerts := make([]live.AlertRef, 0, len(rows))
	for _, a := range rows {
		alerts = append(alerts, live.AlertRef{TS: a.TS, DevPodUID: a.DevPodUID, SessionID: a.SessionID, Kind: a.Kind, Detail: a.Detail})
	}
	li.mu.Lock()
	li.presence, li.alerts = pres, alerts
	li.mu.Unlock()
	return nil
}

func sumResources(spec *corev1.PodSpec, limits bool) corev1.ResourceList {
	if !limits {
		sum := corev1.ResourceList{}
		for _, c := range spec.Containers {
			for name, qty := range c.Resources.Requests {
				cur := sum[name]
				cur.Add(qty)
				sum[name] = cur
			}
		}
		return sum
	}
	return PodLimits(spec)
}

func cores(l corev1.ResourceList) float64 { return l.Cpu().AsApproximateFloat64() }
func bytesOf(l corev1.ResourceList) float64 {
	return l.Memory().AsApproximateFloat64()
}
func gpus(l corev1.ResourceList) int {
	q, ok := l[corev1.ResourceName(resourceGPU)]
	if !ok {
		return 0
	}
	return int(q.Value())
}

// Build snapshots the cluster objects the rollups need.
// A list on an informer that cannot sync (e.g. no RBAC for nodes) waits
// for its context, so Build bounds itself to keep the ticker moving.
func (li *LiveInputs) Build(ctx context.Context) live.Inputs {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	s := li.S
	in := live.Inputs{Quotas: map[string]live.Quota{}}
	// every cluster the console manages (its own, when it manages no
	// others); in a console of several each ref says its cluster
	owners := map[ownerIn]bool{}
	var nodes []nodeIn
	for n, cc := range s.ksc(ctx) {
		label := s.clusterLabel(cc)
		var dps devpodv1alpha1.DevPodList
		if err := cc.Client.List(ctx, &dps, client.InNamespace(s.NS), client.UnsafeDisableDeepCopy); err != nil {
			if n == 0 && s.Clusters == nil {
				return in
			}
			continue // a cluster that cannot be read is left out of this round
		}
		var pods corev1.PodList
		_ = cc.Client.List(ctx, &pods, client.InNamespace(s.NS), client.HasLabels{render.LabelDevPod}, client.UnsafeDisableDeepCopy)
		// what other workloads hold on each node, as the cluster's agent says
		var others map[string]agent.NodeUsage
		if s.Clusters != nil && cc.Name != "" {
			if info, ok := s.Clusters.Info(cc.Name); ok {
				others = info.Nodes
			}
		}
		var ns corev1.NodeList
		if err := cc.Client.List(ctx, &ns, client.UnsafeDisableDeepCopy); err == nil {
			for i := range ns.Items {
				nodes = append(nodes, nodeIn{cluster: label, node: &ns.Items[i], other: others[ns.Items[i].Name]})
			}
		}
		li.devpodRefs(&in, label, cc, owners, dps.Items, pods.Items)
	}
	li.finish(ctx, &in, owners, nodes)
	return in
}

// ownerIn: an owner of DevPods in a cluster (quotas are per cluster).
type ownerIn struct {
	cluster, owner string
	cc             *clusters.ClusterClients
}

// nodeIn: a node, the cluster it is in, and what pods other than DevPods
// hold on it.
type nodeIn struct {
	cluster string
	node    *corev1.Node
	other   agent.NodeUsage
}

// devpodRefs adds one cluster's DevPods to in.
func (li *LiveInputs) devpodRefs(in *live.Inputs, cluster string, cc *clusters.ClusterClients, owners map[ownerIn]bool, dps []devpodv1alpha1.DevPod, pods []corev1.Pod) {
	podOf := map[string]*corev1.Pod{}
	for i := range pods {
		podOf[pods[i].Labels[render.LabelDevPod]] = &pods[i]
	}
	for i := range dps {
		dp := &dps[i]
		ref := live.DevPodRef{Cluster: cluster, UID: string(dp.UID), Name: dp.Name, Owner: dp.Spec.Owner, Phase: string(dp.Status.Phase),
			PhaseSince: dp.CreationTimestamp.Time}
		owners[ownerIn{cluster: cluster, owner: dp.Spec.Owner, cc: cc}] = true
		var spec *corev1.PodSpec
		if pod := podOf[dp.Name]; pod != nil {
			spec, ref.Node, ref.PhaseSince = &pod.Spec, pod.Spec.NodeName, pod.CreationTimestamp.Time
		} else if dp.Spec.Pod != nil {
			spec = &dp.Spec.Pod.Spec
		}
		if spec != nil {
			lim, req := sumResources(spec, true), sumResources(spec, false)
			ref.CPULimit, ref.MemLimit, ref.GPURequest = cores(lim), bytesOf(lim), gpus(lim)
			ref.CPURequest, ref.MemRequest = cores(req), bytesOf(req)
		}
		in.DevPods = append(in.DevPods, ref)
	}
}

// finish adds the nodes, the owners' quotas and what the audit store said.
func (li *LiveInputs) finish(ctx context.Context, in *live.Inputs, owners map[ownerIn]bool, nodes []nodeIn) {
	s := li.S
	{
		for _, ni := range nodes {
			n := ni.node
			ref := live.NodeRef{Cluster: ni.cluster, Name: n.Name, Labels: map[string]string{}, GPUModel: n.Labels[gpuProductLabel],
				CPUAllocatable: cores(n.Status.Allocatable), MemAllocatable: bytesOf(n.Status.Allocatable), GPUAllocatable: gpus(n.Status.Allocatable),
				OtherCPU: ni.other.CPU, OtherMem: ni.other.Mem, OtherGPU: ni.other.GPU}
			for k, v := range n.Labels {
				if keepNodeLabel(k) {
					ref.Labels[k] = v
				}
			}
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady {
					ref.Ready = c.Status == corev1.ConditionTrue
				}
			}
			in.Nodes = append(in.Nodes, ref)
		}
	}

	var users devpodv1alpha1.UserList
	byName := map[string]*devpodv1alpha1.User{}
	if err := s.Client.List(ctx, &users, client.UnsafeDisableDeepCopy); err == nil {
		for i := range users.Items {
			byName[users.Items[i].Name] = &users.Items[i]
		}
	}
	for o := range owners {
		q := EffectiveQuota(byName[o.owner], "", s.DefaultQuota)
		if s.Clusters != nil && o.cc != nil {
			q = EffectiveQuota(byName[o.owner], o.cc.Name, s.clusterQuota(ctx, o.cc))
		}
		in.Quotas[live.QuotaKey(o.cluster, o.owner)] = live.Quota{CPU: cores(q.Compute), Mem: bytesOf(q.Compute), GPU: gpus(q.Compute)}
	}

	li.mu.Lock()
	in.Idle, in.Alerts, in.Presence = li.idle, li.alerts, li.presence
	li.mu.Unlock()
}

// uidIndex is the cache index DevPodByUID uses (RegisterIndexes).
const uidIndex = "metadata.uid"

// RegisterIndexes adds the cache indexes the webui needs; call before
// the manager starts.
func RegisterIndexes(ctx context.Context, fi client.FieldIndexer) error {
	return fi.IndexField(ctx, &devpodv1alpha1.DevPod{}, uidIndex, func(o client.Object) []string {
		return []string{string(o.GetUID())}
	})
}

// UIDs lists the DevPods' UIDs; an error means "unknown", not "none".
func (li *LiveInputs) UIDs(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	for _, cc := range li.S.ksc(ctx) { // of every cluster
		var list devpodv1alpha1.DevPodList
		if err := cc.Client.List(ctx, &list, client.InNamespace(li.S.NS), client.UnsafeDisableDeepCopy); err != nil {
			return nil, err
		}
		for i := range list.Items {
			out[string(list.Items[i].UID)] = true
		}
	}
	return out, nil
}

// DevPodByUID finds a DevPod in the cache by UID (an index lookup: the
// ingest authenticates every batch with it).
func (s *Server) DevPodByUID(ctx context.Context, uid string) (*devpodv1alpha1.DevPod, string, bool) {
	dp, cc, ok := s.devpodByUID(ctx, uid)
	if !ok {
		return nil, "", false
	}
	return dp, s.clusterLabel(cc), true
}

// devpodByUID finds a DevPod by uid in whichever cluster it is.
func (s *Server) devpodByUID(ctx context.Context, uid string) (*devpodv1alpha1.DevPod, *clusters.ClusterClients, bool) {
	for _, cc := range s.ksc(ctx) {
		var list devpodv1alpha1.DevPodList
		if err := cc.Client.List(ctx, &list, client.InNamespace(s.NS), client.MatchingFields{uidIndex: uid}); err == nil && len(list.Items) > 0 {
			return &list.Items[0], cc, true
		}
	}
	return nil, nil, false
}

// DevPodEvent records a Kubernetes Event about the DevPod with uid, in
// the cluster it is in (where kubectl describe and the console's events
// tab look for it).
func (s *Server) DevPodEvent(ctx context.Context, uid, eventType, reason, message string) error {
	dp, cc, ok := s.devpodByUID(ctx, uid)
	if !ok {
		return fmt.Errorf("no DevPod %s", uid)
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: dp.Name + ".", Namespace: dp.Namespace},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: devpodv1alpha1.GroupVersion.String(), Kind: "DevPod",
			Namespace: dp.Namespace, Name: dp.Name, UID: dp.UID, ResourceVersion: dp.ResourceVersion,
		},
		Type: eventType, Reason: reason, Message: message,
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
		Source:              corev1.EventSource{Component: "devpod-webui"},
		ReportingController: "devpod-webui", ReportingInstance: "devpod-webui",
	}
	return cc.Client.Create(ctx, ev)
}
