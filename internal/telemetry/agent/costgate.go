// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import "time"

// costBudget: an expensive read may use at most this share of one core.
const costBudget = 1000 // 1/1000 = 0.1 %

// costGate spaces a read whose cost depends on the node, not the
// container (a sock_diag dump or /proc/net/tcp walks the kernel's
// node-wide TCP hash table): after a read that took d, the next is due
// after d×1000, clamped to [min, max].
type costGate struct {
	min, max time.Duration
	last     time.Time
	every    time.Duration
}

func (g *costGate) due(now time.Time) bool {
	return g.last.IsZero() || now.Sub(g.last) >= max(g.every, g.min)
}

func (g *costGate) done(now time.Time, took time.Duration) {
	g.last = now
	g.every = min(max(took*costBudget, g.min), g.max)
}
