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
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
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
	c := newNvidiaSMI(ctx, script, time.Second).(*smiCollector)
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
	if n := c.(*smiCollector).starts.Load(); n > 1 {
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

// Moore Threads' mthreads-gmi answers the same query (its SM clock is
// clocks.current.gpu) but cannot leave out the units: "59630 [MiB]".
func TestMthreadsCSV_Parse(t *testing.T) {
	g, err := parseMthreadsCSV("0, 17306b9e-f595-fb6d-1693-0aa0f969e506, MTT S5000, 3 [%], 59630 [MiB], 81920 [MiB], 400.49 [W], 36, 1750 [MHz]")
	if err != nil {
		t.Fatal(err)
	}
	if g.Index != 0 || g.UUID != "17306b9e-f595-fb6d-1693-0aa0f969e506" || g.Name != "MTT S5000" || g.Util != 3 || g.MemUsed != 59630 ||
		g.MemTotal != 81920 || g.Power != 400.49 || g.Temp != 36 || g.SMClock != 1750 {
		t.Fatalf("%+v", g)
	}
	g, err = parseMthreadsCSV("1, u-1, MTT S4000, N/A, 1 [MiB], 2 [MiB], N/A, [N/A], N/A [MHz]")
	if err != nil || g.Util != -1 || g.Power != -1 || g.Temp != -1 || g.SMClock != -1 || g.MemUsed != 1 {
		t.Fatalf("N/A → -1: %+v %v", g, err)
	}
	if _, err := parseMthreadsCSV("garbage"); err == nil {
		t.Fatal("short line must fail")
	}
}

// The collector runs mthreads-gmi with that query, every second.
func TestMthreadsGMI_Runs(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mthreads-gmi")
	body := "#!/bin/sh\necho \"$@\" > " + args + "\necho '0, u-a, MTT S5000, 7 [%], 1 [MiB], 2 [MiB], 3 [W], 4, 5 [MHz]'\nsleep 5\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewMthreadsGMI(ctx, script)
	defer c.Close()
	deadline := time.Now().Add(3 * time.Second)
	var gpus []wire.GPU
	for ok := false; !ok && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		gpus, ok = c.Latest()
	}
	if len(gpus) != 1 || gpus[0].UUID != "u-a" || gpus[0].Util != 7 || gpus[0].SMClock != 5 {
		t.Fatalf("latest: %+v", gpus)
	}
	b, _ := os.ReadFile(args)
	if got := strings.TrimSpace(string(b)); got != "--query-gpu="+mthreadsQuery+" --format=csv,noheader -lms 1000" {
		t.Fatalf("args %q", got)
	}
}

// A card held open: /dev/nvidiaN, or Moore Threads' /dev/mtgpu.N — the
// same N as the tool's index (the same PCI device).
func TestGPUMinor(t *testing.T) {
	for in, want := range map[string]int{"/dev/nvidia2": 2, "/dev/mtgpu.3": 3, "/dev/mtgpu.0": 0} {
		if got, ok := gpuMinor(in); !ok || got != want {
			t.Fatalf("%s: %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/dri/card0", "/dev/mtgpu.", "/dev/null"} {
		if _, ok := gpuMinor(in); ok {
			t.Fatalf("%s counted as a card", in)
		}
	}
}

// Which tool: nvidia-smi where there is one, else mthreads-gmi, else none.
func TestPickGPUTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	have := func(names ...string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			for _, n := range names {
				if n == name {
					return "/x/" + name, true
				}
			}
			return "", false
		}
	}
	for _, c := range []struct {
		have []string
		want string
	}{{[]string{"nvidia-smi", "mthreads-gmi"}, "nvidia-smi"}, {[]string{"mthreads-gmi"}, "mthreads-gmi"}, {[]string{"mx-smi"}, "mx-smi"}, {[]string{"npu-smi"}, "npu-smi"}, {nil, ""}} {
		g := pickGPUCollector(ctx, have(c.have...))
		got := ""
		if s, ok := g.(*smiCollector); ok {
			got = s.tool
		}
		g.Close()
		if got != c.want {
			t.Fatalf("with %v: %q, want %q", c.have, got, c.want)
		}
	}
}

// MetaX's mx-smi: `dmon` lines read by the header's column names (the
// used memory is a percentage of the total), names and UUIDs from -L.
func TestMetaXDmon_Parse(t *testing.T) {
	cards := parseMetaXList("mx-smi  version: 2.3.4\nGPU#0    MXC500      0000:0e:00.0   Available (UUID: GPU-8c6f3104-4551-66af-f489-1b2c27bb1ac9)\nGPU#1    MXC500      0000:0f:00.0   Available (UUID: GPU-bf88dd2f)\n")
	if cards[0].name != "MXC500" || cards[0].uuid != "GPU-8c6f3104-4551-66af-f489-1b2c27bb1ac9" || cards[1].uuid != "GPU-bf88dd2f" {
		t.Fatalf("list %+v", cards)
	}
	p := newMetaXParser(func() map[int]metaxCard { return cards })
	for _, l := range []string{
		"dev, die, gpu, vpue, vpud, visvram, vram, xtt, total, hottemp, drmossoctemp, drmoscoretemp, power, xcoreclk, mcclk, vpueclk, vpudclk",
		"idx, idx, %, %, %, %, %, %, GB, C, C, C, W, MHz, MHz, MHz, MHz",
	} {
		if _, err := p(l); err == nil {
			t.Fatalf("a header line is not a card: %q", l)
		}
	}
	g, err := p("1, 0, 37, 0, 0, 90, 90, 1, 64, 40, 31, 32, 69, 1125, 1800, 535, 525")
	if err != nil {
		t.Fatal(err)
	}
	if g.Index != 1 || g.UUID != "GPU-bf88dd2f" || g.Name != "MXC500" || g.Util != 37 || g.MemTotal != 65536 || g.MemUsed != 58982.4 ||
		g.Power != 69 || g.Temp != 40 || g.SMClock != 1125 {
		t.Fatalf("%+v", g)
	}
	// a card -L did not name; a second die of a card
	if g, err := p("7, 0, 0, 0, 0, 10, 10, 1, 64, 40, 31, 32, 69, 1125, 1800, 535, 525"); err != nil || g.UUID != "metax-7" || g.Name != "MetaX" {
		t.Fatalf("unnamed: %+v %v", g, err)
	}
	if _, err := p("1, 1, 0, 0, 0, 90, 90, 1, 64, 40, 31, 32, 69, 1125, 1800, 535, 525"); err == nil {
		t.Fatal("die 1 counted as a card")
	}
	// before any header: nothing to go by
	if _, err := newMetaXParser(func() map[int]metaxCard { return nil })("0, 0, 1, 0, 0, 1, 1, 1, 64, 1, 1, 1, 1, 1, 1, 1, 1"); err == nil {
		t.Fatal("a line without a header")
	}
}

// The DRM render node of card N (renderD128+N) is card N, as MetaX's
// tools number them.
func TestGPUMinor_RenderNode(t *testing.T) {
	if got, ok := gpuMinor("/dev/dri/renderD130"); !ok || got != 2 {
		t.Fatalf("renderD130: %d %v", got, ok)
	}
	for _, in := range []string{"/dev/dri/card1", "/dev/dri/renderD", "/dev/mxcd"} {
		if _, ok := gpuMinor(in); ok {
			t.Fatalf("%s counted as a card", in)
		}
	}
}

// Ascend's npu-smi: `info watch -s ptmn` lines read by the header's
// names (columns apart by runs of spaces: "NPU Util(%)" is one), the
// cards' names from `info -m` and HBM sizes from `info -t memory`.
func TestAscendWatch_Parse(t *testing.T) {
	names := parseAscendMap("\tNPU ID                         Slot ID                        Chip ID                        Chip Phy-ID                    Chip Name                     \n\t0                              0                              0                              0                              Ascend950DT\n\t1                              1                              0                              1                              Ascend950DT\n")
	if names[0] != "Ascend950DT" || names[1] != "Ascend950DT" || len(names) != 2 {
		t.Fatalf("names %v", names)
	}
	if mb := parseHBMCapacity("\tNPU ID                         : 0\n\tDDR Capacity(MB)               : 0\n\tHBM Capacity(MB)               : 98304\n\tHBM Clock Speed(MHz)           : 4000\n"); mb != 98304 {
		t.Fatalf("HBM %v", mb)
	}
	cards := map[int]ascendCard{1: {name: "Ascend950DT", memMB: 98304}}
	p := newAscendParser(func() map[int]ascendCard { return cards })
	if _, err := p("NpuID(Idx)  Pwr(W)      Temp(C)     Memory(%)    NPU Util(%) "); err == nil {
		t.Fatal("the header is not a card")
	}
	g, err := p("1           401.6       51          4            12          ")
	if err != nil {
		t.Fatal(err)
	}
	if g.Index != 1 || g.UUID != "npu-1" || g.Name != "Ascend950DT" || g.Util != 12 || g.MemTotal != 98304 || g.MemUsed != 3932.16 ||
		g.Power != 401.6 || g.Temp != 51 || g.SMClock != -1 {
		t.Fatalf("%+v", g)
	}
	// a card whose size is not known: its memory is not either
	if g, err := p("0           401.6       51          4            0"); err != nil || g.MemUsed != -1 || g.MemTotal != -1 || g.Name != "Ascend" {
		t.Fatalf("unknown card: %+v %v", g, err)
	}
	if _, err := newAscendParser(func() map[int]ascendCard { return nil })("0  1  2  3  4"); err == nil {
		t.Fatal("a line before the header")
	}
}

// /dev/davinciN is NPU N (its chip's physical id); davinci_manager is not a card.
func TestGPUMinor_Davinci(t *testing.T) {
	if got, ok := gpuMinor("/dev/davinci6"); !ok || got != 6 {
		t.Fatalf("davinci6: %d %v", got, ok)
	}
	if _, ok := gpuMinor("/dev/davinci_manager"); ok {
		t.Fatal("davinci_manager counted as a card")
	}
}
