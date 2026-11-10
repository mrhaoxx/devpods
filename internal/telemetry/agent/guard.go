// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// The agent runs inside the supervisor, PID 1 of the user's container
// (spec §5.5): a panic that reached the runtime would end the container.
// Every goroutine the agent starts goes through goGuarded, and Run hands
// a recovered panic back as an error so the supervisor restarts it.

type panicError struct {
	where string
	val   any
	stack []byte
}

func (e *panicError) Error() string {
	return fmt.Sprintf("telemetry: %s panicked: %v\n%s", e.where, e.val, e.stack)
}

// IsPanic reports whether err is a recovered agent panic.
func IsPanic(err error) bool {
	var p *panicError
	return errors.As(err, &p)
}

// safely runs fn and returns its panic, if any, as an error.
func safely(where string, fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{where: where, val: r, stack: debug.Stack()}
		}
	}()
	fn()
	return nil
}

// goGuarded runs fn on a new goroutine; a panic cancels the agent with
// the panic as the cause instead of killing the process.
func goGuarded(wg *sync.WaitGroup, where string, cancel context.CancelCauseFunc, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := safely(where, fn); err != nil {
			cancel(err)
		}
	}()
}

// waitAtMost waits for wg but not past d: a goroutine stuck in a
// syscall must not keep the supervisor from restarting the agent.
func waitAtMost(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}
