// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The webui reads KoreNodeTopology as unstructured and json-round-trips
// the status into these local mirrors, so it never build-depends on the
// Kore Go module (github.com/zjusct/kore) — consistent with the
// annotation-only Kore integration.
type koreDevice struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type koreZone struct {
	ID          int          `json:"id"`
	Cpus        string       `json:"cpus"`
	FreeCpus    string       `json:"freeCpus"`
	SMTSiblings [][]int      `json:"smtSiblings"`
	Memory      string       `json:"memory,omitempty"`
	Devices     []koreDevice `json:"devices,omitempty"`
}

type koreAlloc struct {
	Pod       string `json:"pod"` // "<namespace>/<name>" as Kore reports it
	Container string `json:"container"`
	Cpuset    string `json:"cpuset"`
	// DevPod is the bare name, set only when the pod lives in the
	// webui's devpod namespace (so the UI links to its detail page).
	DevPod string `json:"devpod,omitempty"`
}

type korePool struct {
	Name    string   `json:"name"`
	Cpuset  string   `json:"cpuset"`
	NUMA    []int    `json:"numa,omitempty"`
	Members []string `json:"members,omitempty"`
}

type nodeTopology struct {
	Node         string      `json:"node"`
	ReservedCpus string      `json:"reservedCpus,omitempty"`
	Zones        []koreZone  `json:"zones"`
	Allocations  []koreAlloc `json:"allocations"`
	Pools        []korePool  `json:"pools"`
}

// koreStatusRaw mirrors KoreNodeTopology.status (Kore's json tags).
type koreStatusRaw struct {
	ReservedSystemCpus string `json:"reservedSystemCpus"`
	Zones              []struct {
		ID          int          `json:"id"`
		Cpus        string       `json:"cpus"`
		FreeCpus    string       `json:"freeCpus"`
		SMTSiblings [][]int      `json:"smtSiblings"`
		MemoryTotal string       `json:"memoryTotal"`
		Devices     []koreDevice `json:"devices"`
	} `json:"zones"`
	Allocations []koreAlloc `json:"allocations"`
	Pools       []korePool  `json:"pools"`
}

// koreTopologyFromList transforms the raw CR list into the clean shape
// the SPA renders. devpodNS marks which allocations are DevPods (so the
// UI can deep-link). Pure — unit-testable without a live cluster.
func koreTopologyFromList(list *unstructured.UnstructuredList, devpodNS string) []nodeTopology {
	out := make([]nodeTopology, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		raw, err := json.Marshal(item.Object["status"])
		if err != nil {
			continue
		}
		var st koreStatusRaw
		if err := json.Unmarshal(raw, &st); err != nil {
			continue
		}
		for j := range st.Allocations {
			if ns, name, ok := strings.Cut(st.Allocations[j].Pod, "/"); ok && ns == devpodNS {
				st.Allocations[j].DevPod = name
			}
		}
		n := nodeTopology{
			Node:         item.GetName(),
			ReservedCpus: st.ReservedSystemCpus,
			Allocations:  st.Allocations,
			Pools:        st.Pools,
		}
		for _, z := range st.Zones {
			n.Zones = append(n.Zones, koreZone{
				ID: z.ID, Cpus: z.Cpus, FreeCpus: z.FreeCpus,
				SMTSiblings: z.SMTSiblings, Memory: z.MemoryTotal, Devices: z.Devices,
			})
		}
		if n.Allocations == nil {
			n.Allocations = []koreAlloc{}
		}
		if n.Pools == nil {
			n.Pools = []korePool{}
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

func (s *Server) handleKoreTopology(w http.ResponseWriter, r *http.Request) {
	if !s.KoreEnabled {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "Kore integration is not enabled", nil)
		return
	}
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kore.zjusct.io", Version: "v1alpha1", Kind: "KoreNodeTopologyList",
	})
	if err := s.Reader.List(r.Context(), &list); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	nodes := koreTopologyFromList(&list, s.NS)
	s.resolvePoolMembers(r.Context(), nodes)
	s.writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

// resolvePoolMembers rewrites Kore's pod-UID pool members to pod names
// (= DevPod names in our namespace). Unresolvable UIDs stay as-is.
func (s *Server) resolvePoolMembers(ctx context.Context, nodes []nodeTopology) {
	var pods corev1.PodList
	if err := s.Reader.List(ctx, &pods, client.InNamespace(s.NS)); err != nil {
		return
	}
	uid2name := make(map[string]string, len(pods.Items))
	for i := range pods.Items {
		uid2name[string(pods.Items[i].UID)] = pods.Items[i].Name
	}
	for ni := range nodes {
		for pi := range nodes[ni].Pools {
			for mi, m := range nodes[ni].Pools[pi].Members {
				if name, ok := uid2name[m]; ok {
					nodes[ni].Pools[pi].Members[mi] = name
				}
			}
		}
	}
}

// handleDevPodTopology returns the physical layout of the node a
// DevPod runs on, plus the cores this DevPod occupies — for the
// OWNER (not just admins). Other pods' identities are not exposed;
// the SPA renders them anonymously.
func (s *Server) handleDevPodTopology(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	empty := map[string]any{"node": nil}
	if !s.KoreEnabled || dp.Status.WorkloadRef == nil || dp.Status.WorkloadRef.Kind != "Pod" {
		s.writeJSON(w, http.StatusOK, empty)
		return
	}
	var pod corev1.Pod
	if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: dp.Status.WorkloadRef.Name, Namespace: s.NS}, &pod); err != nil || pod.Spec.NodeName == "" {
		s.writeJSON(w, http.StatusOK, empty)
		return
	}
	var obj unstructured.Unstructured
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "kore.zjusct.io", Version: "v1alpha1", Kind: "KoreNodeTopology"})
	if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: pod.Spec.NodeName}, &obj); err != nil {
		s.writeJSON(w, http.StatusOK, empty)
		return
	}
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{obj}}
	nodes := koreTopologyFromList(list, s.NS)
	s.resolvePoolMembers(r.Context(), nodes)
	if len(nodes) == 0 {
		s.writeJSON(w, http.StatusOK, empty)
		return
	}
	node := nodes[0]

	// This DevPod's cores: an exclusive allocation, else a pool it joins.
	mine := ""
	for _, a := range node.Allocations {
		if a.DevPod == dp.Name {
			mine = a.Cpuset
		}
	}
	if mine == "" {
		for _, p := range node.Pools {
			for _, m := range p.Members {
				if m == dp.Name {
					mine = p.Cpuset
				}
			}
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"node": node, "cpuset": mine})
}
