// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

// TelemetryStore is the phase-2 part of the audit store.
type TelemetryStore interface {
	MetricsRange(ctx context.Context, uid string, from, to time.Time, step time.Duration) (*store.MetricSeries, error)
	LatestInfo(ctx context.Context, uid string, since time.Time) (*store.ContainerInfoRow, *store.SnapshotRow, error)
	LastSessionByUser(ctx context.Context, since time.Time) (map[string]time.Time, error)
	RecentContainer(ctx context.Context, uids []string, from, to time.Time, step time.Duration) (*store.RecentSeries, error)
}

// metricRanges are the history windows the detail page offers.
var metricRanges = map[string]time.Duration{
	"15m": 15 * time.Minute, "1h": time.Hour, "3h": 3 * time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour,
	"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
}

// maxPoints caps range/step.
const maxPoints = 10000

// infoLookback: container info is re-sent at every agent start.
const infoLookback = 30 * 24 * time.Hour

func (s *Server) telemetryStore(w http.ResponseWriter) (TelemetryStore, bool) {
	if s.Audit != nil {
		if ts, ok := s.Audit.Store.(TelemetryStore); ok {
			return ts, true
		}
	}
	s.writeErr(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "the telemetry store is not available", nil)
	return nil, false
}

func parseStep(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(v)
}

// handleDevPodMetrics serves GET /api/devpods/{name}/metrics?range=&step=.
func (s *Server) handleDevPodMetrics(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	rg := r.URL.Query().Get("range")
	if rg == "" {
		rg = "1h"
	}
	d, ok := metricRanges[rg]
	if !ok {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "range must be one of 15m, 1h, 3h, 6h, 12h, 24h, 7d", nil)
		return
	}
	step, err := parseStep(r.URL.Query().Get("step"))
	if err != nil || step < 0 || (step > 0 && (step < time.Second || d/step > maxPoints)) {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "step must be ≥ 1s and give at most 10000 points", nil)
		return
	}
	ts, ok := s.telemetryStore(w)
	if !ok {
		return
	}
	to := time.Now().UTC()
	series, err := ts.MetricsRange(r.Context(), string(dp.UID), to.Add(-d), to, step)
	if err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, series)
}

// handleDevPodInfo serves GET /api/devpods/{name}/info.
func (s *Server) handleDevPodInfo(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	ts, ok := s.telemetryStore(w)
	if !ok {
		return
	}
	info, snap, err := ts.LatestInfo(r.Context(), string(dp.UID), time.Now().Add(-infoLookback))
	if errors.Is(err, store.ErrNotFound) {
		s.writeErr(w, http.StatusNotFound, "NO_TELEMETRY", "no telemetry yet; monitoring starts when the Pod is next recreated", nil)
		return
	}
	if err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", err.Error(), nil)
		return
	}
	out := map[string]any{"info": json.RawMessage(info.Info), "infoTs": info.TS}
	if snap != nil {
		out["snapshot"], out["snapshotTs"] = json.RawMessage(snap.Data), snap.TS
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleDevPodProcesses serves GET /api/devpods/{name}/processes: every
// process, redacted like the stream.
func (s *Server) handleDevPodProcesses(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	if s.Live == nil || s.Live.Source == nil {
		s.writeErr(w, http.StatusNotFound, "TELEMETRY_DISABLED", "live metrics are not enabled", nil)
		return
	}
	f, err := s.Live.Source.DevPod(r.Context(), string(dp.UID), true)
	if err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "LIVE_UNAVAILABLE", err.Error(), nil)
		return
	}
	procs := redactProcesses(f.Processes, sess)
	if procs == nil {
		procs = []live.ProcLive{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": procs})
}

// Sparkline history (UI feedback round 1): the last 15 minutes in 15 s
// buckets, from ClickHouse, so a page shows curves as soon as it opens.
const (
	recentWindow = 15 * time.Minute
	recentStep   = 15 * time.Second
)

// handleLiveRecent serves GET /api/live/devpods/recent?scope=mine|all or
// ?name=<devpod>: CPU and memory sparkline history keyed by DevPod name.
func (s *Server) handleLiveRecent(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var dps []devpodv1alpha1.DevPod
	if name := r.URL.Query().Get("name"); name != "" {
		dp, ok := s.getOwned(w, r, sess, name)
		if !ok {
			return
		}
		dps = []devpodv1alpha1.DevPod{*dp}
	} else {
		all, ok := s.listScope(w, r, sess)
		if !ok {
			return
		}
		var err error
		if all {
			dps, err = s.allDevPods(r)
		} else {
			dps, err = s.ownedDevPods(r, sess.User)
		}
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
	}
	ts, ok := s.telemetryStore(w)
	if !ok {
		return
	}
	names := map[string]string{}
	uids := make([]string, 0, len(dps))
	for _, dp := range dps {
		if !dp.Spec.Running {
			continue // hibernated: nothing recent to draw
		}
		names[string(dp.UID)] = dp.Name
		uids = append(uids, string(dp.UID))
	}
	to := time.Now().UTC()
	rs, err := ts.RecentContainer(r.Context(), uids, to.Add(-recentWindow), to, recentStep)
	if err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", err.Error(), nil)
		return
	}
	items := map[string]store.RecentItem{}
	for uid, it := range rs.Items {
		if n, ok := names[uid]; ok {
			items[n] = it
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"step": rs.Step, "ts": rs.TS, "items": items})
}
