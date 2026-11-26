// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// The record of what was done to DevPods, jobs and machines (the audit
// store's events): written after the change itself, and never holding it
// up — a record the store does not take now waits in memory and is tried
// again, the oldest going first when too many wait.

// eventBuffer: how many records may wait for the store.
const eventBuffer = 10000

// eventWriter keeps the records not written yet, in order.
type eventWriter struct {
	mu      sync.Mutex
	pending []store.EventRow
	running bool // a writer goroutine is at work
	dropped int  // records given up, all told
}

// record writes ev to the audit store, now or later; without a store it
// does nothing. It never blocks on the store.
func (s *Server) record(ctx context.Context, ev store.EventRow) {
	if s.Audit == nil || s.Audit.Store == nil {
		return
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	w := &s.events
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) >= eventBuffer {
		w.pending = w.pending[1:]
		w.dropped++
		if w.dropped == 1 || w.dropped%100 == 0 {
			slog.Warn("events_dropped", "dropped", w.dropped, "why", "the audit store has not taken them")
		}
	}
	w.pending = append(w.pending, ev)
	if !w.running {
		w.running = true
		go s.writeEvents(context.WithoutCancel(ctx))
	}
}

// writeEvents writes what waits, in order, until nothing does; what the
// store does not take is tried again after eventRetry.
func (s *Server) writeEvents(ctx context.Context) {
	w := &s.events
	retry := s.eventRetry
	if retry <= 0 {
		retry = 10 * time.Second
	}
	for {
		w.mu.Lock()
		if len(w.pending) == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		ev := w.pending[0]
		w.mu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.Audit.Store.InsertEvent(wctx, ev)
		cancel()
		if err != nil {
			slog.Warn("event_write", "kind", ev.Kind, "object", ev.Object, "action", ev.Action, "err", err)
			time.Sleep(retry)
			continue
		}
		w.mu.Lock()
		// it may have been dropped meanwhile (the buffer full): take it
		// off only if it is still the first
		if len(w.pending) > 0 && w.pending[0] == ev {
			w.pending = w.pending[1:]
		}
		w.mu.Unlock()
	}
}

// eventFrom: a record of what a request does — who, how, from where.
func eventFrom(r *http.Request, sess Session) store.EventRow {
	via := sess.Via
	if via == "" {
		via = "web"
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return store.EventRow{Actor: sess.User, Via: via, Origin: sess.Origin, ClientIP: ip, RequestID: r.Header.Get("X-Request-Id")}
}

// systemEvent: a record of what the console did by itself, as actor
// (system:queue, system:reservation, …).
func systemEvent(actor string) store.EventRow {
	return store.EventRow{Actor: actor, Via: "system"}
}
