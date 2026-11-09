// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mrhaoxx/devpod/internal/webui/live"
)

type resView struct {
	Allocatable float64  `json:"allocatable"`
	Allocated   float64  `json:"allocated"`
	Used        *float64 `json:"used,omitempty"` // absent without live data
}

type gpuView struct {
	Model string          `json:"model,omitempty"`
	Total int             `json:"total"`
	Used  int             `json:"used"`
	Cards []live.CardLive `json:"cards"`
}

type rdmaView struct {
	Resource    string `json:"resource"`
	Allocatable int64  `json:"allocatable"`
}

// NodeView is one node on the admin nodes page.
type NodeView struct {
	Name    string            `json:"name"`
	Ready   bool              `json:"ready"`
	Labels  map[string]string `json:"labels"`
	CPU     resView           `json:"cpu"`
	Mem     resView           `json:"mem"`
	GPU     gpuView           `json:"gpu"`
	RDMA    []rdmaView        `json:"rdma"`
	DevPods int               `json:"devpods"`
}

type nodeDevPod struct {
	Name  string   `json:"name"`
	Owner string   `json:"owner"`
	Phase string   `json:"phase"`
	CPU   *float64 `json:"cpu"`
	Mem   *float64 `json:"mem"`
	GPU   int      `json:"gpu"`
}

// liveSnapshot returns the live rollups, or rollups without usage when
// the live layer is off or unreachable.
func (s *Server) liveSnapshot(ctx context.Context) (live.Snapshot, live.Inputs, bool) {
	in := (&LiveInputs{S: s}).Build(ctx)
	if s.Live != nil && s.Live.Snapshot != nil {
		if snap, ok := s.Live.Snapshot(ctx); ok {
			return snap, in, true
		}
	}
	return live.NewState(nil).Snapshot(in), in, false
}

// isRDMAResource matches the RDMA device-plugin resources.
func isRDMAResource(name corev1.ResourceName) bool {
	n := strings.ToLower(string(name))
	return strings.HasPrefix(n, "rdma/") || strings.Contains(n, "/hca") || strings.Contains(n, "rdma")
}

func (s *Server) nodeViews(ctx context.Context) ([]NodeView, live.Snapshot, live.Inputs, bool) {
	snap, in, ok := s.liveSnapshot(ctx)
	rdma := map[string][]rdmaView{}
	var nodes corev1.NodeList
	if err := s.Client.List(ctx, &nodes, client.UnsafeDisableDeepCopy); err == nil {
		for i := range nodes.Items {
			for name, q := range nodes.Items[i].Status.Allocatable {
				if isRDMAResource(name) {
					rdma[nodes.Items[i].Name] = append(rdma[nodes.Items[i].Name], rdmaView{Resource: string(name), Allocatable: q.Value()})
				}
			}
		}
	}
	out := make([]NodeView, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		v := NodeView{Name: n.Name, Ready: n.Ready, Labels: n.Labels, DevPods: n.DevPods,
			CPU:  resView{Allocatable: n.CPUAllocatable, Allocated: n.CPUAllocated},
			Mem:  resView{Allocatable: n.MemAllocatable, Allocated: n.MemAllocated},
			GPU:  gpuView{Model: n.GPUModel, Total: n.GPUTotal, Used: n.GPUUsed, Cards: n.Cards},
			RDMA: rdma[n.Name]}
		if ok {
			cpu, mem := n.CPUUsed, n.MemUsed
			v.CPU.Used, v.Mem.Used = &cpu, &mem
		}
		if v.Labels == nil {
			v.Labels = map[string]string{}
		}
		if v.GPU.Cards == nil {
			v.GPU.Cards = []live.CardLive{}
		}
		if v.RDMA == nil {
			v.RDMA = []rdmaView{}
		}
		sort.Slice(v.RDMA, func(i, j int) bool { return v.RDMA[i].Resource < v.RDMA[j].Resource })
		out = append(out, v)
	}
	return out, snap, in, ok
}

// handleAdminNodes serves GET /api/admin/nodes.
func (s *Server) handleAdminNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	views, _, _, ok := s.nodeViews(r.Context())
	s.writeJSON(w, http.StatusOK, map[string]any{"items": views, "live": ok})
}

// handleAdminNode serves GET /api/admin/nodes/{name}: the node, its
// DevPods and (with Kore) its CPU topology.
func (s *Server) handleAdminNode(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	views, snap, in, isLive := s.nodeViews(r.Context())
	var view *NodeView
	for i := range views {
		if views[i].Name == name {
			view = &views[i]
		}
	}
	if view == nil {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "node not found", nil)
		return
	}
	dps := []nodeDevPod{}
	for _, ref := range in.DevPods {
		if ref.Node != name {
			continue
		}
		d := nodeDevPod{Name: ref.Name, Owner: ref.Owner, Phase: ref.Phase, GPU: ref.GPURequest}
		if l, ok := snap.DevPods[ref.UID]; ok {
			d.CPU, d.Mem = l.CPU, l.Mem
		}
		dps = append(dps, d)
	}
	sort.Slice(dps, func(i, j int) bool { return dps[i].Name < dps[j].Name })
	out := map[string]any{"node": view, "devpods": dps, "live": isLive}
	if s.KoreEnabled {
		var obj unstructured.Unstructured
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "kore.zjusct.io", Version: "v1alpha1", Kind: "KoreNodeTopology"})
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: name}, &obj); err == nil {
			list := unstructured.UnstructuredList{Items: []unstructured.Unstructured{obj}}
			if topo := koreTopologyFromList(&list, s.NS); len(topo) == 1 {
				s.resolvePoolMembers(r.Context(), topo)
				out["topology"] = topo[0]
			}
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}
