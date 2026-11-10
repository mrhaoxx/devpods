// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
)

const (
	telemetryInitialBackoff = time.Second
	telemetryMaxBackoff     = 60 * time.Second
	telemetryStableRun      = 10 * time.Minute
	// The agent runs inside PID 1 and shares its Go runtime. It is
	// sequential: two Ps keep each GC cycle from fanning out over every
	// core of a big node (spec §14 item 4). The soft memory limit is the
	// agent's old 24 MiB budget (measured RSS 14-16 MB) plus PID 1's own
	// few MiB; it only makes the GC work harder, it never kills anything.
	telemetryMaxProcs = 2
	telemetryMemLimit = 32 << 20
)

// applyTelemetryLimits sets PID 1's runtime limits for the agent; the
// container's GOMAXPROCS / GOMEMLIMIT (meant for the user's programs)
// are overridden here and still reach sshd and the user's command
// untouched through the environment.
func applyTelemetryLimits() {
	runtime.GOMAXPROCS(telemetryMaxProcs)
	debug.SetMemoryLimit(telemetryMemLimit)
}

// telemetryEnv is the agent's view of the container, with PID 1's sshd
// and workload pids handed over directly.
func telemetryEnv(configPath string, sshdPid, workloadPid int) agent.Env {
	env := agent.DefaultEnv()
	env.ConfigPath = configPath
	env.SSHDPid = func() int { return sshdPid }
	env.WorkloadPid = func() int { return workloadPid }
	return env
}

// agentRun loads the config afresh for every (re)start and runs the agent.
func agentRun(configPath string, sshdPid, workloadPid int) func(context.Context) error {
	return func(ctx context.Context) error {
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			return err
		}
		return agent.Run(ctx, cfg, telemetryEnv(configPath, sshdPid, workloadPid))
	}
}

// telemetryRunner runs the telemetry agent inside PID 1 — no process of
// its own — and restarts it after an error or a panic with backoff
// (initial → 60 s, reset after a 10-minute run). The agent never
// influences the supervisor's exit.
type telemetryRunner struct {
	run     func(context.Context) error
	initial time.Duration
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

func newTelemetryRunner(run func(context.Context) error, initial time.Duration) *telemetryRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &telemetryRunner{run: run, initial: initial, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (t *telemetryRunner) start() { go t.loop() }

// stop cancels the agent and ends the restarts; it does not wait.
func (t *telemetryRunner) stop() {
	if t != nil {
		t.cancel()
	}
}

// runRecovered runs the agent once; a panic that escaped the agent's own
// guards (its main loop) comes back as an error instead of ending PID 1.
func runRecovered(ctx context.Context, run func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("telemetry panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return run(ctx)
}

func (t *telemetryRunner) loop() {
	defer close(t.done)
	delay := t.initial
	for {
		started := time.Now()
		err := runRecovered(t.ctx, t.run)
		if t.ctx.Err() != nil {
			return
		}
		if time.Since(started) >= telemetryStableRun {
			delay = t.initial
		}
		fmt.Fprintf(os.Stderr, "supervisor: telemetry stopped (%v); restarting in %v\n", err, delay)
		select {
		case <-t.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, telemetryMaxBackoff)
	}
}
