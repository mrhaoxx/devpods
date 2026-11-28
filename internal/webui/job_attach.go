// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/coder/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/kattach"
	"github.com/mrhaoxx/devpod/internal/render"
)

// Attaching to a running job's rank (srun's way, plat101's scheme): the
// caller's WebSocket bridged to the rank's container through the API
// server's attach. Frames from the caller: binary — the container's stdin;
// text — control, {"type":"resize","cols":C,"rows":R} or {"type":"eof"}
// (stdin has ended). Frames to the caller: binary, the first byte the
// stream (1 stdout, 2 stderr) and then what the container wrote. It ends
// with a normal close when the container's streams end. What cannot be
// attached to is answered before any upgrade (409 not running, 502 the
// attach failed), so the caller can follow the output instead.

// PodAttacher attaches to container of a Pod in cluster ("" the console's
// own), with a terminal or not.
type PodAttacher func(ctx context.Context, cluster, ns, pod, container string, tty bool) (*kattach.Conn, error)

// handleJobAttach serves GET /api/jobs/{id}/attach?rank=N for the job's
// owner or an admin.
func (s *Server) handleJobAttach(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	j, ok := s.jobFor(w, r, sess, true)
	if !ok {
		return
	}
	s.attachRank(w, r, j, eventFrom(r, sess))
}

// HandleDPAttach serves GET /dp/v1/attach?job=ID&rank=N for dp: the
// DevPod's owner attached to a rank of a job of theirs. ?probe=1 is
// answered 204 — dp asks it before it says it can attach (an agent of
// before passes no WebSocket on).
func (s *Server) HandleDPAttach(w http.ResponseWriter, r *http.Request) {
	dp, cluster, ok := s.dpCaller(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("probe") == "1" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	j, err := s.jobByID(r.Context(), r.URL.Query().Get("job"))
	if err != nil || j.Spec.Owner != dp.Spec.Owner {
		http.Error(w, "no such job of this DevPod's owner", http.StatusNotFound)
		return
	}
	ev := store.EventRow{Actor: dp.Spec.Owner, Via: "dp", Origin: cluster + "/" + dp.Name}
	s.attachRank(w, r, j, ev)
}

// attachRank bridges the request's WebSocket with rank ?rank= of j (a
// terminal for rank 0 of a tty job).
func (s *Server) attachRank(w http.ResponseWriter, r *http.Request, j *devpodv1alpha1.Job, ev store.EventRow) {
	if s.PodAttach == nil {
		http.Error(w, "attaching is not available on this console", http.StatusServiceUnavailable)
		return
	}
	rank, err := strconv.Atoi(r.URL.Query().Get("rank"))
	if err != nil || rank < 0 || rank >= int(j.Spec.Nodes) {
		http.Error(w, fmt.Sprintf("a rank of this job is 0 to %d", j.Spec.Nodes-1), http.StatusBadRequest)
		return
	}
	if jobEnded(j.Status.Phase) {
		http.Error(w, "this job has ended", http.StatusConflict)
		return
	}
	ctx := r.Context()
	cc := s.clientsOf(j.Spec.Cluster)
	if cc == nil {
		http.Error(w, "the job's cluster is away", http.StatusServiceUnavailable)
		return
	}
	name := rankName(j.Spec.Owner, j.Name, int32(rank))
	var dp devpodv1alpha1.DevPod
	var pod corev1.Pod
	container := rankContainer(j)
	if cc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: s.NS}, &dp) != nil ||
		cc.Client.Get(ctx, types.NamespacedName{Name: render.PodName(&dp), Namespace: s.NS}, &pod) != nil || !containerRunning(&pod, container) {
		http.Error(w, "this rank is not running", http.StatusConflict)
		return
	}
	tty := j.Spec.Attach == "tty" && rank == 0
	kc, err := s.PodAttach(ctx, j.Spec.Cluster, s.NS, pod.Name, container, tty)
	if err != nil {
		slog.Warn("job_attach", "job", j.Name, "rank", rank, "err", err)
		http.Error(w, "attaching to the rank failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = kc.Close() }()
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(1 << 20) // a terminal's paste is one frame
	detail := "rank " + strconv.Itoa(rank)
	if tty {
		detail += "; a terminal"
	}
	ev.Node = pod.Spec.NodeName
	s.recordJob(ctx, ev, j, "attach", detail)

	// the caller → the container
	go func() {
		for {
			typ, msg, err := ws.Read(ctx)
			if err != nil {
				_ = kc.Close()
				return
			}
			if typ == websocket.MessageBinary {
				if kc.Stdin(msg) != nil {
					return
				}
				continue
			}
			var c struct {
				Type       string
				Cols, Rows uint16
			}
			if json.Unmarshal(msg, &c) != nil {
				continue
			}
			switch {
			case c.Type == "resize" && c.Cols > 0 && c.Rows > 0:
				_ = kc.Resize(c.Cols, c.Rows)
			case c.Type == "eof":
				_ = kc.CloseStdin()
			}
		}
	}()
	// the container → the caller
	for {
		ch, data, err := kc.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = ws.Close(websocket.StatusNormalClosure, "")
			}
			return
		}
		if ch != kattach.Stdout && ch != kattach.Stderr {
			continue // the attach's own status: its end follows
		}
		if ws.Write(ctx, websocket.MessageBinary, append([]byte{ch}, data...)) != nil {
			return
		}
	}
}

// containerRunning: pod runs, and its container does.
func containerRunning(pod *corev1.Pod, container string) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == container {
			return cs.State.Running != nil
		}
	}
	return false
}
