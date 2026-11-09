// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package live

import (
	"context"
	"sync"
	"time"
)

// freshFor: a new subscriber gets the last snapshot only if it is this
// recent (otherwise it waits for the next one).
const freshFor = 3 * time.Second

// Hub fans snapshots out to browser streams (and, on the leader, to
// followers). It keeps only the latest snapshot per subscriber.
type Hub struct {
	mu        sync.Mutex
	subs      map[chan Snapshot]struct{}
	last      *Snapshot
	connected bool
}

// NewHub creates a hub that reports connected (the leader's view).
func NewHub() *Hub { return &Hub{subs: map[chan Snapshot]struct{}{}, connected: true} }

// Publish delivers s to every subscriber, replacing an unread snapshot.
func (h *Hub) Publish(s Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = &s
	for ch := range h.subs {
		select {
		case <-ch: // drop the stale one
		default:
		}
		select {
		case ch <- s:
		default:
		}
	}
}

// Subscribe returns a channel of snapshots and its cancel function.
func (h *Hub) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if h.last != nil && time.Since(h.last.TS) < freshFor {
		ch <- *h.last
	}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
		})
	}
}

// Subscribers counts current subscribers.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// SetConnected records whether the follower link to the leader is up.
func (h *Hub) SetConnected(c bool) {
	h.mu.Lock()
	h.connected = c
	h.mu.Unlock()
}

// Connected reports whether live data is flowing (always true on the
// leader; on a follower, whether its link to the leader is up).
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected
}

// Tick builds and publishes a snapshot every period while anyone is
// subscribed (browsers on this replica or followers); otherwise it
// does no work. It returns when ctx ends (leadership lost).
func Tick(ctx context.Context, h *Hub, every time.Duration, build func() Snapshot) {
	h.SetConnected(true)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if h.Subscribers() > 0 {
				h.Publish(build())
			}
		}
	}
}

// Last returns the latest snapshot if it is fresh.
func (h *Hub) Last() (Snapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last == nil || time.Since(h.last.TS) >= freshFor {
		return Snapshot{}, false
	}
	return *h.last, true
}
