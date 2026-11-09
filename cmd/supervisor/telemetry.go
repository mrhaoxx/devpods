// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
)

const (
	telemetryMaxBackoff = 60 * time.Second
	telemetryStableRun  = 10 * time.Minute
	telemetryMemLimit   = "24MiB"
)

// telemetrySup keeps the `telemetry` child running. PID 1's reap loop
// owns wait4(-1), so it hands every exit it does not track to exited().
// A telemetry crash never influences the supervisor's own exit.
type telemetrySup struct {
	argv    []string
	env     []string
	initial time.Duration

	mu       sync.Mutex
	pid      int
	exitCh   chan int
	starting bool
	early    map[int]int // exits reaped while a start was in flight
	stopped  bool
	stopCh   chan struct{}
}

func newTelemetrySup(sshdPid int) *telemetrySup {
	argv := []string{"/opt/devpod/devpod-supervisor", "telemetry"}
	if self, err := os.Executable(); err == nil {
		argv[0] = self
	}
	if override := os.Getenv("SUPERVISOR_TELEMETRY_CMD"); override != "" {
		argv = []string{override}
	}
	initial := time.Second
	if d, err := time.ParseDuration(os.Getenv("SUPERVISOR_TELEMETRY_BACKOFF")); err == nil && d > 0 {
		initial = d
	}
	env := append(os.Environ(), "GOMEMLIMIT="+telemetryMemLimit, "SUPERVISOR_SSHD_PID="+strconv.Itoa(sshdPid))
	return &telemetrySup{argv: argv, env: env, initial: initial, early: map[int]int{}, stopCh: make(chan struct{})}
}

// exited is called by the reap loop for pids it does not track.
func (t *telemetrySup) exited(pid, code int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if pid == t.pid && t.exitCh != nil {
		t.exitCh <- code
		t.pid, t.exitCh = 0, nil
		return
	}
	if t.starting {
		t.early[pid] = code
	}
}

// stop terminates the child and ends the restart loop.
func (t *telemetrySup) stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.stopped = true
	close(t.stopCh)
	if t.pid > 0 {
		_ = syscall.Kill(-t.pid, syscall.SIGTERM)
	}
}

func (t *telemetrySup) loop() {
	delay := t.initial
	for {
		t.mu.Lock()
		if t.stopped {
			t.mu.Unlock()
			return
		}
		t.starting = true
		t.mu.Unlock()

		cmd := exec.Command(t.argv[0], t.argv[1:]...)
		cmd.Env = t.env
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		started := time.Now()
		err := cmd.Start()

		ch := make(chan int, 1)
		t.mu.Lock()
		t.starting = false
		if err == nil {
			t.pid, t.exitCh = cmd.Process.Pid, ch
			if code, ok := t.early[t.pid]; ok {
				t.pid, t.exitCh = 0, nil
				ch <- code
			}
		}
		clear(t.early)
		t.mu.Unlock()

		if err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: start telemetry: %v\n", err)
		} else {
			code := <-ch
			fmt.Fprintf(os.Stderr, "supervisor: telemetry exited code=%d; restarting in %v\n", code, delay)
			if time.Since(started) >= telemetryStableRun {
				delay = t.initial
			}
		}
		select {
		case <-t.stopCh:
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, telemetryMaxBackoff)
	}
}

// runTelemetry is the `devpod-supervisor telemetry` entry point.
func runTelemetry() int {
	path := envOr("SUPERVISOR_TELEMETRY_CONFIG", config.ConfigPath)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry: %v\n", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	env := agent.DefaultEnv()
	env.ConfigPath = path
	if err := agent.Run(ctx, cfg, env); err != nil {
		fmt.Fprintf(os.Stderr, "telemetry: %v\n", err)
		return 1
	}
	return 0
}
