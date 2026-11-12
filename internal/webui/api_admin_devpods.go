// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"
	"sort"
)

type adminDevPod struct {
	Name    string `json:"name"`
	Cluster string `json:"cluster,omitempty"`
	Owner   string `json:"owner"`
	Phase   string `json:"phase"`
	Running bool   `json:"running"`
	CPU     string `json:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Storage string `json:"storage,omitempty"`
}

// handleAdminListDevPods returns every DevPod across all owners — the
// admin's global view. Unlike the owner-scoped list, it does not
// filter by the caller. Works on any auth method.
func (s *Server) handleAdminListDevPods(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	list, err := s.devPods(r, "")
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.staleOf(w, s.ks(r))
	items := make([]adminDevPod, 0, len(list))
	for i := range list {
		dp := &list[i]
		item := adminDevPod{
			Name:    dp.Name,
			Cluster: dp.Cluster,
			Owner:   dp.Spec.Owner,
			Phase:   string(dp.Status.Phase),
			Running: dp.Spec.Running,
		}
		if dp.Spec.Pod != nil {
			lim := PodLimits(&dp.Spec.Pod.Spec)
			if c := lim.Cpu(); !c.IsZero() {
				item.CPU = c.String()
			}
			if m := lim.Memory(); !m.IsZero() {
				item.Memory = m.String()
			}
		}
		if dp.Spec.Persistence != nil {
			item.Storage = dp.Spec.Persistence.Size.String()
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Owner != items[j].Owner {
			return items[i].Owner < items[j].Owner
		}
		return items[i].Name < items[j].Name
	})
	s.writeJSON(w, http.StatusOK, map[string]any{
		"items":   items,
		"devpods": list,
		"pods":    s.podInfos(r.Context(), list),
	})
}
