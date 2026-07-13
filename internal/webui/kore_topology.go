// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	s.writeJSON(w, http.StatusOK, map[string]any{"nodes": koreTopologyFromList(&list, s.NS)})
}
