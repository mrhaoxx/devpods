// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

type watchEvent struct {
	Type   string                 `json:"type"` // ADDED | MODIFIED | DELETED
	DevPod *devpodv1alpha1.DevPod `json:"devpod"`
	Pod    *PodInfo               `json:"pod,omitempty"`
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
	all, ok := s.listScope(w, r, sess)
	if !ok {
		return
	}
	visible := func(dp *devpodv1alpha1.DevPod) bool { return all || dp.Spec.Owner == sess.User }
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

	podInformer, err := s.Cache.GetInformer(r.Context(), &corev1.Pod{})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}

	events := make(chan watchEvent, 64)
	send := func(ev watchEvent) {
		select {
		case events <- ev:
		default: // slow consumer: drop; the client re-lists on reconnect
		}
	}
	push := func(typ string, obj any) {
		dp, ok := unwrap(obj).(*devpodv1alpha1.DevPod)
		if !ok || dp.Namespace != s.NS || !visible(dp) {
			return
		}
		ev := watchEvent{Type: typ, DevPod: dp}
		if typ != "DELETED" {
			ev.Pod = s.podInfo(r.Context(), dp)
		}
		send(ev)
	}
	// A Pod change (container start/restart, IP, node) re-pushes its
	// DevPod so the list's Up column follows container restarts.
	pushPod := func(obj any, deleted bool) {
		pod, ok := unwrap(obj).(*corev1.Pod)
		if !ok || pod.Namespace != s.NS {
			return
		}
		name := pod.Labels[render.LabelDevPod]
		if name == "" {
			return
		}
		var dp devpodv1alpha1.DevPod
		if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.NS, Name: name}, &dp); err != nil || !visible(&dp) {
			return
		}
		ev := watchEvent{Type: "MODIFIED", DevPod: &dp}
		if !deleted {
			ev.Pod = PodInfoFor(&dp, pod)
		}
		send(ev)
	}
	podReg, err := podInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { pushPod(obj, false) },
		UpdateFunc: func(_, obj any) { pushPod(obj, false) },
		DeleteFunc: func(obj any) { pushPod(obj, true) },
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	defer func() { _ = podInformer.RemoveEventHandler(podReg) }()
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
