// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

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

// mthreadsQuery asks Moore Threads' mthreads-gmi for the same per-card
// values; its SM clock is clocks.current.gpu.
const mthreadsQuery = "index,uuid,name,utilization.gpu,memory.used,memory.total,power.draw,temperature.gpu,clocks.current.gpu"

// gpuStale drops a card from Latest when the tool stopped reporting it.
const gpuStale = 5 * time.Second

// smiCollector keeps a GPU vendor's query tool (nvidia-smi,
// mthreads-gmi) running in a loop and keeps each card's latest line.
type smiCollector struct {
	tool  string // its name, for messages
	path  string
	args  []string
	parse func(line string) (wire.GPU, error)
	// parseDoc, when set, reads the tool's output as a stream of
	// documents — a document's cards each time — instead of lines
	parseDoc   func(dec *json.Decoder) ([]wire.GPU, error)
	before     func(ctx context.Context) // before each run, when set
	minBackoff time.Duration
	cancel     context.CancelFunc
	starts     atomic.Int64
	child      atomic.Int64 // the running tool, 0 between runs

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
	return newSMI(ctx, "nvidia-smi", path, []string{"--query-gpu=" + nvidiaQuery, "--format=csv,noheader,nounits", "-lms", "1000"}, parseNvidiaCSV, nil, minBackoff)
}

// NewMthreadsGMI is NewNvidiaSMI for Moore Threads cards: `mthreads-gmi
// -lms 1000` with the same query (it has no nounits: the values carry
// their units, which the parser drops) — or, for a version that has no
// --query-gpu (2.3.x, with driver 3.3), `mthreads-gmi -q --json -l 1`.
func NewMthreadsGMI(ctx context.Context, path string) GPUCollector {
	if !mthreadsHasQuery(ctx, path) {
		return newSMIWith(ctx, "mthreads-gmi", path, []string{"-q", "--json", "-l", "1"}, nil, nil, time.Second, parseMthreadsJSON)
	}
	return newSMI(ctx, "mthreads-gmi", path, []string{"--query-gpu=" + mthreadsQuery, "--format=csv,noheader", "-lms", "1000"}, parseMthreadsCSV, nil, time.Second)
}

// metaxArgs: MetaX's `mx-smi dmon` every second, as CSV with its header
// (the columns are read by name).
var metaxArgs = []string{"dmon", "--show-usage", "--show-memory", "--total-memory", "--show-temperature", "--show-board-power", "--show-clock", "--format", "csv", "-l", "1000"}

// NewMetaXSMI is NewNvidiaSMI for MetaX cards: `mx-smi dmon`, the cards'
// names and UUIDs from `mx-smi -L` before each run. dmon says the memory
// in use only as a whole percentage of the total.
func NewMetaXSMI(ctx context.Context, path string) GPUCollector {
	var mu sync.Mutex
	var cards map[int]metaxCard
	list := func(ctx context.Context) {
		// PID 1 may reap it before Output waits: what it printed counts
		out, _ := exec.CommandContext(ctx, path, "-L").Output()
		if len(out) > 0 {
			mu.Lock()
			cards = parseMetaXList(string(out))
			mu.Unlock()
		}
	}
	known := func() map[int]metaxCard {
		mu.Lock()
		defer mu.Unlock()
		return cards
	}
	return newSMI(ctx, "mx-smi", path, metaxArgs, newMetaXParser(known), list, time.Second)
}

// ascendArgs: Ascend's `npu-smi info watch`, every second: power,
// temperature, memory (HBM) in use as a percentage, NPU utilisation.
var ascendArgs = []string{"info", "watch", "-s", "ptmn"}

// ascendCard: an NPU's name (`npu-smi info -m`) and HBM size in MB
// (`npu-smi info -t memory`).
type ascendCard struct {
	name  string
	memMB float64
}

// NewAscendSMI is NewNvidiaSMI for Ascend NPUs: `npu-smi info watch`,
// the cards' names and HBM sizes asked before each run (npu-smi's summary
// takes seconds; these are quick).
func NewAscendSMI(ctx context.Context, path string) GPUCollector {
	var mu sync.Mutex
	var cards map[int]ascendCard
	learn := func(ctx context.Context) {
		// PID 1 may reap these before Output waits: what they printed counts
		out, _ := exec.CommandContext(ctx, path, "info", "-m").Output()
		got := map[int]ascendCard{}
		for id, name := range parseAscendMap(string(out)) {
			mem, _ := exec.CommandContext(ctx, path, "info", "-t", "memory", "-i", strconv.Itoa(id)).Output()
			got[id] = ascendCard{name: name, memMB: parseHBMCapacity(string(mem))}
		}
		if len(got) > 0 {
			mu.Lock()
			cards = got
			mu.Unlock()
		}
	}
	known := func() map[int]ascendCard {
		mu.Lock()
		defer mu.Unlock()
		return cards
	}
	return newSMI(ctx, "npu-smi", path, ascendArgs, newAscendParser(known), learn, time.Second)
}

// parseAscendMap reads `npu-smi info -m`: "NPU ID  Slot ID  Chip ID
// Chip Phy-ID  Chip Name" rows — the name by NPU ID.
func parseAscendMap(out string) map[int]string {
	names := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		if id, err := strconv.Atoi(f[0]); err == nil {
			names[id] = strings.Join(f[4:], " ")
		}
	}
	return names
}

// parseHBMCapacity reads "HBM Capacity(MB) : 98304" from `npu-smi info -t
// memory`; -1 when it is not there.
func parseHBMCapacity(out string) float64 {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "HBM Capacity(MB)" {
			if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return n
			}
		}
	}
	return -1
}

// columnGap parts npu-smi's header: its names hold single spaces.
var columnGap = regexp.MustCompile(`\s{2,}`)

// newAscendParser reads `npu-smi info watch` lines: the header says which
// column is which (NpuID(Idx), Pwr(W), Temp(C), Memory(%), NPU Util(%));
// then one line per NPU.
func newAscendParser(cards func() map[int]ascendCard) func(string) (wire.GPU, error) {
	var col map[string]int
	return func(line string) (wire.GPU, error) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "NpuID") {
			col = map[string]int{}
			for i, name := range columnGap.Split(line, -1) {
				col[name] = i
			}
			return wire.GPU{}, errors.New("npu-smi: the header")
		}
		if col == nil {
			return wire.GPU{}, errors.New("npu-smi: a line before the header")
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			return wire.GPU{}, errors.New("npu-smi: an empty line")
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return wire.GPU{}, fmt.Errorf("npu-smi: not an NPU: %q", line)
		}
		get := func(name string) float64 {
			i, ok := col[name]
			if !ok || i >= len(f) {
				return -1
			}
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				return -1
			}
			return v
		}
		g := wire.GPU{Index: idx, UUID: "npu-" + strconv.Itoa(idx), Util: get("NPU Util(%)"), Power: get("Pwr(W)"), Temp: get("Temp(C)"), SMClock: -1, MemUsed: -1, MemTotal: -1}
		c, known := cards()[idx]
		g.Name = c.name
		if g.Name == "" {
			g.Name = "Ascend"
		}
		if known && c.memMB > 0 {
			g.MemTotal = c.memMB
			if used := get("Memory(%)"); used >= 0 {
				g.MemUsed = used * c.memMB / 100
			}
		}
		return g, nil
	}
}

func newSMI(ctx context.Context, tool, path string, args []string, parse func(string) (wire.GPU, error), before func(context.Context), minBackoff time.Duration) GPUCollector {
	return newSMIWith(ctx, tool, path, args, parse, before, minBackoff, nil)
}

// newSMIWith is newSMI with parseDoc: the output read as documents.
func newSMIWith(ctx context.Context, tool, path string, args []string, parse func(string) (wire.GPU, error), before func(context.Context), minBackoff time.Duration, parseDoc func(*json.Decoder) ([]wire.GPU, error)) *smiCollector {
	ctx, cancel := context.WithCancel(ctx)
	n := &smiCollector{tool: tool, path: path, args: args, parse: parse, parseDoc: parseDoc, before: before, minBackoff: minBackoff, cancel: cancel, latest: map[string]wire.GPU{}, seen: map[string]time.Time{}}
	if _, err := os.Stat(path); err != nil {
		n.starts.Add(1)
		return n // N/A: never started
	}
	go n.loop(ctx)
	return n
}

func (n *smiCollector) Close() { n.cancel() }

func (n *smiCollector) Latest() ([]wire.GPU, bool) {
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

// ChildPid is the running tool's pid (0 between runs); the agent counts
// that process as infrastructure.
func (n *smiCollector) ChildPid() int { return int(n.child.Load()) }

func (n *smiCollector) loop(ctx context.Context) {
	delay := n.minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		// a panic (say, on odd output) ends this run only: GPU samples
		// resume after the backoff, and PID 1 never sees it
		if err := safely(n.tool, func() { n.runOnce(ctx) }); err != nil {
			fmt.Fprintln(os.Stderr, err)
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

// runOnce runs the tool until its output ends. The agent lives in PID 1,
// whose reap loop (wait4(-1)) may collect the child before Wait does;
// Wait then fails with ECHILD, which changes nothing here: the end of
// stdout is what ends a run.
func (n *smiCollector) runOnce(ctx context.Context) {
	n.starts.Add(1)
	if n.before != nil {
		n.before(ctx)
	}
	cmd := exec.CommandContext(ctx, n.path, n.args...)
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return
	}
	n.child.Store(int64(cmd.Process.Pid))
	defer n.child.Store(0)
	keep := func(g wire.GPU) {
		n.mu.Lock()
		n.latest[g.UUID], n.seen[g.UUID] = g, time.Now()
		n.mu.Unlock()
	}
	if n.parseDoc != nil {
		dec := json.NewDecoder(out)
		for {
			gs, err := n.parseDoc(dec)
			if err != nil {
				break // the end, or not a document: run again (with backoff)
			}
			for _, g := range gs {
				keep(g)
			}
		}
		_, _ = io.Copy(io.Discard, out)
		_ = cmd.Wait()
		return
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if g, err := n.parse(sc.Text()); err == nil {
			keep(g)
		}
	}
	_ = cmd.Wait()
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

// parseMthreadsCSV parses one line of mthreadsQuery output: the values
// with their units ("59630 [MiB]", "400.49 [W]"), the units dropped;
// "N/A" becomes -1.
func parseMthreadsCSV(line string) (wire.GPU, error) {
	f := strings.Split(line, ",")
	if len(f) != 9 {
		return wire.GPU{}, fmt.Errorf("mthreads-gmi: want 9 fields, got %d", len(f))
	}
	for i := range f {
		v := strings.TrimSpace(f[i])
		if i > 2 { // after index, uuid and name: a number and its unit
			if at := strings.Index(v, " ["); at > 0 {
				v = v[:at]
			}
		}
		f[i] = v
	}
	return parseNvidiaCSV(strings.Join(f, ","))
}

// mthreadsHasQuery: does this mthreads-gmi take --query-gpu? Asked once:
// a version that does not says "input is unknow" and no card.
func mthreadsHasQuery(ctx context.Context, path string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path, "--query-gpu=index", "--format=csv,noheader").CombinedOutput()
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	idx, _, _ := strings.Cut(first, ",")
	_, err := strconv.Atoi(strings.TrimSpace(idx))
	return err == nil
}

// mthreadsDoc: what of `mthreads-gmi -q --json` is read.
type mthreadsDoc struct {
	GPU []struct {
		Index  string `json:"Index"`
		Name   string `json:"Product Name"`
		UUID   string `json:"GPU UUID"`
		Memory struct {
			Total string `json:"Total"`
			Used  string `json:"Used"`
		} `json:"FB Memory Usage"`
		Utilization struct {
			GPU string `json:"Gpu"`
		} `json:"Utilization"`
		Temperature struct {
			Current string `json:"GPU Current Temp"`
		} `json:"Temperature"`
		Power  map[string]string `json:"Power Readings"` // its "Power Draw " has a space after it
		Clocks struct {
			Graphics string `json:"Graphics"`
		} `json:"Clocks"`
	} `json:"GPU"`
}

// parseMthreadsJSON reads the next document of `mthreads-gmi -q --json`:
// the values with their units ("81920MiB", "410.68W", "38C", "7%"), the
// units dropped; "N/A" becomes -1.
func parseMthreadsJSON(dec *json.Decoder) ([]wire.GPU, error) {
	var d mthreadsDoc
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	num := func(s string) float64 {
		s = strings.TrimRightFunc(strings.TrimSpace(s), func(r rune) bool { return !unicode.IsDigit(r) && r != '.' })
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return -1
		}
		return v
	}
	out := make([]wire.GPU, 0, len(d.GPU))
	for _, g := range d.GPU {
		idx, err := strconv.Atoi(strings.TrimSpace(g.Index))
		if err != nil {
			continue
		}
		power := "N/A"
		for k, v := range g.Power {
			if strings.TrimSpace(k) == "Power Draw" {
				power = v
			}
		}
		out = append(out, wire.GPU{Index: idx, UUID: g.UUID, Name: g.Name, Util: num(g.Utilization.GPU), MemUsed: num(g.Memory.Used), MemTotal: num(g.Memory.Total),
			Power: num(power), Temp: num(g.Temperature.Current), SMClock: num(g.Clocks.Graphics)})
	}
	return out, nil
}

// metaxCard: what `mx-smi -L` says of a card.
type metaxCard struct{ name, uuid string }

var metaxListLine = regexp.MustCompile(`^GPU#(\d+)\s+(\S+)\s.*UUID:\s*([^)\s]+)\)`)

// parseMetaXList reads `mx-smi -L`: "GPU#0  MXC500  0000:0e:00.0
// Available (UUID: GPU-…)", by index.
func parseMetaXList(out string) map[int]metaxCard {
	cards := map[int]metaxCard{}
	for _, line := range strings.Split(out, "\n") {
		m := metaxListLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if i, err := strconv.Atoi(m[1]); err == nil {
			cards[i] = metaxCard{name: m[2], uuid: m[3]}
		}
	}
	return cards
}

// newMetaXParser reads `mx-smi dmon --format csv` lines: the header says
// which column is which (dev, die, gpu %, vram %, total GB, hottemp C,
// power W, xcoreclk MHz); then one line per card (and die — die 0
// stands for the card).
func newMetaXParser(cards func() map[int]metaxCard) func(string) (wire.GPU, error) {
	var col map[string]int
	return func(line string) (wire.GPU, error) {
		f := strings.Split(line, ",")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if f[0] == "dev" {
			col = map[string]int{}
			for i, name := range f {
				col[name] = i
			}
			return wire.GPU{}, errors.New("mx-smi: the header")
		}
		if col == nil {
			return wire.GPU{}, errors.New("mx-smi: a line before the header")
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return wire.GPU{}, fmt.Errorf("mx-smi: not a card: %q", line) // the units line, say
		}
		get := func(name string) float64 {
			i, ok := col[name]
			if !ok || i >= len(f) {
				return -1
			}
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				return -1
			}
			return v
		}
		if die := get("die"); die > 0 {
			return wire.GPU{}, fmt.Errorf("mx-smi: card %d die %v", idx, die)
		}
		g := wire.GPU{Index: idx, Util: get("gpu"), Power: get("power"), Temp: get("hottemp"), SMClock: get("xcoreclk"), MemUsed: -1, MemTotal: -1}
		if total := get("total"); total >= 0 {
			g.MemTotal = total * 1024 // GB → MiB
			if used := get("vram"); used >= 0 {
				g.MemUsed = used * g.MemTotal / 100
			}
		}
		c := cards()[idx]
		g.Name, g.UUID = c.name, c.uuid
		if g.Name == "" {
			g.Name = "MetaX"
		}
		if g.UUID == "" {
			g.UUID = "metax-" + strconv.Itoa(idx)
		}
		return g, nil
	}
}

// ProcGPUs maps processes to the cards they hold open: /dev/nvidia<N>,
// Moore Threads' /dev/mtgpu.<N>, Ascend's /dev/davinci<N>, or the DRM
// render node of card N (renderD<128+N>, which MetaX's cards are used
// through); control nodes (nvidiactl, nvidia-uvm, mxcd, davinci_manager)
// are not cards.
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

// gpuMinor parses an fd target "/dev/nvidiaN", "/dev/mtgpu.N" or
// "/dev/dri/renderD<128+N>": card N, as the tool numbers it (the same PCI
// device).
func gpuMinor(t string) (int, bool) {
	if v, ok := strings.CutPrefix(t, "/dev/dri/renderD"); ok {
		n, err := strconv.Atoi(v)
		return n - 128, err == nil && n >= 128
	}
	for _, prefix := range []string{"/dev/nvidia", "/dev/mtgpu.", "/dev/davinci"} {
		if v, ok := strings.CutPrefix(t, prefix); ok {
			minor, err := strconv.Atoi(v)
			return minor, err == nil
		}
	}
	return 0, false
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
