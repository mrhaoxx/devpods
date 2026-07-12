// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
)

type eventEvent struct {
	Type  string        `json:"type"` // ADDED | MODIFIED | DELETED
	Event *corev1.Event `json:"event"`
}

// handleWatchDevPodEvents streams the k8s Events related to one
// DevPod as SSE. The informer replays its cache on handler
// registration, so the client receives the existing backlog before
// live updates — no separate initial fetch needed. Pod events match
// because the rendered Pod shares the DevPod's name.
func (s *Server) handleWatchDevPodEvents(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported", nil)
		return
	}

	informer, err := s.Cache.GetInformer(r.Context(), &corev1.Event{})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}

	events := make(chan eventEvent, 64)
	push := func(typ string, obj any) {
		ev, ok := obj.(*corev1.Event)
		if !ok {
			if tomb, isTomb := obj.(toolscache.DeletedFinalStateUnknown); isTomb {
				ev, ok = tomb.Obj.(*corev1.Event)
			}
			if !ok {
				return
			}
		}
		if ev.Namespace != s.NS || ev.InvolvedObject.Name != dp.Name {
			return
		}
		select {
		case events <- eventEvent{Type: typ, Event: ev}:
		default: // slow consumer: drop; the client refetches on reconnect
		}
	}
	reg, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { push("ADDED", obj) },
		UpdateFunc: func(_, obj any) { push("MODIFIED", obj) },
		DeleteFunc: func(obj any) { push("DELETED", obj) },
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	defer func() { _ = informer.RemoveEventHandler(reg) }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			raw, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	}
}
