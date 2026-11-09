// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

func TestLifecycle_ExitCarriesStartDetails(t *testing.T) {
	l := newLifecycle()
	t0 := time.Unix(100, 0).UTC()
	start := wire.ProcEvent{TS: t0, Event: "process_start", Pid: 7, Start: 9, PPid: 3, Exe: "/usr/bin/python3", Cmdline: "python x.py", UID: 1000}
	l.event(&start)
	if !start.FirstSeen.Equal(t0) {
		t.Fatalf("start first_seen %v", start.FirstSeen)
	}
	exit := wire.ProcEvent{TS: t0.Add(time.Minute), Event: "process_exit", Pid: 7, Start: 9}
	l.event(&exit)
	if !exit.FirstSeen.Equal(t0) || exit.Exe != "/usr/bin/python3" || exit.Cmdline != "python x.py" || exit.PPid != 3 || exit.UID != 1000 {
		t.Fatalf("exit must repeat the start row: %+v", exit)
	}
	if len(l.procs) != 0 {
		t.Fatal("exited process still remembered")
	}
	c := procfs.PID{Pid: 50, Start: 100}
	if got := l.connSeen(c, t0); !got.Equal(t0) {
		t.Fatal(got)
	}
	if got := l.connSeen(c, t0.Add(time.Second)); !got.Equal(t0) {
		t.Fatalf("a re-reported connection keeps its first_seen: %v", got)
	}
	if got := l.connClosed(c); !got.Equal(t0) || len(l.conns) != 0 {
		t.Fatalf("closed: %v %d", got, len(l.conns))
	}
}
