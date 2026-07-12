// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	toolscache "k8s.io/client-go/tools/cache"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

type watchEvent struct {
	Type   string                 `json:"type"` // ADDED | MODIFIED | DELETED
	DevPod *devpodv1alpha1.DevPod `json:"devpod"`
}

// handleWatchDevPods streams the session user's DevPod changes as
// Server-Sent Events, fed straight from the shared informer — no
// polling. One informer handler per connection; removed on
// disconnect.
func (s *Server) handleWatchDevPods(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported", nil)
		return
	}

	informer, err := s.Cache.GetInformer(r.Context(), &devpodv1alpha1.DevPod{})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}

	events := make(chan watchEvent, 64)
	push := func(typ string, obj any) {
		dp, ok := obj.(*devpodv1alpha1.DevPod)
		if !ok {
			if tomb, isTomb := obj.(toolscache.DeletedFinalStateUnknown); isTomb {
				dp, ok = tomb.Obj.(*devpodv1alpha1.DevPod)
			}
			if !ok {
				return
			}
		}
		if dp.Namespace != s.NS || dp.Spec.Owner != sess.User {
			return
		}
		select {
		case events <- watchEvent{Type: typ, DevPod: dp}:
		default: // slow consumer: drop; the client re-lists on reconnect
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
