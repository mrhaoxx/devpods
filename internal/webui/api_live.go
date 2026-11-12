// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

// LiveDeps wires the phase-2 live layer (spec §5.10); nil disables the
// live endpoints.
type LiveDeps struct {
	Hub *live.Hub
	// Source yields one DevPod's full sample and processes (the leader's
	// state, or the leader over HTTP); nil = no per-DevPod live data.
	Source live.Source
	// Snapshot returns current rollups for the REST views (nodes,
	// users): built fresh on the leader, the relayed one on followers;
	// false when unavailable.
	Snapshot func(ctx context.Context) (live.Snapshot, bool)
}

// liveItem is one row of the DevPod list stream.
type liveItem struct {
	// Cluster: the cluster it is in, in a console of several.
	Cluster string `json:"cluster,omitempty"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	live.DevPodLive
}

func emptyLive(d live.DevPodLive) live.DevPodLive {
	if d.GPU == nil {
		d.GPU = []live.GPULive{}
	}
	if d.Sessions == nil {
		d.Sessions = []live.SessionLive{}
	}
	return d
}

func (s *Server) liveDisabled(w http.ResponseWriter) bool {
	if s.Live == nil || s.Live.Hub == nil {
		s.writeErr(w, http.StatusNotFound, "TELEMETRY_DISABLED", "live metrics are not enabled", nil)
		return true
	}
	return false
}

// handleLiveDevPods streams GET /api/live/devpods?scope=mine|all.
func (s *Server) handleLiveDevPods(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok || s.liveDisabled(w) {
		return
	}
	all := false
	switch r.URL.Query().Get("scope") {
	case "", "mine":
	case "all":
		if !sess.Admin {
			s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "admin only", nil)
			return
		}
		all = true
	default:
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "scope must be mine or all", nil)
		return
	}
	s.streamLive(w, r, func(ctx context.Context, snap live.Snapshot) (any, error) {
		items := []liveItem{}
		for _, cc := range s.ksc(r.Context()) { // the request's cluster, or every one
			var one devpodv1alpha1.DevPodList
			if err := cc.Client.List(ctx, &one, client.InNamespace(s.NS), client.UnsafeDisableDeepCopy); err != nil {
				return nil, err
			}
			for i := range one.Items {
				dp := &one.Items[i]
				if !all && dp.Spec.Owner != sess.User {
					continue
				}
				items = append(items, liveItem{Cluster: s.clusterLabel(cc), Name: dp.Name, Owner: dp.Spec.Owner, DevPodLive: emptyLive(snap.DevPods[string(dp.UID)])})
			}
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Cluster != items[j].Cluster {
				return items[i].Cluster < items[j].Cluster
			}
			return items[i].Name < items[j].Name
		})
		return map[string]any{"ts": snap.TS, "items": items}, nil
	})
}

// handleLiveOverview streams GET /api/admin/live/overview.
func (s *Server) handleLiveOverview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok || s.liveDisabled(w) {
		return
	}
	s.streamLive(w, r, func(_ context.Context, snap live.Snapshot) (any, error) {
		return map[string]any{"ts": snap.TS, "kpi": snap.KPI, "nodes": snap.Nodes, "users": snap.Users, "attention": snap.Attention}, nil
	})
}

// streamLive writes each hub snapshot rendered by render as an SSE data
// event, plus a named "liveStatus" event whenever the link to the
// leader drops or returns.
func (s *Server) streamLive(w http.ResponseWriter, r *http.Request, render func(context.Context, live.Snapshot) (any, error)) {
	fl, ok := w.(http.Flusher)
	if !ok {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported", nil)
		return
	}
	hub := s.Live.Hub
	ch, cancel := hub.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	connected := hub.Connected()
	writeStatus := func() {
		fmt.Fprintf(w, "event: liveStatus\ndata: {\"connected\":%t}\n\n", connected)
		fl.Flush()
	}
	writeStatus()
	check := time.NewTicker(time.Second)
	defer check.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-check.C:
			if c := hub.Connected(); c != connected {
				connected = c
				writeStatus()
			}
		case snap := <-ch:
			v, err := render(r.Context(), snap)
			if err != nil {
				continue
			}
			b, err := json.Marshal(v)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// redactProcesses hides other users' command lines from non-admins: a
// process attributed to someone else's session shows only its exe name
// (spec §9.3).
func redactProcesses(procs []live.ProcLive, viewer Session) []live.ProcLive {
	if viewer.Admin {
		return procs
	}
	out := make([]live.ProcLive, len(procs))
	for i, p := range procs {
		if p.User != "" && p.User != viewer.User {
			p.Cmdline = ""
			if p.Exe != "" {
				p.Cmdline = path.Base(p.Exe)
			}
		}
		out[i] = p
	}
	return out
}

// pushLive feeds the per-DevPod stream with "metrics" and "processes"
// events every second, and "liveStatus" when the leader is unreachable.
func (s *Server) pushLive(ctx context.Context, viewer Session, uid string, allProcs bool, send func(streamMsg)) {
	if s.Live == nil || s.Live.Source == nil {
		return
	}
	connected := true
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		f, err := s.Live.Source.DevPod(ctx, uid, allProcs)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			if connected {
				connected = false
				send(streamMsg{Kind: "liveStatus", Connected: &connected})
			}
		default:
			if !connected {
				connected = true
				send(streamMsg{Kind: "liveStatus", Connected: &connected})
			}
			if f.Metrics != nil {
				send(streamMsg{Kind: "metrics", Metrics: f.Metrics})
			}
			procs := redactProcesses(f.Processes, viewer)
			if procs == nil {
				procs = []live.ProcLive{}
			}
			send(streamMsg{Kind: "processes", Processes: &procs})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
