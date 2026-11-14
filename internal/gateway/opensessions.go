// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"sort"
	"sync"

	"github.com/mrhaoxx/devpod/internal/gwlink"
)

// OpenSessions are the SSH sessions this gateway carries now, for the
// console to show.
var OpenSessions = &Sessions{open: map[*openSession]struct{}{}}

// Sessions is a set of open sessions.
type Sessions struct {
	mu   sync.Mutex
	open map[*openSession]struct{}
}

type openSession struct {
	gwlink.Session
	stats *ProxyStats
}

// Add records a session until the returned func is called; its bytes
// are read from stats as they grow.
func (s *Sessions) Add(sess gwlink.Session, stats *ProxyStats) (remove func()) {
	o := &openSession{Session: sess, stats: stats}
	s.mu.Lock()
	s.open[o] = struct{}{}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.open, o)
		s.mu.Unlock()
	}
}

// List is the open sessions, the oldest first.
func (s *Sessions) List() []gwlink.Session {
	s.mu.Lock()
	out := make([]gwlink.Session, 0, len(s.open))
	for o := range s.open {
		v := o.Session
		if o.stats != nil {
			v.Up, v.Down = o.stats.BytesClientToBackend.Load(), o.stats.BytesBackendToClient.Load()
		}
		out = append(out, v)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}
