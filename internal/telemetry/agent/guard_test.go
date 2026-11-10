// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// The agent runs inside PID 1: a panic on any of its goroutines must not
// reach the runtime (it would end the user's container). goGuarded turns
// it into the agent's cancel cause, so Run returns it and the supervisor
// restarts the agent.
func TestGoGuarded_PanicCancelsTheAgent(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var wg sync.WaitGroup
	goGuarded(&wg, "uploader", cancel, func() { panic("boom") })
	wg.Wait()
	err := context.Cause(ctx)
	if err == nil || !strings.Contains(err.Error(), "uploader") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("cause %v", err)
	}
	if !IsPanic(err) {
		t.Fatalf("%v is not reported as a panic", err)
	}
}

func TestSafely(t *testing.T) {
	if err := safely("gpu", func() {}); err != nil {
		t.Fatalf("no panic, got %v", err)
	}
	err := safely("gpu", func() { panic("bad csv") })
	if err == nil || !strings.Contains(err.Error(), "gpu") || !strings.Contains(err.Error(), "bad csv") {
		t.Fatalf("got %v", err)
	}
}
