// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// lifecycle remembers each live process's start record and each ssh
// connection's first sighting, so exit / close events are complete rows:
// the leader can write them without having seen the start (failover).
type lifecycle struct {
	procs map[procfs.PID]wire.ProcEvent
	conns map[procfs.PID]time.Time
}

func newLifecycle() *lifecycle {
	return &lifecycle{procs: map[procfs.PID]wire.ProcEvent{}, conns: map[procfs.PID]time.Time{}}
}

// event fills FirstSeen (and, on exit, the start's details) in place.
func (l *lifecycle) event(e *wire.ProcEvent) {
	id := procfs.PID{Pid: e.Pid, Start: e.Start}
	switch e.Event {
	case "process_start":
		e.FirstSeen = e.TS
		s := *e
		if e.Attr != nil {
			a := *e.Attr
			s.Attr = &a
		}
		l.procs[id] = s
	case "process_exit":
		if s, ok := l.procs[id]; ok {
			e.FirstSeen, e.PPid, e.Exe, e.Cmdline, e.UID = s.FirstSeen, s.PPid, s.Exe, s.Cmdline, s.UID
			delete(l.procs, id)
		}
	default:
		e.FirstSeen = e.TS
	}
}

// resolved re-issues the start rows of processes attributed to conn that
// were reported before its peer was known: with the peer the leader can
// link them to the session (same first_seen, later TS = higher version).
func (l *lifecycle) resolved(conn procfs.PID, peer string, now time.Time) []wire.ProcEvent {
	var out []wire.ProcEvent
	for id, s := range l.procs {
		if s.Attr == nil || s.Attr.Peer != "" || s.Attr.ConnPid != conn.Pid || s.Attr.ConnStart != conn.Start {
			continue
		}
		a := *s.Attr
		a.Peer = peer
		s.Attr = &a
		l.procs[id] = s
		e := s
		e.TS = now
		attr := a
		e.Attr = &attr
		out = append(out, e)
	}
	return out
}

// connSeen returns the connection's first sighting, recording now if new.
func (l *lifecycle) connSeen(c procfs.PID, now time.Time) time.Time {
	if t, ok := l.conns[c]; ok {
		return t
	}
	l.conns[c] = now
	return now
}

// connClosed forgets the connection and returns its first sighting.
func (l *lifecycle) connClosed(c procfs.PID) time.Time {
	t := l.conns[c]
	delete(l.conns, c)
	return t
}
