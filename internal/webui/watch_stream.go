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

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// streamMsg is one Server-Sent Event on the per-DevPod stream. Kind
// selects the payload: "devpod" carries the DevPod object plus its
// Kore binding readback (drives live phase/status), "event" carries a
// related k8s Event (drives the live event log).
type streamMsg struct {
	Kind   string                 `json:"kind"` // "devpod" | "event"
	Type   string                 `json:"type"` // ADDED | MODIFIED | DELETED
	DevPod *devpodv1alpha1.DevPod `json:"devpod,omitempty"`
	Detail *DevPodDetail          `json:"detail,omitempty"`
	Event  *corev1.Event          `json:"event,omitempty"`
}

// handleDevPodStream is the single SSE connection a detail page needs:
// it multiplexes DevPod status changes AND the DevPod's k8s Events
// over one HTTP request. Collapsing the two former streams into one
// keeps the detail page within the browser's 6-connection HTTP/1.1
// per-origin cap.
//
// Both informers replay their cache on handler registration, so the
// client receives the current DevPod and the event backlog on
// connect — no separate initial fetch required.
func (s *Server) handleDevPodStream(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	name := dp.Name

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported", nil)
		return
	}

	dpInformer, err := s.Cache.GetInformer(r.Context(), &devpodv1alpha1.DevPod{})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	evInformer, err := s.Cache.GetInformer(r.Context(), &corev1.Event{})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}

	msgs := make(chan streamMsg, 128)
	send := func(m streamMsg) {
		select {
		case msgs <- m:
		default: // slow consumer: drop; EventSource reconnect refills the backlog
		}
	}

	pushDevPod := func(typ string, obj any) {
		obj = unwrap(obj)
		got, ok := obj.(*devpodv1alpha1.DevPod)
		if !ok || got.Namespace != s.NS || got.Name != name {
			return
		}
		send(streamMsg{Kind: "devpod", Type: typ, DevPod: got, Detail: &DevPodDetail{DevPod: *got, Binding: s.bindingInfo(r, got)}})
	}
	pushEvent := func(typ string, obj any) {
		obj = unwrap(obj)
		ev, ok := obj.(*corev1.Event)
		if !ok || ev.Namespace != s.NS || ev.InvolvedObject.Name != name {
			return
		}
		send(streamMsg{Kind: "event", Type: typ, Event: ev})
	}

	dpReg, err := dpInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { pushDevPod("ADDED", o) },
		UpdateFunc: func(_, o any) { pushDevPod("MODIFIED", o) },
		DeleteFunc: func(o any) { pushDevPod("DELETED", o) },
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	defer func() { _ = dpInformer.RemoveEventHandler(dpReg) }()

	evReg, err := evInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { pushEvent("ADDED", o) },
		UpdateFunc: func(_, o any) { pushEvent("MODIFIED", o) },
		DeleteFunc: func(o any) { pushEvent("DELETED", o) },
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	defer func() { _ = evInformer.RemoveEventHandler(evReg) }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-msgs:
			raw, err := json.Marshal(m)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	}
}

// unwrap resolves the tombstone wrapper the informer delivers on some
// deletes back to the underlying object.
func unwrap(obj any) any {
	if tomb, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		return tomb.Obj
	}
	return obj
}
