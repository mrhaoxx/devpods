// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package live

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

func clock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func container(cpu float64, ws uint64) wire.Record {
	return wire.Record{K: wire.KContainer, C: &wire.Container{CPU: cpu, MemWorkingSet: ws, NetRx: 100, NetKind: "pod"}}
}

func TestState_StaleSampleIsNoData(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewState(clock(&now))
	s.Apply("u1", []wire.Record{container(1.5, 100)})
	in := Inputs{DevPods: []DevPodRef{{UID: "u1", Name: "d", Owner: "alice", Node: "g206", Phase: "Running", CPULimit: 4}}}
	snap := s.Snapshot(in)
	if p := snap.DevPods["u1"]; p.CPU == nil || *p.CPU != 1.5 || p.CPULimit != 4 {
		t.Fatalf("fresh: %+v", p)
	}
	now = now.Add(6 * time.Second)
	snap = s.Snapshot(in)
	p, ok := snap.DevPods["u1"]
	if !ok || p.CPU != nil || p.Mem != nil || p.NetRx != nil {
		t.Fatalf("a sample older than 5s is no data, got %+v (present=%v)", p, ok)
	}
}

func TestRollup_NodeAndUser(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewState(clock(&now))
	s.Apply("a", []wire.Record{container(2.0, 1<<30), {K: wire.KGPU, G: &wire.GPU{Index: 0, UUID: "GPU-0", Util: 90, MemUsed: 1000, MemTotal: 81920, Power: 312}}})
	s.Apply("b", []wire.Record{container(3.5, 2<<30), {K: wire.KGPU, G: &wire.GPU{Index: 1, UUID: "GPU-1", Util: 50}},
		{K: wire.KGPU, G: &wire.GPU{Index: 2, UUID: "GPU-2", Util: 40}}})
	in := Inputs{
		DevPods: []DevPodRef{
			{UID: "a", Name: "a1", Owner: "alice", Node: "g206", Phase: "Running", GPURequest: 1, CPURequest: 4},
			{UID: "b", Name: "b1", Owner: "alice", Node: "g206", Phase: "Running", GPURequest: 2, CPURequest: 8},
		},
		Nodes: []NodeRef{{Name: "g206", Ready: true, CPUAllocatable: 64, GPUAllocatable: 4}},
	}
	snap := s.Snapshot(in)
	if len(snap.Nodes) != 1 {
		t.Fatalf("%+v", snap.Nodes)
	}
	// a card's memory and power reach the live view
	if g := snap.DevPods["a"].GPU; len(g) != 1 || g[0].MemUsed != 1000 || g[0].MemTotal != 81920 || g[0].Power != 312 {
		t.Fatalf("a's card %+v", g)
	}
	n := snap.Nodes[0]
	if n.CPUUsed != 5.5 || n.CPUAllocated != 12 || n.GPUUsed != 3 || n.GPUTotal != 4 || n.DevPods != 2 || len(n.Cards) != 3 {
		t.Fatalf("node %+v", n)
	}
	sort.Slice(n.Cards, func(i, j int) bool { return n.Cards[i].Index < n.Cards[j].Index })
	if n.Cards[0].DevPod != "a1" || n.Cards[1].DevPod != "b1" || n.Cards[1].Util != 50 {
		t.Fatalf("cards %+v", n.Cards)
	}
	if len(snap.Users) != 1 || snap.Users[0].CPU != 5.5 || snap.Users[0].GPU != 3 || snap.Users[0].Running != 2 {
		t.Fatalf("users %+v", snap.Users)
	}
	if snap.KPI.CPUUsed != 5.5 || snap.KPI.CPUAllocatable != 64 || snap.KPI.GPUUsed != 3 || snap.KPI.Running != 2 {
		t.Fatalf("kpi %+v", snap.KPI)
	}
}

func TestRollup_Attention(t *testing.T) {
	now := time.Unix(100000, 0)
	s := NewState(clock(&now))
	in := Inputs{
		DevPods: []DevPodRef{
			{UID: "idle", Name: "i", Owner: "bob", Phase: "Running", GPURequest: 1},
			{UID: "pend", Name: "p", Owner: "carol", Phase: "Pending", PhaseSince: now.Add(-11 * time.Minute)},
			{UID: "big", Name: "q", Owner: "dave", Phase: "Running", CPURequest: 2, CPULimit: 7.6},
		},
		Quotas: map[string]Quota{"dave": {CPU: 8}},
		Idle:   map[string]bool{"idle": true},
		Alerts: []AlertRef{{TS: now.Add(-10 * time.Minute), DevPodUID: "big", Kind: "untracked_entry"}, {TS: now.Add(-2 * time.Hour), Kind: "old"}},
	}
	snap := s.Snapshot(in)
	kinds := map[string]bool{}
	for _, a := range snap.Attention {
		kinds[a.Kind] = true
	}
	for _, k := range []string{"idle_gpu", "pending_long", "quota_high", "alert"} {
		if !kinds[k] {
			t.Errorf("missing %s in %+v", k, snap.Attention)
		}
	}
	if len(snap.Attention) != 4 || snap.Attention[0].Kind != "alert" {
		t.Fatalf("one of each, alerts first (the old alert dropped): %+v", snap.Attention)
	}
}

// Two clusters with a node of the same name: two nodes, each with its
// own DevPods; the cluster's own numbers beside the console's; a quota
// is judged in the cluster it is set for.
func TestRollup_PerCluster(t *testing.T) {
	now := time.Unix(100000, 0)
	s := NewState(clock(&now))
	in := Inputs{
		Nodes: []NodeRef{
			{Cluster: "a", Name: "m602", Ready: true, CPUAllocatable: 64},
			{Cluster: "b", Name: "m602", Ready: true, CPUAllocatable: 8},
		},
		DevPods: []DevPodRef{
			{Cluster: "a", UID: "1", Name: "x", Owner: "dave", Node: "m602", Phase: "Running", CPURequest: 2, CPULimit: 2},
			{Cluster: "b", UID: "2", Name: "x", Owner: "dave", Node: "m602", Phase: "Running", CPURequest: 4, CPULimit: 7.6},
		},
		Quotas: map[string]Quota{QuotaKey("b", "dave"): {CPU: 8}},
	}
	snap := s.Snapshot(in)
	if len(snap.Nodes) != 2 || snap.Nodes[0].Cluster != "a" || snap.Nodes[0].CPUAllocated != 2 || snap.Nodes[1].Cluster != "b" || snap.Nodes[1].CPUAllocated != 4 {
		t.Fatalf("nodes %+v", snap.Nodes)
	}
	if snap.KPI.CPUAllocatable != 72 || snap.KPI.Running != 2 {
		t.Fatalf("the console's kpi %+v", snap.KPI)
	}
	if b := snap.Clusters["b"]; b.KPI.CPUAllocatable != 8 || b.KPI.Running != 1 || len(b.Users) != 1 || b.Users[0].Running != 1 {
		t.Fatalf("b's own numbers %+v", b)
	}
	if len(snap.Attention) != 1 || snap.Attention[0].Kind != "quota_high" || snap.Attention[0].Cluster != "b" {
		t.Fatalf("b's quota, judged in b: %+v", snap.Attention)
	}
}

func TestSnapshot_JSONFieldNames(t *testing.T) {
	b, _ := json.Marshal(DevPodLive{})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// UI spec §6.1 plus idle (the §5.1 "空转 >24h" filter)
	want := "cpu,cpuLimit,gpu,idle,ioR,ioW,mem,memLimit,netKind,netRx,netTx,rdmaKind,rdmaRx,rdmaTx,sessions"
	if strings.Join(keys, ",") != want {
		t.Fatalf("DevPodLive keys %v, want UI spec §6.1 %s", keys, want)
	}
}

func TestState_ProcessesMergeFullAndLive(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewState(clock(&now))
	s.ResolveUser = func(uid, peer string) string { return map[string]string{"10.0.0.9:1": "carol"}[peer] }
	s.Apply("u", []wire.Record{
		{K: wire.KFullProcs, Full: &wire.FullProcs{Procs: []wire.SnapProc{
			{Pid: 5, Start: 1, Exe: "/usr/bin/python3", Cmdline: "python train.py", Attr: &wire.Attr{Peer: "10.0.0.9:1", How: "tree"}},
			{Pid: 6, Start: 2, Exe: "/bin/sleep", Cmdline: "sleep 9"},
		}}},
		{K: wire.KProc, P: &wire.Proc{Pid: 5, Start: 1, CPU: 3.2, RSS: 100}},
	})
	all := s.Processes("u", true)
	if len(all) != 2 || all[0].Pid != 5 || all[0].CPU != 3.2 || all[0].User != "carol" || all[0].Cmdline != "python train.py" {
		t.Fatalf("%+v", all)
	}
	if top := s.Processes("u", false); len(top) != 2 {
		t.Fatalf("top %+v", top)
	}
}

func TestState_StaleProcessesAndRetain(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewState(clock(&now))
	s.Apply("u", []wire.Record{container(1, 1), {K: wire.KFullProcs, Full: &wire.FullProcs{Procs: []wire.SnapProc{{Pid: 5, Start: 1}}}}})
	if len(s.Processes("u", true)) != 1 {
		t.Fatal("fresh processes missing")
	}
	now = now.Add(11 * time.Second)
	if got := s.Processes("u", true); len(got) != 0 {
		t.Fatalf("a silent agent's processes must not be shown: %+v", got)
	}
	s.Retain(func(uid string) bool { return uid != "u" })
	if _, ok := s.Sample("u"); ok || len(s.pods) != 0 {
		t.Fatal("Retain kept a deleted DevPod")
	}
}

// A container restart (new boot) drops the old boot's sessions: their
// close events were never sent (review #10).
func TestState_NewBootDropsSessions(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewState(clock(&now))
	s.ResolveUser = func(uid, peer string) string { return "alice" }
	s.SetBoot("u", "b1")
	s.Apply("u", []wire.Record{container(1, 1), {K: wire.KSSHConn, SC: &wire.SSHConn{Pid: 50, Start: 1, Peer: "10.0.0.9:1"}}})
	in := Inputs{DevPods: []DevPodRef{{UID: "u", Name: "d", Owner: "alice", Phase: "Running"}}}
	if got := s.Snapshot(in).DevPods["u"].Sessions; len(got) != 1 {
		t.Fatalf("before restart: %+v", got)
	}
	s.SetBoot("u", "b1") // same boot: kept
	if got := s.Snapshot(in).DevPods["u"].Sessions; len(got) != 1 {
		t.Fatal("same boot dropped the session")
	}
	s.SetBoot("u", "b2")
	s.Apply("u", []wire.Record{container(1, 1)})
	if got := s.Snapshot(in).DevPods["u"].Sessions; len(got) != 0 {
		t.Fatalf("ghost session after restart: %+v", got)
	}
}

func TestSnapshotIdle(t *testing.T) {
	now := time.Unix(100000, 0)
	s := NewState(clock(&now))
	snap := s.Snapshot(Inputs{
		DevPods: []DevPodRef{
			{UID: "a", Name: "a", Owner: "bob", Phase: "Running"},
			{UID: "b", Name: "b", Owner: "bob", Phase: "Running"},
		},
		Idle: map[string]bool{"a": true},
	})
	if !snap.DevPods["a"].Idle || snap.DevPods["b"].Idle {
		t.Fatalf("idle flags: a=%v b=%v", snap.DevPods["a"].Idle, snap.DevPods["b"].Idle)
	}
	b, _ := json.Marshal(snap.DevPods["a"])
	if !strings.Contains(string(b), `"idle":true`) {
		t.Fatalf("json: %s", b)
	}
}
