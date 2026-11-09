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
