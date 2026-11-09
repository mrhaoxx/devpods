// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

type fakeJobStore struct {
	mu       sync.Mutex
	conns    []store.SSHConnRow
	procs    []store.ProcessRow
	cmds     []store.PendingCommand
	sessions []store.BackendSession
	hook     []store.SessionRow
	info     map[string]time.Time
	idle     []string
	block    bool
	inserts  []store.TelemetryBatch
}

func (f *fakeJobStore) wait(ctx context.Context) error {
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeJobStore) IdleCandidates(ctx context.Context, _ time.Time, _, _ float64) ([]string, error) {
	return f.idle, f.wait(ctx)
}
func (f *fakeJobStore) PendingAttributions(ctx context.Context, _ time.Time) ([]store.SSHConnRow, error) {
	return f.conns, f.wait(ctx)
}
func (f *fakeJobStore) PendingProcesses(ctx context.Context, _ time.Time) ([]store.ProcessRow, error) {
	return f.procs, f.wait(ctx)
}
func (f *fakeJobStore) PendingCommands(ctx context.Context, _, _ time.Time) ([]store.PendingCommand, error) {
	return f.cmds, f.wait(ctx)
}
func (f *fakeJobStore) BackendSessions(ctx context.Context, _ time.Time) ([]store.BackendSession, error) {
	return f.sessions, f.wait(ctx)
}
func (f *fakeJobStore) HookMissing(ctx context.Context, _, _ time.Time) ([]store.SessionRow, error) {
	return f.hook, f.wait(ctx)
}
func (f *fakeJobStore) InfoSeen(ctx context.Context, _ time.Time) (map[string]time.Time, error) {
	return f.info, f.wait(ctx)
}
func (f *fakeJobStore) InsertTelemetry(_ context.Context, b store.TelemetryBatch, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserts = append(f.inserts, b)
	return nil
}

func (f *fakeJobStore) all() store.TelemetryBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out store.TelemetryBatch
	for _, b := range f.inserts {
		out.SSHConns = append(out.SSHConns, b.SSHConns...)
		out.ProcRows = append(out.ProcRows, b.ProcRows...)
		out.Commands = append(out.Commands, b.Commands...)
		out.Alerts = append(out.Alerts, b.Alerts...)
	}
	return out
}

func newJobs(st *fakeJobStore) *Jobs {
	return &Jobs{Store: st, IdleCPUPct: 5, IdleGPUPct: 5, SilentAfter: 3 * time.Minute,
		Running: func(context.Context) []RunningDevPod { return nil }, LastSample: func(string) (time.Time, bool) { return time.Time{}, false },
		SetIdle: func(map[string]bool) {}}
}

func TestJobs_BackfillReinsertsHigherVersion(t *testing.T) {
	now := time.Now().UTC()
	at := now.Add(-10 * time.Minute)
	peer := "10.0.0.9:41000"
	st := &fakeJobStore{
		conns: []store.SSHConnRow{{DevPodUID: "u1", Pid: 50, StartTime: 100, PeerAddr: peer, FirstSeen: at, SessionID: "pending", Version: 7}},
		procs: []store.ProcessRow{{DevPodUID: "u1", Pid: 60, StartTime: 110, Peer: peer, FirstSeen: at, SessionID: "pending", Attribution: "tree", Version: 9},
			{DevPodUID: "u1", Pid: 70, StartTime: 130, Peer: "10.0.0.7:1", FirstSeen: at, SessionID: "pending", Version: 9}},
		cmds: []store.PendingCommand{{CommandRow: store.CommandRow{EventID: "e1", TS: at.Add(time.Minute), DevPodUID: "u1", SessionID: "pending",
			Source: "shell_hook", Attribution: "pending", Version: 1}, Peer: peer, ProcAttribution: "tree"}},
		sessions: []store.BackendSession{
			{UID: "u1", Addr: peer, SessionID: "S-old", User: "bob", OpenedAt: at.Add(-3 * time.Hour), ClosedAt: timePtr(at.Add(-2 * time.Hour))},
			{UID: "u1", Addr: peer, SessionID: "S1", User: "alice", OpenedAt: at.Add(-time.Minute)},
		},
	}
	j := newJobs(st)
	if err := j.RunOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	b := st.all()
	if len(b.SSHConns) != 1 || b.SSHConns[0].SessionID != "S1" || b.SSHConns[0].User != "alice" || b.SSHConns[0].Version != 8 {
		t.Fatalf("ssh_conns %+v", b.SSHConns)
	}
	if len(b.ProcRows) != 1 || b.ProcRows[0].Pid != 60 || b.ProcRows[0].SessionID != "S1" || b.ProcRows[0].Version != 10 {
		t.Fatalf("processes (the unmatched one stays pending) %+v", b.ProcRows)
	}
	if len(b.Commands) != 1 || b.Commands[0].SessionID != "S1" || b.Commands[0].User != "alice" || b.Commands[0].Attribution != "exact" || b.Commands[0].Version != 2 {
		t.Fatalf("commands %+v", b.Commands)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestJobs_AgentSilentOncePerIncident(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Minute)
	st := &fakeJobStore{info: map[string]time.Time{"u1": t0.Add(10 * time.Second)}}
	var last time.Time
	var events []string
	j := newJobs(st)
	j.Since = t0
	j.Running = func(context.Context) []RunningDevPod { return []RunningDevPod{{UID: "u1", Name: "d1", PodStart: t0}} }
	j.LastSample = func(string) (time.Time, bool) { return last, !last.IsZero() }
	j.Event = func(uid, kind, _ string) { events = append(events, uid+"/"+kind) }
	run := func(d time.Duration) {
		t.Helper()
		if err := j.RunOnce(context.Background(), t0.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		n := 0
		for _, a := range st.all().Alerts {
			if a.Kind == "agent_silent" && a.DevPodUID == "u1" && a.Source == "webui" {
				n++
			}
		}
		return n
	}
	run(2 * time.Minute)
	if count() != 0 {
		t.Fatal("alerted before the threshold")
	}
	run(3*time.Minute + 30*time.Second)
	run(4*time.Minute + 30*time.Second)
	if count() != 1 {
		t.Fatalf("one alert per incident, got %d", count())
	}
	last = t0.Add(5 * time.Minute) // samples resume
	run(5*time.Minute + 10*time.Second)
	run(9 * time.Minute) // silent again for 4 min
	if count() != 2 || len(events) != 2 {
		t.Fatalf("second incident: %d alerts, events %v", count(), events)
	}
	// a DevPod without telemetry in this Pod's lifetime never alerts
	st.info = map[string]time.Time{"u1": t0.Add(-time.Hour)}
	j.silent = nil
	run(20 * time.Minute)
	if count() != 2 {
		t.Fatal("alerted for a DevPod whose Pod has no telemetry")
	}
}

func TestJobs_StopWhenLeadershipLost(t *testing.T) {
	st := &fakeJobStore{block: true, idle: []string{"u1"}}
	j := newJobs(st)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- j.RunOnce(ctx, time.Now()) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled run must report it")
		}
	case <-time.After(time.Second):
		t.Fatal("RunOnce kept running after leadership was lost")
	}
	if n := len(st.all().Alerts) + len(st.all().SSHConns); n != 0 {
		t.Fatalf("%d rows written after cancel", n)
	}
	st.block = false
	idle := map[string]bool{}
	j.SetIdle = func(m map[string]bool) { idle = m }
	if err := j.RunOnce(context.Background(), time.Now()); err != nil || !idle["u1"] {
		t.Fatalf("idle job: %v %v", idle, err)
	}
}
