// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"math"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

// PID 1 hands the agent its sshd and workload pids directly; the
// container's own SUPERVISOR_* variables play no part.
func TestTelemetryEnv_PassesPids(t *testing.T) {
	t.Setenv("SUPERVISOR_SSHD_PID", "999")
	t.Setenv("SUPERVISOR_WORKLOAD_PID", "998")
	env := telemetryEnv("/etc/devpod/telemetry.json", 42, 77)
	if env.SSHDPid() != 42 || env.WorkloadPid() != 77 || env.ConfigPath != "/etc/devpod/telemetry.json" {
		t.Fatalf("sshd %d workload %d config %q", env.SSHDPid(), env.WorkloadPid(), env.ConfigPath)
	}
}

// The agent is sequential and shares PID 1's runtime: without a cap Go
// uses one P per core, and on a 104-core node each GC cycle fanned out
// over all of them (spec §14 item 4); the soft memory limit covers the
// agent's budget plus PID 1's own few MiB.
func TestTelemetryLimits(t *testing.T) {
	oldP := runtime.GOMAXPROCS(64)
	oldM := debug.SetMemoryLimit(math.MaxInt64)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(oldP)
		debug.SetMemoryLimit(oldM)
	})
	applyTelemetryLimits()
	if p := runtime.GOMAXPROCS(0); p != 2 {
		t.Errorf("GOMAXPROCS %d, want 2", p)
	}
	if m := debug.SetMemoryLimit(-1); m != 32<<20 {
		t.Errorf("memory limit %d, want 32 MiB", m)
	}
}

// A panicking agent is recovered — it runs inside PID 1, so an escaped
// panic would end the user's container — and restarted with growing
// delays; stop ends the restarts.
func TestTelemetryRunner_PanicIsRestartedWithBackoff(t *testing.T) {
	var runs atomic.Int32
	r := newTelemetryRunner(func(context.Context) error {
		runs.Add(1)
		panic("collector bug")
	}, 50*time.Millisecond)
	r.start()
	time.Sleep(time.Second)
	r.stop()
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("the runner did not stop")
	}
	// runs at 0, 50, 150, 350 and 750 ms
	n := runs.Load()
	if n < 4 || n > 6 {
		t.Fatalf("%d runs in 1s; want restarts with exponential backoff", n)
	}
	time.Sleep(200 * time.Millisecond)
	if runs.Load() != n {
		t.Fatal("the agent ran again after stop")
	}
}

// stop cancels a running agent and does not restart it.
func TestTelemetryRunner_StopCancelsTheRun(t *testing.T) {
	var runs atomic.Int32
	started := make(chan struct{}, 1)
	r := newTelemetryRunner(func(ctx context.Context) error {
		runs.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return nil
	}, time.Millisecond)
	r.start()
	<-started
	r.stop()
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("the run was not cancelled")
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d runs; a stopped agent is not restarted", n)
	}
	r.stop() // twice is fine
	var none *telemetryRunner
	none.stop() // and so is no telemetry at all
}
