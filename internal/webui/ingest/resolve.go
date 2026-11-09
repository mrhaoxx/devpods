// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Pending marks a session the gateway has not written yet; the minute
// backfill job re-inserts the row once it resolves (spec §5.4).
const Pending = "pending"

// SessionSource is the part of *store.CH the resolver uses.
type SessionSource interface {
	OpenSessionsByBackend(ctx context.Context, since time.Time) (map[string]map[string]store.SessionRef, error)
}

// Resolver maps an ssh connection's peer address (the gateway's
// backend_local_addr) to its gateway session.
type Resolver struct {
	Source   SessionSource
	Every    time.Duration // refresh period; default 5 s
	Lookback time.Duration // sessions opened within; default 7 days

	mu sync.RWMutex
	m  map[string]map[string]store.SessionRef
}

// Refresh reloads the session map.
func (r *Resolver) Refresh(ctx context.Context) error {
	lb := r.Lookback
	if lb <= 0 {
		lb = 7 * 24 * time.Hour
	}
	m, err := r.Source.OpenSessionsByBackend(ctx, time.Now().Add(-lb))
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.m = m
	r.mu.Unlock()
	return nil
}

// Run refreshes until ctx ends.
func (r *Resolver) Run(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.Refresh(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("telemetry: refresh sessions", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Resolve returns (session_id, user) for a DevPod's peer address:
// ("", "") without a peer, (Pending, "") while unknown.
func (r *Resolver) Resolve(uid, peer string) (string, string) {
	if peer == "" {
		return "", ""
	}
	if r == nil {
		return Pending, ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.m[uid][peer]; ok {
		return s.SessionID, s.User
	}
	return Pending, ""
}

// User is State.ResolveUser.
func (r *Resolver) User(uid, peer string) string {
	_, u := r.Resolve(uid, peer)
	return u
}
