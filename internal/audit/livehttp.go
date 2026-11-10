// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
)

// LivePath is the gateway's internal live-view endpoint (audit spec §16).
const LivePath = "/internal/live"

// liveHeartbeat keeps idle streams (and proxies on the way) alive.
const liveHeartbeat = 15 * time.Second

// LiveLine is one NDJSON line of the live stream: first {from,cur}, then
// the local backlog, {live:true} at the hand-over, then live records;
// {hb} while idle; {closed} when the channel or session ends; {error}
// when the viewer was dropped.
type LiveLine struct {
	From   uint32 `json:"from,omitempty"`
	Cur    uint32 `json:"cur,omitempty"`
	Live   bool   `json:"live,omitempty"`
	T      int64  `json:"t,omitempty"` // unix milliseconds
	Out    []byte `json:"o,omitempty"` // terminal output
	Cols   uint32 `json:"c,omitempty"` // terminal size change
	Rows   uint32 `json:"r,omitempty"`
	Closed bool   `json:"closed,omitempty"`
	HB     bool   `json:"hb,omitempty"`
	Error  string `json:"error,omitempty"`
}

// LiveRecordLine maps a raw record to a line; ok is false for records a
// viewer does not need.
func LiveRecordLine(r rawlog.Record) (LiveLine, bool) {
	switch r.Kind {
	case rawlog.KindDataOut, rawlog.KindDataErr, rawlog.KindInjectToClient:
		return LiveLine{T: r.TS.UnixMilli(), Out: r.Payload}, true
	case rawlog.KindRequest:
		if c, rows, ok := rawlog.TermSize(r); ok {
			return LiveLine{T: r.TS.UnixMilli(), Cols: c, Rows: rows}, true
		}
	case rawlog.KindChannelClose:
		return LiveLine{T: r.TS.UnixMilli(), Closed: true}, true
	}
	return LiveLine{}, false
}

// LiveHandler serves GET /internal/live?session=&channel= to webui
// replicas holding the shared key; at most maxViewers at once.
func LiveHandler(rec *Recorder, key string, maxViewers int) http.Handler {
	sem := make(chan struct{}, maxViewers)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ch, err := strconv.ParseUint(r.URL.Query().Get("channel"), 10, 32)
		if err != nil {
			http.Error(w, "bad channel", http.StatusBadRequest)
			return
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			http.Error(w, "too many live viewers", http.StatusTooManyRequests)
			return
		}
		watch, err := rec.Watch(r.URL.Query().Get("session"), uint32(ch))
		if errors.Is(err, ErrNoSession) {
			http.Error(w, "no such open session", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer watch.Close()
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		bw := bufio.NewWriterSize(w, 32<<10)
		enc := json.NewEncoder(bw)
		send := func(l LiveLine) error {
			if err := enc.Encode(l); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}
			if fl != nil {
				fl.Flush()
			}
			return nil
		}
		if send(LiveLine{From: watch.From, Cur: watch.Cur}) != nil {
			return
		}
		err = watch.Backlog(func(rr rawlog.Record) error {
			if l, ok := LiveRecordLine(rr); ok {
				return enc.Encode(l) // flushed below; the backlog may be long
			}
			return nil
		})
		if err != nil {
			slog.Warn("audit live backlog", "session", r.URL.Query().Get("session"), "err", err)
		}
		if send(LiveLine{Live: true}) != nil {
			return
		}
		for {
			ctx, cancel := context.WithTimeout(r.Context(), liveHeartbeat)
			rr, err := watch.Next(ctx)
			cancel()
			switch {
			case err == nil:
				l, ok := LiveRecordLine(rr)
				if !ok {
					continue
				}
				if send(l) != nil || l.Closed {
					return
				}
			case errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil:
				if send(LiveLine{HB: true}) != nil {
					return
				}
			case errors.Is(err, io.EOF):
				_ = send(LiveLine{Closed: true})
				return
			case errors.Is(err, ErrWatchOverflow):
				_ = send(LiveLine{Error: "overflow"})
				return
			default:
				return
			}
		}
	})
}

// KillPath is the gateway's internal endpoint an admin's 断开会话 lands
// on (audit spec §17).
const KillPath = "/internal/kill"

// KillHandler serves POST /internal/kill?session= to webui replicas
// holding the shared key: 204 once the session is being torn down, 404
// when it is not open on this gateway.
func KillHandler(rec *Recorder, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("session")
		if err := rec.Kill(id, "admin_kill"); err != nil {
			http.Error(w, "no such open session", http.StatusNotFound)
			return
		}
		slog.Info("audit_session_killed", "session", id)
		w.WriteHeader(http.StatusNoContent)
	})
}
