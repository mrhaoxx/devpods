// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// GPUCollector reports the latest per-card samples (spec §6.7: an
// interface so NVML can replace nvidia-smi later).
type GPUCollector interface {
	Latest() ([]wire.GPU, bool)
	Close()
}

const nvidiaQuery = "index,uuid,name,utilization.gpu,memory.used,memory.total,power.draw,temperature.gpu,clocks.sm"

// gpuStale drops a card from Latest when nvidia-smi stopped reporting it.
const gpuStale = 5 * time.Second

type nvidiaSMI struct {
	path       string
	minBackoff time.Duration
	cancel     context.CancelFunc
	starts     atomic.Int64

	mu     sync.Mutex
	latest map[string]wire.GPU
	seen   map[string]time.Time
}

// NewNvidiaSMI keeps `nvidia-smi -lms 1000` running, restarting it with
// backoff 1 s → 60 s. A missing binary means GPU is N/A forever.
func NewNvidiaSMI(ctx context.Context, path string) GPUCollector {
	return newNvidiaSMI(ctx, path, time.Second)
}

func newNvidiaSMI(ctx context.Context, path string, minBackoff time.Duration) GPUCollector {
	ctx, cancel := context.WithCancel(ctx)
	n := &nvidiaSMI{path: path, minBackoff: minBackoff, cancel: cancel, latest: map[string]wire.GPU{}, seen: map[string]time.Time{}}
	if _, err := os.Stat(path); err != nil {
		n.starts.Add(1)
		return n // N/A: never started
	}
	go n.loop(ctx)
	return n
}

func (n *nvidiaSMI) Close() { n.cancel() }

func (n *nvidiaSMI) Latest() ([]wire.GPU, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.latest) == 0 {
		return nil, false
	}
	now := time.Now()
	out := make([]wire.GPU, 0, len(n.latest))
	for uuid, g := range n.latest {
		if now.Sub(n.seen[uuid]) <= gpuStale {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, len(out) > 0
}

func (n *nvidiaSMI) loop(ctx context.Context) {
	delay := n.minBackoff
	for ctx.Err() == nil {
		n.starts.Add(1)
		started := time.Now()
		cmd := exec.CommandContext(ctx, n.path, "--query-gpu="+nvidiaQuery, "--format=csv,noheader,nounits", "-lms", "1000")
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if g, err := parseNvidiaCSV(sc.Text()); err == nil {
					n.mu.Lock()
					n.latest[g.UUID], n.seen[g.UUID] = g, time.Now()
					n.mu.Unlock()
				}
			}
			_ = cmd.Wait()
		}
		if time.Since(started) > 10*time.Minute {
			delay = n.minBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 60*time.Second)
	}
}

// parseNvidiaCSV parses one line of nvidiaQuery output; "[N/A]",
// "N/A" and "[Not Supported]" (MIG, unsupported sensors) become -1.
func parseNvidiaCSV(line string) (wire.GPU, error) {
	f := strings.Split(line, ",")
	if len(f) != 9 {
		return wire.GPU{}, fmt.Errorf("nvidia-smi: want 9 fields, got %d", len(f))
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return -1
		}
		return v
	}
	idx, err := strconv.Atoi(f[0])
	if err != nil {
		return wire.GPU{}, fmt.Errorf("nvidia-smi: bad index %q", f[0])
	}
	return wire.GPU{Index: idx, UUID: f[1], Name: f[2], Util: num(f[3]), MemUsed: num(f[4]), MemTotal: num(f[5]),
		Power: num(f[6]), Temp: num(f[7]), SMClock: num(f[8])}, nil
}

// ProcGPUs maps processes to the /dev/nvidia<N> minors they hold open
// (control nodes nvidiactl / nvidia-uvm are ignored).
func ProcGPUs(root string, procs map[procfs.PID]*procfs.Proc) map[procfs.PID][]int {
	g := gpuHolders{}
	procfs.WalkFDs(root, procs, g.add)
	return g.result()
}

// gpuHolders collects the /dev/nvidiaN minors each process holds open.
type gpuHolders map[procfs.PID]map[int]bool

func (g gpuHolders) add(id procfs.PID, t string) {
	if minor, ok := gpuMinor(t); ok {
		if g[id] == nil {
			g[id] = map[int]bool{}
		}
		g[id][minor] = true
	}
}

// gpuMinor parses an fd target "/dev/nvidiaN".
func gpuMinor(t string) (int, bool) {
	v, ok := strings.CutPrefix(t, "/dev/nvidia")
	if !ok {
		return 0, false
	}
	minor, err := strconv.Atoi(v)
	return minor, err == nil
}

func (g gpuHolders) result() map[procfs.PID][]int {
	out := map[procfs.PID][]int{}
	for id, set := range g {
		for m := range set {
			out[id] = append(out[id], m)
		}
		sort.Ints(out[id])
	}
	return out
}
