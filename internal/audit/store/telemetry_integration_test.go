// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTelemetry_InsertAndReplayCollapses(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	b := TelemetryBatch{
		Containers: []ContainerMetricRow{{TS: base, DevPodUID: "u1", BootID: "b", CPU: 1.5, MemWS: 1 << 30, NetKind: "pod"}},
		GPUs:       []GPUMetricRow{{TS: base, DevPodUID: "u1", GPUUUID: "GPU-a", Util: 97}},
		RDMAs:      []RDMAMetricRow{{TS: base, DevPodUID: "u1", IBDev: "mlx5_0", Port: 1, TxBps: 3e9, Health: map[string]uint64{"np_cnp_sent": 3}, Scope: "vf_exclusive"}},
		Procs:      []ProcMetricRow{{TS: base, DevPodUID: "u1", BootID: "b", Pid: 7, StartTime: 9, CPU: 0.5, GPU: []uint8{0}}},
	}
	for _, tok := range []string{"t1", "t2"} { // a replay lands with a different token
		if err := c.InsertTelemetry(ctx, b, tok); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"metrics_container_1s", "metrics_gpu_1s", "metrics_rdma_1s", "metrics_process_1s"} {
		var n uint64
		if err := c.conn.QueryRow(ctx, fmt.Sprintf("SELECT count() FROM %s.%s FINAL", c.db, table)).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: %d rows after replay (%v)", table, n, err)
		}
	}
}

func TestTelemetry_OneMinuteRollup(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	var b TelemetryBatch
	for i := 0; i < 60; i++ {
		b.Containers = append(b.Containers, ContainerMetricRow{TS: base.Add(time.Duration(i) * time.Second), DevPodUID: "u1", BootID: "b", CPU: float32(i + 1)})
	}
	if err := c.InsertTelemetry(ctx, b, "roll"); err != nil {
		t.Fatal(err)
	}
	var avg float64
	var mx float32
	if err := c.conn.QueryRow(ctx, fmt.Sprintf("SELECT avgMerge(cpu_avg), maxMerge(cpu_max) FROM %s.metrics_container_1m WHERE devpod_uid = 'u1'", c.db)).Scan(&avg, &mx); err != nil {
		t.Fatal(err)
	}
	if avg != 30.5 || mx != 60 {
		t.Fatalf("avg %v max %v", avg, mx)
	}
}

func TestMigrate_TelemetryTTLs(t *testing.T) {
	ctx := context.Background()
	db := fmt.Sprintf("ttl_%d", time.Now().UnixNano())
	c := openTestCH(t, db)
	t.Cleanup(func() { _ = c.conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db) })
	r := DefaultRetention(30)
	r.Metrics1sDays = 3
	if err := Migrate(ctx, c, r); err != nil {
		t.Fatal(err)
	}
	ddl := func(table string) string {
		var q string
		if err := c.conn.QueryRow(ctx, "SELECT create_table_query FROM system.tables WHERE database = ? AND name = ?", db, table).Scan(&q); err != nil {
			t.Fatal(err)
		}
		return q
	}
	if q := ddl("metrics_container_1s"); !strings.Contains(q, "toIntervalDay(3)") {
		t.Fatalf("1s TTL: %s", q)
	}
	if q := ddl("metrics_process_1s"); !strings.Contains(q, "toIntervalHour(48)") {
		t.Fatalf("process TTL: %s", q)
	}
	if q := ddl("metrics_container_1m"); !strings.Contains(q, "toIntervalDay(90)") {
		t.Fatalf("1m TTL: %s", q)
	}
	if q := ddl("processes"); !strings.Contains(q, "toIntervalDay(30)") {
		t.Fatalf("container-side events follow the audit retention: %s", q)
	}
}

// minuteRows writes `minutes` 1-minute container rollups ending at end.
func minuteRows(t *testing.T, c *CH, uid string, end time.Time, minutes int, cpu, limit float32) {
	t.Helper()
	q := fmt.Sprintf(`INSERT INTO %s.metrics_container_1m (ts, devpod_uid, cpu_avg, cpu_max, cpu_p95, cpu_limit)
		SELECT toStartOfMinute(toDateTime(?) - toIntervalMinute(number)), ?, avgState(toFloat32(?)), maxState(toFloat32(?)),
		       quantileState(0.95)(toFloat32(?)), maxState(toFloat32(?))
		FROM numbers(?) GROUP BY number`, c.db)
	if err := c.conn.Exec(context.Background(), q, end, uid, cpu, cpu, cpu, limit, minutes); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsRange_PicksTable(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	var b TelemetryBatch
	for i := 0; i < 60; i++ {
		b.Containers = append(b.Containers, ContainerMetricRow{TS: now.Add(-10*time.Minute + time.Duration(i)*time.Second), DevPodUID: "u1", BootID: "b", CPU: 2, CPULimit: 4})
	}
	b.GPUs = []GPUMetricRow{{TS: now.Add(-10 * time.Minute), DevPodUID: "u1", GPUUUID: "GPU-a", Util: 90}}
	if err := c.InsertTelemetry(ctx, b, "r"); err != nil {
		t.Fatal(err)
	}
	minuteRows(t, c, "u1", now.Add(-2*time.Hour), 1, 7, 4) // only in the 1m table
	s, err := c.MetricsRange(ctx, "u1", now.Add(-30*time.Minute), now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasValue(s.Series["cpu"], 2) || hasValue(s.Series["cpu"], 7) || !hasValue(s.Series["gpu.GPU-a.util"], 90) {
		t.Fatalf("30 min range must read the 1s tables: %+v", s.Series)
	}
	s, err = c.MetricsRange(ctx, "u1", now.Add(-3*time.Hour), now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasValue(s.Series["cpu"], 7) || s.Step < 60 {
		t.Fatalf("3 h range must read the 1m tables (step %d): %+v", s.Step, s.Series["cpu"])
	}
}

func hasValue(xs []*float64, v float64) bool {
	for _, x := range xs {
		if x != nil && *x == v {
			return true
		}
	}
	return false
}

func TestIdleCandidates(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	minuteRows(t, c, "A", now, 1440, 0.04, 4) // 1 % of 4 cores, all day
	minuteRows(t, c, "B", now, 1440, 1.6, 4)  // 40 %
	minuteRows(t, c, "C", now, 1440, 0.04, 4) // idle CPU but used over SSH 2 h ago
	minuteRows(t, c, "D", now, 120, 0.04, 4)  // idle, but only up for 2 h
	sess := sess("S-C", "carol", "C", now.Add(-2*time.Hour), 1, true)
	mustInsert(t, c, Batch{Sessions: []SessionRow{sess}}, "s")
	got, err := c.IdleCandidates(ctx, now, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[A]" {
		t.Fatalf("idle %v", got)
	}
}

func TestHookMissing(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	mk := func(id string, dur time.Duration) {
		s := sess(id, "alice", "u1", now.Add(-dur), 2, false)
		closed := now
		s.ClosedAt = &closed
		ch := ChannelRow{SessionID: id, ChannelID: 1, Type: "session", Pty: true, OpenedAt: now.Add(-dur), BytesIn: 2048, Version: 2}
		mustInsert(t, c, Batch{Sessions: []SessionRow{s}, Channels: []ChannelRow{ch}}, id)
	}
	mk("S-long", 90*time.Second)
	mk("S-hooked", 90*time.Second)
	mk("S-short", 30*time.Second)
	mustInsert(t, c, Batch{Commands: []CommandRow{{EventID: "h1", TS: now.Add(-time.Minute), SessionID: "S-hooked", DevPodUID: "u1", Source: "shell_hook", Cmdline: "ls", Version: 1}}}, "cmd")
	got, err := c.HookMissing(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "S-long" {
		t.Fatalf("hook_missing %+v", got)
	}
}

func TestBackfillQueries(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	shellAt := now.Add(-10 * time.Minute)
	s := sess("S1", "alice", "u1", now.Add(-20*time.Minute), 1, false)
	s.BackendLocalAddr = "10.0.0.9:41000"
	mustInsert(t, c, Batch{Sessions: []SessionRow{s}}, "s")
	exit := int32(0)
	b := TelemetryBatch{
		SSHConns: []SSHConnRow{{DevPodUID: "u1", BootID: "b", Pid: 50, StartTime: 100, PeerAddr: "10.0.0.9:41000", FirstSeen: shellAt, SessionID: "pending", Version: 1}},
		ProcRows: []ProcessRow{
			{DevPodUID: "u1", BootID: "b", Pid: 60, StartTime: 110, Exe: "/bin/bash", FirstSeen: shellAt, SessionID: "pending", Peer: "10.0.0.9:41000", Attribution: "tree", Version: 1},
			{DevPodUID: "u1", BootID: "b", Pid: 61, StartTime: 120, Exe: "/bin/ls", FirstSeen: shellAt, SessionID: "S0", Peer: "10.0.0.8:1", Attribution: "tree", Version: 1},
			{DevPodUID: "u2", BootID: "b", Pid: 70, StartTime: 130, Exe: "/bin/x", FirstSeen: shellAt, Attribution: "unattributed", Version: 1},
			{DevPodUID: "u2", BootID: "b", Pid: 71, StartTime: 131, Exe: "/bin/y", FirstSeen: shellAt, Attribution: "unattributed", Version: 1},
			{DevPodUID: "u2", BootID: "b", Pid: 72, StartTime: 132, Exe: "/bin/z", FirstSeen: shellAt, Attribution: "workload", Version: 1},
		},
		Commands: []CommandRow{{EventID: "e1", TS: shellAt.Add(time.Minute), SessionID: "pending", DevPodUID: "u1", Source: "shell_hook", PID: 60, Cmdline: "make", ExitCode: &exit, Attribution: "pending", Version: 1}},
		Infos: []ContainerInfoRow{{TS: shellAt, DevPodUID: "u1", BootID: "b", Info: "{}", Version: 1},
			{TS: shellAt, DevPodUID: "u2", BootID: "b", Info: `{"collectors":{"hooks":"ok","cgroup":"ok"}}`, Version: 1},
			{TS: shellAt, DevPodUID: "u3", BootID: "b", Info: `{"collectors":{"hooks":"disabled: config"}}`, Version: 1}},
	}
	if err := c.InsertTelemetry(ctx, b, "t"); err != nil {
		t.Fatal(err)
	}
	conns, err := c.PendingAttributions(ctx, now.Add(-time.Hour))
	if err != nil || len(conns) != 1 || conns[0].Pid != 50 {
		t.Fatalf("pending conns %+v %v", conns, err)
	}
	procs, err := c.PendingProcesses(ctx, now.Add(-time.Hour))
	if err != nil || len(procs) != 1 || procs[0].Pid != 60 {
		t.Fatalf("pending processes %+v %v", procs, err)
	}
	cmds, err := c.PendingCommands(ctx, now.Add(-time.Hour), now.Add(-7*24*time.Hour))
	if err != nil || len(cmds) != 1 || cmds[0].EventID != "e1" || cmds[0].Peer != "10.0.0.9:41000" || cmds[0].ProcAttribution != "tree" {
		t.Fatalf("pending commands %+v %v", cmds, err)
	}
	sessions, err := c.BackendSessions(ctx, now.Add(-24*time.Hour))
	if err != nil || len(sessions) != 1 || sessions[0].Addr != "10.0.0.9:41000" || sessions[0].SessionID != "S1" || sessions[0].UID != "u1" {
		t.Fatalf("backend sessions %+v %v", sessions, err)
	}
	seen, err := c.InfoSeen(ctx, now.Add(-time.Hour))
	if err != nil || !seen["u1"].Equal(shellAt) {
		t.Fatalf("info seen %v %v", seen, err)
	}
	hooks, err := c.HooksSince(ctx, now.Add(-time.Hour))
	if err != nil || len(hooks) != 1 || !hooks["u2"].Equal(shellAt) {
		t.Fatalf("hooks since %v %v (only u2 runs hooks)", hooks, err)
	}
	un, err := c.UnattributedCounts(ctx, shellAt.Add(-time.Second), shellAt.Add(time.Second))
	if err != nil || len(un) != 1 || un["u2"] != 2 {
		t.Fatalf("unattributed %v %v", un, err)
	}
}

// throttled_usec is a cumulative counter: the series is its rate, at the
// 1 s resolution and from the 1-minute rollups (review #5).
func TestMetricsRange_ThrottledIsARate(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	end := time.Now().UTC().Truncate(time.Minute)
	start := end.Add(-3 * time.Minute)
	var b TelemetryBatch
	for i := 0; i < 180; i++ { // +1000 µs throttled every second
		b.Containers = append(b.Containers, ContainerMetricRow{TS: start.Add(time.Duration(i) * time.Second), DevPodUID: "u1", BootID: "b",
			CPU: 1, ThrottledUsec: uint64(5_000_000 + 1000*i)})
	}
	if err := c.InsertTelemetry(ctx, b, "thr"); err != nil {
		t.Fatal(err)
	}
	check := func(name string, s *MetricSeries) {
		t.Helper()
		got, first := 0, true
		for i, v := range s.Series["cpu_throttled"] {
			if v == nil || *v == 0 {
				continue
			}
			// where the data itself begins the first sample has no
			// predecessor: that bucket holds one delta less
			if first && s.TS[i] <= start.Unix() {
				first = false
				continue
			}
			first = false
			if *v < 900 || *v > 1100 {
				t.Fatalf("%s: throttled %.0f µs/s, want ~1000: %v", name, *v, s.Series["cpu_throttled"])
			}
			got++
		}
		if got == 0 {
			t.Fatalf("%s: no throttling at all", name)
		}
	}
	s, err := c.MetricsRange(ctx, "u1", end.Add(-10*time.Minute), end, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	check("1 s", s)
	s, err = c.MetricsRange(ctx, "u1", end.Add(-10*time.Minute), end, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	check("5 s", s)
	// a window starting inside the data: its first bucket is complete
	s, err = c.MetricsRange(ctx, "u1", start.Add(30*time.Second), end, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if v := s.Series["cpu_throttled"][0]; v == nil || *v < 900 {
		t.Fatalf("first bucket of a window inside the data: %v", s.Series["cpu_throttled"][:3])
	}
	s, err = c.MetricsRange(ctx, "u1", end.Add(-3*time.Hour), end, 0)
	if err != nil {
		t.Fatal(err)
	}
	check("1 m rollup", s)
}

func TestRecentContainer(t *testing.T) {
	c := freshDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	var b TelemetryBatch
	for i := 0; i < 30; i++ {
		b.Containers = append(b.Containers, ContainerMetricRow{TS: now.Add(-10*time.Minute + time.Duration(i)*time.Second), DevPodUID: "u1", BootID: "b", CPU: 2, MemWS: 1 << 30})
	}
	b.Containers = append(b.Containers,
		ContainerMetricRow{TS: now.Add(-5 * time.Minute), DevPodUID: "u2", BootID: "b", CPU: 4, MemWS: 2 << 30},
		ContainerMetricRow{TS: now.Add(-30 * time.Minute), DevPodUID: "u3", BootID: "b", CPU: 8},    // outside the window
		ContainerMetricRow{TS: now.Add(-1 * time.Minute), DevPodUID: "other", BootID: "b", CPU: 16}, // not asked for
	)
	if err := c.InsertTelemetry(ctx, b, "r"); err != nil {
		t.Fatal(err)
	}
	r, err := c.RecentContainer(ctx, []string{"u1", "u2", "u3"}, now.Add(-15*time.Minute), now, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r.Step != 15 || len(r.TS) < 60 || len(r.TS) > 62 {
		t.Fatalf("step %d, %d buckets", r.Step, len(r.TS))
	}
	if _, ok := r.Items["u3"]; ok {
		t.Fatalf("u3 has no rows in the window: %+v", r.Items["u3"])
	}
	if _, ok := r.Items["other"]; ok {
		t.Fatal("uids not asked for must not appear")
	}
	if !hasValue(r.Items["u1"].CPU, 2) || !hasValue(r.Items["u1"].Mem, 1<<30) || !hasValue(r.Items["u2"].CPU, 4) {
		t.Fatalf("items: %+v", r.Items)
	}
	if len(r.Items["u1"].CPU) != len(r.TS) || r.Items["u1"].CPU[len(r.TS)-1] != nil {
		t.Fatal("series align with ts; buckets without samples are null")
	}
	if r, err := c.RecentContainer(ctx, nil, now.Add(-15*time.Minute), now, 15*time.Second); err != nil || len(r.Items) != 0 {
		t.Fatalf("no uids: %v %+v", err, r)
	}
}
