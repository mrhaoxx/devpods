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
		// utime (clock ticks, 100 Hz) and rss (pages) come from stat
		"stat": fmt.Sprintf("%d (p%d) S %d %d %d 0 -1 0 0 0 0 0 %d 0 0 0 20 0 1 0 %d 0 %d\n",
			pid, pid, ppid, pid, pid, cpuNs/10_000_000, start, rssPages),
		"cmdline": fmt.Sprintf("p%d\x00", pid),
		"status":  "Uid:\t0\t0\t0\t0\n",
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

type countingTCP struct {
	calls int
	delay time.Duration
}

func (c *countingTCP) Bytes() (map[uint64]procfs.TCPBytes, error) {
	c.calls++
	time.Sleep(c.delay)
	return map[uint64]procfs.TCPBytes{}, nil
}

// A sock_diag dump walks the node's whole TCP hash table: the agent
// spaces dumps so they cost at most 0.1 % of a core.
func TestAgent_TCPSampledLessOftenWhenSlow(t *testing.T) {
	for _, tc := range []struct {
		delay time.Duration
		want  int
	}{{0, 6}, {10 * time.Millisecond, 1}} { // 10 ms → at most 0.1 % of a core → every 10 s
		env, proc := testEnv(t)
		writeFakeProc(t, proc, 1, 0, 1, 0, 10)
		tcp := &countingTCP{delay: tc.delay}
		env.TCP = tcp
		a := newAgent(config.DefaultConfig(), env)
		t0 := time.Unix(1000, 0)
		for i := 0; i < 6; i++ {
			a.collect(t0.Add(time.Duration(i) * time.Second))
		}
		if tcp.calls != tc.want {
			t.Fatalf("dump taking %v: %d dumps in 6 s, want %d", tc.delay, tcp.calls, tc.want)
		}
	}
}

// Per-process rates divide by the time between the two readings, not by
// the collect interval: a process re-read after 5 s shows its average.
func TestAgent_ProcessRateUsesReadTimes(t *testing.T) {
	env, proc := testEnv(t)
	writeFakeProc(t, proc, 1, 0, 1, 0, 10)
	writeFakeProc(t, proc, 100, 1, 50, 0, 10)
	a := newAgent(config.DefaultConfig(), env)
	t0 := time.Unix(1000, 0)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }
	for i := 0; i < 8; i++ { // idle long enough to be read every 5th scan
		a.collect(at(i))
	}
	writeFakeProc(t, proc, 100, 1, 50, 2_000_000_000, 10) // 2 s of CPU since its last reading
	var cpu float64
	_, to, _ := a.buf.Pending(1 << 30)
	a.buf.Ack(to)
	for i := 8; i < 14; i++ {
		a.collect(at(i))
		_, to, recs := a.buf.Pending(1 << 30)
		a.buf.Ack(to)
		for _, r := range recs {
			if r.K == wire.KProc && r.P.Pid == 100 && r.P.CPU > 0 {
				cpu = r.P.CPU
			}
		}
		if cpu > 0 {
			break
		}
	}
	if cpu <= 0 || cpu > 1.01 {
		t.Fatalf("woken process shows %.2f cores; 2 s of CPU over ≥ 2 s between readings must not exceed 1 core", cpu)
	}
}

func TestCostGate_SpacesExpensiveReads(t *testing.T) {
	g := costGate{min: time.Second, max: 30 * time.Second}
	t0 := time.Unix(1000, 0)
	if !g.due(t0) {
		t.Fatal("first read is due")
	}
	g.done(t0, 18*time.Millisecond) // 18 ms → next after 18 s
	if g.due(t0.Add(17*time.Second)) || !g.due(t0.Add(18*time.Second)) {
		t.Fatal("an 18 ms read must wait 18 s")
	}
	g.done(t0, 100*time.Microsecond)
	if !g.due(t0.Add(time.Second)) {
		t.Fatal("a cheap read is due every min")
	}
	g.done(t0, time.Second)
	if !g.due(t0.Add(30 * time.Second)) {
		t.Fatal("capped at max")
	}
}

// session writes an sshd-session (pid, under sshd 10) holding socket
// inode with an established peer.
func session(t *testing.T, proc string, pid int, inode uint64, peerHex string) {
	t.Helper()
	writeFakeProc(t, proc, pid, 10, uint64(pid), 0, 10)
	dir := filepath.Join(proc, fmt.Sprint(pid))
	if err := os.Symlink(SessionExe, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "fd"), 0o755)
	if err := os.Symlink(fmt.Sprintf("socket:[%d]", inode), filepath.Join(dir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "net"), 0o755)
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		fmt.Sprintf("   0: 0A00000B:08AE %s 01 00000000:00000000 00:00000000 00000000     0        0 %d 1 0000000000000000 20 4 30 10 -1\n", peerHex, inode)
	if err := os.WriteFile(filepath.Join(dir, "net", "tcp"), []byte(tcp), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A session's peer never changes: it is read once (each read of
// /proc/<pid>/net/tcp walks the node's whole TCP hash table).
func TestAgent_SessionPeerResolvedOnce(t *testing.T) {
	env, proc := testEnv(t)
	writeFakeProc(t, proc, 1, 0, 1, 0, 10)
	writeFakeProc(t, proc, 10, 1, 2, 0, 10)
	session(t, proc, 50, 777, "0900000A:A028") // 10.0.0.9:41000
	a := newAgent(config.DefaultConfig(), env)
	var calls [][]procfs.PID
	a.sshConns = func(root string, sm *procfs.SocketMap, sessions []procfs.PID) map[procfs.PID]string {
		calls = append(calls, sessions)
		return procfs.SSHConns(root, sm, sessions)
	}
	t0 := time.Unix(1000, 0)
	step := func(from, to int) {
		for i := from; i < to; i++ {
			now := t0.Add(time.Duration(i) * 500 * time.Millisecond)
			if i%2 == 0 {
				a.collect(now)
			} else {
				a.scanProcs(now)
			}
		}
	}
	step(0, 24) // 12 s
	if a.reportedConns[procfs.PID{Pid: 50, Start: 50}] != "10.0.0.9:41000" {
		t.Fatalf("session not resolved: %v", a.reportedConns)
	}
	if len(calls) != 1 {
		t.Fatalf("resolved %d times in 12 s, want once", len(calls))
	}
	session(t, proc, 60, 778, "0A00000A:A029")
	step(24, 40)
	if len(calls) != 2 || len(calls[1]) != 1 || calls[1][0].Pid != 60 {
		t.Fatalf("a new session is resolved alone: %v", calls)
	}
}

// A process that did not run since its last fd walk cannot have opened
// or closed anything: its fds are not walked again.
func TestAgent_FDWalkOnlyForProcessesThatRan(t *testing.T) {
	env, proc := testEnv(t)
	writeFakeProc(t, proc, 1, 0, 1, 0, 10)
	writeFakeProc(t, proc, 100, 1, 100, 0, 10) // sleeps
	writeFakeProc(t, proc, 101, 1, 101, 0, 10) // works
	a := newAgent(config.DefaultConfig(), env)
	walks := map[int]int{}
	a.walkFDs = func(root string, id procfs.PID, visit func(procfs.PID, string)) {
		walks[id.Pid]++
		procfs.WalkProcFDs(root, id, visit)
	}
	t0 := time.Unix(1000, 0)
	for i := 0; i < 16; i++ { // 16 s: fd map refreshed every 5 s
		writeFakeProc(t, proc, 101, 1, 101, uint64(i+1)*100_000_000, 10)
		a.collect(t0.Add(time.Duration(i) * time.Second))
	}
	if walks[100] != 1 || walks[101] < 3 {
		t.Fatalf("walks %v: the sleeping process once, the working one every refresh", walks)
	}
}

// Light scans fall between full scans (one per collect), not next to one.
func TestAgent_LightScanBetweenFullScans(t *testing.T) {
	env, proc := testEnv(t)
	writeFakeProc(t, proc, 1, 0, 1, 0, 10)
	a := newAgent(config.DefaultConfig(), env)
	t0 := time.Unix(1000, 0)
	a.collect(t0)
	for _, tc := range []struct {
		at   time.Duration
		runs bool
	}{{100 * time.Millisecond, false}, {500 * time.Millisecond, true}, {900 * time.Millisecond, false}} {
		before := a.scanNow
		a.scanProcs(t0.Add(tc.at))
		if ran := a.scanNow != before; ran != tc.runs {
			t.Errorf("light scan at +%v ran=%v, want %v", tc.at, ran, tc.runs)
		}
	}
}

// The leader alerts hook_missing only where hooks run: the agent says so.
func TestAgent_ReportsHooksCollector(t *testing.T) {
	for _, on := range []bool{true, false} {
		env, _ := testEnv(t)
		cfg := config.DefaultConfig()
		cfg.Hooks = on
		got := newAgent(cfg, env).collectorStatus()["hooks"]
		if want := map[bool]string{true: "ok", false: "disabled: config"}[on]; got != want {
			t.Fatalf("hooks=%v: collector status %q, want %q", on, got, want)
		}
	}
}

// Processes reported before their session's peer was known are re-issued
// once it is, so their rows can be linked to the session (review #9).
func TestAgent_StartRowsReissuedWhenPeerResolves(t *testing.T) {
	env, proc := testEnv(t)
	writeFakeProc(t, proc, 1, 0, 1, 0, 10)
	writeFakeProc(t, proc, 10, 1, 2, 0, 10)
	session(t, proc, 50, 777, "0900000A:A028") // 10.0.0.9:41000
	writeFakeProc(t, proc, 60, 50, 60, 0, 10)  // the login shell
	a := newAgent(config.DefaultConfig(), env)
	resolve := false
	a.sshConns = func(root string, sm *procfs.SocketMap, sessions []procfs.PID) map[procfs.PID]string {
		if !resolve {
			return map[procfs.PID]string{}
		}
		return procfs.SSHConns(root, sm, sessions)
	}
	t0 := time.Unix(1000, 0)
	a.collect(t0)
	resolve = true
	for i := 1; i < 4; i++ {
		a.collect(t0.Add(time.Duration(i) * time.Second))
	}
	var starts []*wire.ProcEvent
	_, _, recs := a.buf.Pending(1 << 30)
	for _, r := range recs {
		if r.K == wire.KProcEvent && r.PE.Event == "process_start" && r.PE.Pid == 60 {
			starts = append(starts, r.PE)
		}
	}
	if len(starts) != 2 || starts[0].Attr.Peer != "" || starts[1].Attr.Peer != "10.0.0.9:41000" ||
		!starts[1].FirstSeen.Equal(starts[0].FirstSeen) || !starts[1].TS.After(starts[0].TS) {
		for _, s := range starts {
			t.Logf("%+v attr=%+v", *s, *s.Attr)
		}
		t.Fatalf("want the shell's start re-issued with the peer once known")
	}
}
