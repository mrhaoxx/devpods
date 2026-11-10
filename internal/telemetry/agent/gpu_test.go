// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
)

func TestNvidiaCSV_Parse(t *testing.T) {
	g, err := parseNvidiaCSV("0, GPU-ab, NVIDIA A100-SXM4-80GB, 97, 37000, 81920, 250.3, 61, 1410")
	if err != nil {
		t.Fatal(err)
	}
	if g.Index != 0 || g.UUID != "GPU-ab" || g.Name != "NVIDIA A100-SXM4-80GB" || g.Util != 97 || g.MemUsed != 37000 ||
		g.MemTotal != 81920 || g.Power != 250.3 || g.Temp != 61 || g.SMClock != 1410 {
		t.Fatalf("%+v", g)
	}
	if _, err := parseNvidiaCSV("garbage"); err == nil {
		t.Fatal("short line must fail")
	}
}

func TestNvidiaCSV_NotAvailableFields(t *testing.T) {
	g, err := parseNvidiaCSV("1, MIG-xy, NVIDIA A100 MIG 1g.10gb, [N/A], 100, 9984, [N/A], N/A, [Not Supported]")
	if err != nil {
		t.Fatal(err)
	}
	if g.Util != -1 || g.Power != -1 || g.Temp != -1 || g.SMClock != -1 || g.MemUsed != 100 {
		t.Fatalf("N/A → -1: %+v", g)
	}
}

func TestNvidiaSMI_RestartsOnExit(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := filepath.Join(dir, "nvidia-smi")
	body := "#!/bin/sh\necho start >> " + count + "\necho '0, GPU-a, A100, 50, 1, 2, 3, 4, 5'\necho '1, GPU-b, A100, 60, 1, 2, 3, 4, 5'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newNvidiaSMI(ctx, script, 100*time.Millisecond)
	defer c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(count)
		if strings.Count(string(b), "start") >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(count)
	if strings.Count(string(b), "start") < 2 {
		t.Fatalf("nvidia-smi not restarted: %q", b)
	}
	gpus, ok := c.Latest()
	if !ok || len(gpus) != 2 || gpus[0].UUID != "GPU-a" || gpus[1].Util != 60 {
		t.Fatalf("latest: %+v %v", gpus, ok)
	}
}

// The collector reports the pid of the nvidia-smi it runs, so the agent
// can count it as infrastructure (it is PID 1's child in-process).
func TestNvidiaSMI_ReportsItsChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := filepath.Join(dir, "nvidia-smi")
	body := "#!/bin/sh\necho $$ > " + pidFile + ".tmp && mv " + pidFile + ".tmp " + pidFile + "\nexec sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newNvidiaSMI(ctx, script, time.Second).(*nvidiaSMI)
	deadline := time.Now().Add(3 * time.Second)
	var want int
	for time.Now().Before(deadline) && (want == 0 || c.ChildPid() != want) {
		if b, err := os.ReadFile(pidFile); err == nil {
			want, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if want == 0 || c.ChildPid() != want {
		t.Fatalf("child pid %d, want %d", c.ChildPid(), want)
	}
	c.Close()
	for deadline = time.Now().Add(3 * time.Second); time.Now().Before(deadline) && c.ChildPid() != 0; {
		time.Sleep(20 * time.Millisecond)
	}
	if c.ChildPid() != 0 {
		t.Fatalf("child pid %d after Close", c.ChildPid())
	}
}

func TestNvidiaSMI_Missing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newNvidiaSMI(ctx, filepath.Join(t.TempDir(), "nope"), time.Millisecond)
	defer c.Close()
	time.Sleep(50 * time.Millisecond)
	if g, ok := c.Latest(); ok || g != nil {
		t.Fatalf("missing nvidia-smi → N/A, got %+v", g)
	}
	if n := c.(*nvidiaSMI).starts.Load(); n > 1 {
		t.Fatalf("missing binary must not be retried in a busy loop: %d attempts", n)
	}
}

func TestProcGPUs_FdScan(t *testing.T) {
	root := t.TempDir()
	for fd, target := range map[string]string{"3": "/dev/nvidia0", "4": "/dev/nvidia1", "5": "/dev/nvidiactl", "6": "/dev/nvidia-uvm", "7": "/dev/nvidia0"} {
		dir := filepath.Join(root, "4", "fd")
		_ = os.MkdirAll(dir, 0o755)
		if err := os.Symlink(target, filepath.Join(dir, fd)); err != nil {
			t.Fatal(err)
		}
	}
	procs := map[procfs.PID]*procfs.Proc{{Pid: 4, Start: 9}: {ID: procfs.PID{Pid: 4, Start: 9}}}
	got := ProcGPUs(root, procs)
	if g := got[procfs.PID{Pid: 4, Start: 9}]; len(g) != 2 || g[0] != 0 || g[1] != 1 {
		t.Fatalf("%v", got)
	}
}
