// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// writeFakeProc writes the files the agent reads for one process.
func writeFakeProc(tb testing.TB, root string, pid, ppid int, start uint64, cpuNs, rssPages uint64) {
	tb.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatal(err)
	}
	files := map[string]string{
		"stat":      fmt.Sprintf("%d (p%d) S %d %d %d 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0\n", pid, pid, ppid, pid, pid, start),
		"schedstat": fmt.Sprintf("%d 0 0\n", cpuNs),
		"statm":     fmt.Sprintf("1000 %d 10 1 0 100 0\n", rssPages),
		"cmdline":   fmt.Sprintf("p%d\x00", pid),
		"status":    "Uid:\t0\t0\t0\t0\n",
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
}

func testEnv(tb testing.TB) (Env, string) {
	tb.Helper()
	proc := tb.TempDir()
	_ = os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	_ = os.WriteFile(filepath.Join(proc, "net", "dev"), []byte("h\nh\n  eth0: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"), 0o644)
	cg := tb.TempDir()
	for n, b := range map[string]string{"cgroup.controllers": "cpu memory", "cpu.stat": "usage_usec 0\n", "cpu.max": "max 100000", "memory.current": "1000"} {
		_ = os.WriteFile(filepath.Join(cg, n), []byte(b), 0o644)
	}
	return Env{ProcRoot: proc, SysRoot: tb.TempDir(), DevRoot: tb.TempDir(), CgroupRoot: cg, FSRoot: tb.TempDir(),
		SSHDPid: func() int { return 10 }, GPU: noGPU{}, TCP: noTCP{}}, proc
}

type noGPU struct{}

func (noGPU) Latest() ([]wire.GPU, bool) { return nil, false }
func (noGPU) Close()                     {}

type noTCP struct{}

func (noTCP) Bytes() (map[uint64]procfs.TCPBytes, error) { return map[uint64]procfs.TCPBytes{}, nil }

type panicGPU struct{}

func (panicGPU) Latest() ([]wire.GPU, bool) { panic("nvml exploded") }
func (panicGPU) Close()                     {}

func TestAgent_OneSecondBatch(t *testing.T) {
	env, proc := testEnv(t)
	writeFakeProc(t, proc, 1, 0, 1, 0, 10)
	writeFakeProc(t, proc, 10, 1, 2, 0, 20)
	writeFakeProc(t, proc, 100, 1, 50, 0, 100) // busy
	writeFakeProc(t, proc, 101, 1, 51, 0, 30)  // idle
	writeFakeProc(t, proc, 102, 1, 52, 0, 40)  // idle
	cfg := config.DefaultConfig()
	cfg.TopN, cfg.DevPodUID = 1, "uid-1"
	a := newAgent(cfg, env)
	t0 := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		writeFakeProc(t, proc, 100, 1, 50, uint64(i)*500_000_000, 100) // +0.5 core/s
		a.scanProcs(now)
		a.collect(now)
	}
	from, to, recs := a.buf.Pending(1 << 30)
	if from != 1 || to != 3 {
		t.Fatalf("seqs %d..%d", from, to)
	}
	containers, other := 0, 0
	for _, r := range recs {
		switch r.K {
		case wire.KContainer:
			containers++
		case wire.KProc:
			p := r.P
			if p.Pid == 0 {
				other++
				// idle: pid 1, 10, 101, 102 → 10+20+30+40 pages
				if p.CPU != 0 || p.RSS != uint64(100*os.Getpagesize()) {
					t.Fatalf("other row: %+v", p)
				}
			} else if p.Pid != 100 {
				t.Fatalf("only active (or top-N) processes are reported, got pid %d", p.Pid)
			}
		}
	}
	if containers != 3 || other < 2 {
		t.Fatalf("containers=%d other=%d", containers, other)
	}
}

func TestAgent_CollectorPanicDisabled(t *testing.T) {
	env, proc := testEnv(t)
	writeFakeProc(t, proc, 1, 0, 1, 0, 10)
	env.GPU = panicGPU{}
	a := newAgent(config.DefaultConfig(), env)
	t0 := time.Unix(1000, 0)
	for i := 0; i < 4; i++ {
		a.scanProcs(t0.Add(time.Duration(i) * time.Second))
		a.collect(t0.Add(time.Duration(i) * time.Second))
	}
	info := a.info(t0)
	if !strings.HasPrefix(info.Collectors["gpu"], "disabled:") || info.Collectors["cgroup"] != "ok" {
		t.Fatalf("collectors %+v", info.Collectors)
	}
	_, _, recs := a.buf.Pending(1 << 30)
	n := 0
	for _, r := range recs {
		if r.K == wire.KContainer {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("container samples must keep flowing, got %d", n)
	}
}

func BenchmarkAgent_300Processes(b *testing.B) {
	env, proc := testEnv(b)
	writeFakeProc(b, proc, 1, 0, 1, 0, 10)
	for i := 0; i < 300; i++ {
		writeFakeProc(b, proc, 1000+i, 1, uint64(100+i), uint64(i), 100)
	}
	a := newAgent(config.DefaultConfig(), env)
	now := time.Unix(1000, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now = now.Add(time.Second)
		a.scanProcs(now)
		a.collect(now)
		a.buf.Ack(a.seq)
	}
}
