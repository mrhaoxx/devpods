// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"
	"sort"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// handleListTemplates returns every template, binding details
// included — users may SEE bindings, they just can't author them.
// Binding-carrying templates are hidden when Kore is off.
func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionFrom(w, r); !ok {
		return
	}
	var list devpodv1alpha1.DevPodTemplateList
	if err := s.Client.List(r.Context(), &list); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := []devpodv1alpha1.DevPodTemplate{}
	for _, tpl := range list.Items {
		if !s.KoreEnabled && tpl.Spec.Binding != nil {
			continue
		}
		items = append(items, tpl)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
