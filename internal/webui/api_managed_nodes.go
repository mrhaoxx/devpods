// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"
	"slices"
	"strings"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// managedNode: a machine one may switch reservable (design 2026-11-20 §6.1).
type managedNode struct {
	Cluster    string `json:"cluster,omitempty"`
	Name       string `json:"name"`
	Reservable bool   `json:"reservable"`
	Ready      bool   `json:"ready"`
	GPUModel   string `json:"gpuModel,omitempty"`
	GPUTotal   int    `json:"gpuTotal"`
}

// handleManagedNodes: GET /api/nodes/managed — the machines of the
// clusters reachable now that the caller may switch reservable: every one
// to an admin, those a grant gives to anyone else.
func (s *Server) handleManagedNodes(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var gs *access.GrantSet
	var groups map[string]bool
	if !sess.Admin {
		var err error
		if gs, groups, err = s.grantsFor(r.Context(), sess.User); err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
	}
	items := []managedNode{}
	for _, cc := range s.ks(r) {
		ctx := s.in(r.Context(), cc)
		views, _, _, _ := s.nodeViews(ctx)
		for _, v := range views {
			if !sess.Admin && !gs.Has(devpodv1alpha1.PermSetReservable, sess.User, groups, cc.Name, &access.Node{Name: v.Name, Labels: v.Labels}) {
				continue
			}
			items = append(items, managedNode{Cluster: s.clusterLabel(cc), Name: v.Name, Reservable: v.Labels[devpodv1alpha1.ReservableNodeLabel] == "true",
				Ready: v.Ready, GPUModel: v.GPU.Model, GPUTotal: v.GPU.Total})
		}
	}
	slices.SortFunc(items, func(a, b managedNode) int {
		if c := strings.Compare(a.Cluster, b.Cluster); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
