// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package leader elects one webui replica (spec §5.10). The leader alone
// accepts telemetry ingest, writes ClickHouse, keeps the live state and
// runs the minute jobs.
package leader

import (
	"context"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Default lease timings (spec Global Constraints).
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// Elector runs a Lease election, re-joining after losing leadership.
type Elector struct {
	Client                                    kubernetes.Interface
	Namespace, Name, Identity                 string
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration

	leading atomic.Bool
}

// IsLeader reports whether this replica currently leads.
func (e *Elector) IsLeader() bool { return e.leading.Load() }

// Run participates until ctx ends. onStart gets a context cancelled when
// leadership ends; onStop runs (and IsLeader turns false) before the
// lease is released, so a successor never overlaps.
func (e *Elector) Run(ctx context.Context, onStart func(ctx context.Context), onStop func()) {
	lock := &resourcelock.LeaseLock{
		Client:     e.Client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: e.Identity},
	}
	lock.LeaseMeta.Namespace, lock.LeaseMeta.Name = e.Namespace, e.Name
	cfg := leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   or(e.LeaseDuration, DefaultLeaseDuration),
		RenewDeadline:   or(e.RenewDeadline, DefaultRenewDeadline),
		RetryPeriod:     or(e.RetryPeriod, DefaultRetryPeriod),
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(lctx context.Context) {
				e.leading.Store(true)
				onStart(lctx)
			},
			OnStoppedLeading: func() {
				if e.leading.Swap(false) {
					onStop()
				}
			},
		},
	}
	for ctx.Err() == nil {
		le, err := leaderelection.NewLeaderElector(cfg)
		if err != nil {
			return
		}
		le.Run(ctx) // returns when leadership is lost or ctx ends
		select {
		case <-ctx.Done():
		case <-time.After(cfg.RetryPeriod):
		}
	}
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
