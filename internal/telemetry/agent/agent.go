// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// SessionExe is the only binary that counts as an SSH session (§5.3).
const SessionExe = "/opt/devpod/libexec/sshd-session"

const (
	fullProcsEvery = 5 * time.Second
	snapshotEvery  = 15 * time.Second
	infoEvery      = 60 * time.Second
	reloadEvery    = 30 * time.Second
	collectorFails = 3
)

// Env is where the agent reads from; DefaultEnv is the real container.
type Env struct {
	ProcRoot, SysRoot, DevRoot, CgroupRoot, FSRoot string
	SSHDPid                                        func() int
	WorkloadPid                                    func() int // nil = none
	GPU                                            GPUCollector
	TCP                                            procfs.TCPStats
	TokenPath, ConfigPath                          string
	HookDirs                                       []string
}

func (e Env) workloadPid() int {
	if e.WorkloadPid == nil {
		return 0
	}
	return e.WorkloadPid()
}

// DefaultEnv reads the real container. PID 1 passes its sshd's pid in
// SUPERVISOR_SSHD_PID.
func DefaultEnv() Env {
	return Env{
		ProcRoot: "/proc", SysRoot: "/sys", DevRoot: "/dev", CgroupRoot: "/sys/fs/cgroup", FSRoot: "/",
		SSHDPid: func() int {
			n, _ := strconv.Atoi(os.Getenv("SUPERVISOR_SSHD_PID"))
			return n
		},
		WorkloadPid: func() int {
			n, _ := strconv.Atoi(os.Getenv("SUPERVISOR_WORKLOAD_PID"))
			return n
		},
		TokenPath: config.TokenPath, ConfigPath: config.ConfigPath, HookDirs: HookDirs,
	}
}

type collectorState struct {
	fails    int
	disabled string
}

type agent struct {
	cfg    config.Config
	env    Env
	bootID string
	seq    uint64
	buf    *Buffer

	cg      *Cgroup
	prevCg  *CgroupSample
	scanner *procfs.Scanner
	attr    *procfs.Attributor

	procs, prevProcs map[procfs.PID]*procfs.Proc
	attrib           map[procfs.PID]procfs.Attribution
	cpuRates         map[procfs.PID]float64
	sockets          *procfs.SocketMap
	gpuProcs         map[procfs.PID][]int
	rdmaProcs        map[procfs.PID]bool
	lastSockMap      time.Time
	prevTCP          map[uint64]procfs.TCPBytes
	tcpRates         map[procfs.PID]procfs.TCPRate
	reportedConns    map[procfs.PID]string
	lastTCP          time.Time
	scanNow          time.Time // stamps the scanner's readings
	lastFullScan     time.Time
	tcpGate          costGate
	listenGate       costGate
	listenSocks      map[string][]procfs.TCPSock // /proc/net/tcp{,6}, refreshed by listenGate
	connCache        map[procfs.PID]string       // session → peer, resolved once
	lastConns        map[procfs.PID]string
	fdCache          map[procfs.PID]fdEntry
	// seams (tests count calls)
	sshConns    func(root string, sm *procfs.SocketMap, sessions []procfs.PID) map[procfs.PID]string
	walkFDs     func(root string, id procfs.PID, visit func(procfs.PID, string))
	life        *lifecycle
	net         NetCounters
	rdma        []RDMADev
	rdmaPrev    map[string]RDMACounters
	lastCollect time.Time

	pendingAudit, pendingMetric []wire.Record
	cmds                        chan wire.Command
	alerts                      chan wire.Alert
	collectors                  map[string]*collectorState
	lastInfo                    string
}

func newAgent(cfg config.Config, env Env) *agent {
	a := &agent{
		cfg: cfg, env: env, bootID: ulid.Make().String(), buf: NewBuffer(cfg.BufferBytes),
		scanner:       &procfs.Scanner{Root: env.ProcRoot},
		attr:          newAttributor(env.SSHDPid(), env.workloadPid(), env.GPU),
		procs:         map[procfs.PID]*procfs.Proc{},
		attrib:        map[procfs.PID]procfs.Attribution{},
		cpuRates:      map[procfs.PID]float64{},
		reportedConns: map[procfs.PID]string{},
		life:          newLifecycle(),
		rdmaPrev:      map[string]RDMACounters{},
		cmds:          make(chan wire.Command, 1024),
		alerts:        make(chan wire.Alert, 64),
		collectors:    map[string]*collectorState{},
	}
	a.scanner.Clock = func() time.Time { return a.scanNow }
	// reported in Info.collectors: the leader alerts hook_missing only
	// where hooks run
	if cfg.Hooks {
		a.collectors["hooks"] = &collectorState{}
	} else {
		a.collectors["hooks"] = &collectorState{disabled: "disabled: config"}
	}
	a.sshConns, a.walkFDs = procfs.SSHConns, procfs.WalkProcFDs
	a.tcpGate = costGate{min: time.Second, max: 30 * time.Second}
	a.listenGate = costGate{min: snapshotEvery, max: 5 * time.Minute}
	a.connCache, a.fdCache = map[procfs.PID]string{}, map[procfs.PID]fdEntry{}
	if cg, err := OpenCgroup(env.CgroupRoot); err == nil {
		a.cg = cg
	} else {
		a.collectors["cgroup"] = &collectorState{disabled: "disabled: " + err.Error()}
	}
	if cfg.RDMA {
		a.rdma, _ = DiscoverRDMA(env.DevRoot, env.SysRoot)
	}
	return a
}

// safe runs one collector; three panics disable it (spec §11.2).
func (a *agent) safe(name string, fn func()) (ok bool) {
	st := a.collectors[name]
	if st == nil {
		st = &collectorState{}
		a.collectors[name] = st
	}
	if st.disabled != "" {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			st.fails++
			if st.fails >= collectorFails {
				st.disabled = fmt.Sprintf("disabled: %v", r)
			}
			ok = false
		}
	}()
	fn()
	return true
}

func (a *agent) collectorStatus() map[string]string {
	out := map[string]string{}
	for name, st := range a.collectors {
		if st.disabled != "" {
			out[name] = st.disabled
		} else {
			out[name] = "ok"
		}
	}
	return out
}

func (a *agent) header() wire.Header {
	return wire.Header{V: wire.Version, DevPodUID: a.cfg.DevPodUID, BootID: a.bootID, AgentVersion: Version}
}

// scanProcs runs every ProcScanInterval with a light scan (new and
// exited pids), and before every collect with a full one (fresh CPU and
// RSS): sessions, attribution, events.
func (a *agent) scanProcs(now time.Time) {
	// a full scan runs with every collect: skip light scans next to one
	if !a.lastFullScan.IsZero() {
		since := now.Sub(a.lastFullScan)
		if since < a.cfg.ProcScanInterval/2 || a.cfg.MetricsInterval-since < a.cfg.ProcScanInterval/2 {
			return
		}
	}
	a.scan(now, false)
}

func (a *agent) scan(now time.Time, full bool) {
	if !a.cfg.ProcessTracking {
		return
	}
	a.scanNow = now
	if full {
		a.lastFullScan = now
	}
	a.safe("procs", func() {
		scan := a.scanner.ScanNew
		if full {
			scan = a.scanner.Scan
		}
		procs, err := scan()
		if err != nil {
			return
		}
		a.procs = procs
		sshd := a.env.SSHDPid()
		var sessions []procfs.PID
		for id, p := range procs {
			if p.Exe == SessionExe && p.PPid == sshd {
				sessions = append(sessions, id)
			}
		}
		// A session's peer never changes: resolve each once (reading its
		// net/tcp walks the node's whole TCP table).
		conns := map[procfs.PID]string{}
		var unresolved []procfs.PID
		for _, sid := range sessions {
			if peer, ok := a.connCache[sid]; ok {
				conns[sid] = peer
			} else {
				unresolved = append(unresolved, sid)
			}
		}
		for sid := range a.connCache {
			if _, alive := procs[sid]; !alive {
				delete(a.connCache, sid)
			}
		}
		refresh := a.sockets == nil || now.Sub(a.lastSockMap) >= a.cfg.SocketMapInterval ||
			(len(unresolved) > 0 && now.Sub(a.lastSockMap) >= time.Second) // a new session: refresh early
		if refresh {
			a.refreshFDs(procs)
			a.lastSockMap = now
		}
		if len(unresolved) > 0 && refresh {
			for sid, peer := range a.sshConns(a.env.ProcRoot, a.sockets, unresolved) {
				conns[sid], a.connCache[sid] = peer, peer
			}
		}
		// Attribution depends only on which processes exist and on the
		// sessions' peers.
		if !a.scanner.Changed() && a.attrib != nil && maps.Equal(conns, a.lastConns) {
			return
		}
		a.lastConns = conns
		events, attrib := a.attr.Update(procs, conns)
		a.attrib = attrib
		for i := range events {
			e := events[i]
			e.TS = now.UTC()
			a.life.event(&e)
			a.pendingAudit = append(a.pendingAudit, wire.Record{K: wire.KProcEvent, PE: &e})
		}
		for c, peer := range conns {
			if a.reportedConns[c] != peer {
				a.reportedConns[c] = peer
				a.pendingAudit = append(a.pendingAudit, wire.Record{K: wire.KSSHConn, SC: &wire.SSHConn{TS: now.UTC(),
					FirstSeen: a.life.connSeen(c, now.UTC()), Pid: c.Pid, Start: c.Start, Peer: peer}})
				for _, e := range a.life.resolved(c, peer, now.UTC()) {
					a.pendingAudit = append(a.pendingAudit, wire.Record{K: wire.KProcEvent, PE: &e})
				}
			}
		}
		for c, peer := range a.reportedConns {
			if _, alive := procs[c]; !alive {
				delete(a.reportedConns, c)
				a.pendingAudit = append(a.pendingAudit, wire.Record{K: wire.KSSHConn, SC: &wire.SSHConn{TS: now.UTC(),
					FirstSeen: a.life.connClosed(c), Pid: c.Pid, Start: c.Start, Peer: peer, Closed: true}})
			}
		}
	})
}

// collect runs every MetricsInterval and adds one second to the buffer.
func (a *agent) collect(now time.Time) {
	var dt time.Duration
	if !a.lastCollect.IsZero() {
		dt = now.Sub(a.lastCollect)
	}
	a.scan(now, true)
	c := wire.Container{TS: now.UTC()}
	if a.cg != nil {
		a.safe("cgroup", func() {
			s, _ := a.cg.Sample(a.prevCg, now)
			a.prevCg = &s
			c.CPU, c.CPULimit, c.ThrottledUsec, c.Cpuset = s.CPUCores, s.CPULimit, s.ThrottledUsec, s.Cpuset
			c.MemWorkingSet, c.MemAnon, c.MemFile, c.MemShmem, c.MemLimit, c.OOMKills = s.MemWorkingSet, s.MemAnon, s.MemFile, s.MemShmem, s.MemLimit, s.OOMKills
			c.IOReadBps, c.IOWriteBps, c.IOReadIops, c.IOWriteIops, c.Pids = s.IOReadBps, s.IOWriteBps, s.IOReadIops, s.IOWriteIops, s.Pids
		})
	}
	a.safe("tcp", func() {
		// The dump walks the node's whole TCP hash table (shared by every
		// network namespace): ~18 ms on a big node, so it is spaced to
		// cost ≤ 0.1 % of a core; per-process TCP rates are averages
		// over that interval.
		if !a.tcpGate.due(now) {
			return // keep the last rates
		}
		start := time.Now()
		cur, err := a.env.TCP.Bytes()
		a.tcpGate.done(now, time.Since(start))
		if err != nil {
			a.tcpRates = nil
			return
		}
		var tdt time.Duration
		if !a.lastTCP.IsZero() {
			tdt = now.Sub(a.lastTCP)
		}
		if a.sockets != nil {
			a.tcpRates = procfs.PerProcessTCP(a.prevTCP, cur, a.sockets, tdt)
		}
		a.prevTCP, a.lastTCP = cur, now
	})
	a.safe("net", func() {
		ns, nc, err := NetTotals(a.env.ProcRoot, a.cfg.HostNetwork, a.tcpRates, &a.net, dt)
		if err == nil || a.cfg.HostNetwork {
			a.net = nc
		}
		c.NetRx, c.NetTx, c.NetTCPRx, c.NetTCPTx, c.NetOtherRx, c.NetOtherTx = ns.Rx, ns.Tx, ns.TCPRx, ns.TCPTx, ns.OtherRx, ns.OtherTx
		c.NetRxPkts, c.NetTxPkts, c.NetErrs, c.NetDrops, c.NetKind = ns.RxPkts, ns.TxPkts, ns.Errs, ns.Drops, ns.Kind
	})
	recs := []wire.Record{{K: wire.KContainer, C: &c}}
	recs = append(recs, a.procRecords(now, dt)...)
	if a.cfg.GPU && a.env.GPU != nil {
		a.safe("gpu", func() {
			gpus, ok := a.env.GPU.Latest()
			if !ok {
				return
			}
			holders := map[int]int{}
			for _, minors := range a.gpuProcs {
				for _, m := range minors {
					holders[m]++
				}
			}
			for i := range gpus {
				g := gpus[i]
				g.TS, g.ProcCount, g.Shared = now.UTC(), holders[g.Index], holders[g.Index] > 1
				recs = append(recs, wire.Record{K: wire.KGPU, G: &g})
			}
		})
	}
	if a.cfg.RDMA {
		a.safe("rdma", func() {
			for _, d := range a.rdma {
				prev := a.rdmaPrev[d.IBDev]
				var pp *RDMACounters
				if prev != nil {
					pp = &prev
				}
				samples, cur, _ := d.Sample(a.env.SysRoot, pp, dt)
				a.rdmaPrev[d.IBDev] = cur
				for i := range samples {
					s := samples[i]
					s.TS = now.UTC()
					recs = append(recs, wire.Record{K: wire.KRDMA, R: &s})
				}
			}
		})
	}
drain:
	for {
		select {
		case cmd := <-a.cmds:
			if at, ok := a.attrib[procfs.PID{Pid: cmd.ShellPid, Start: cmd.ShellStart}]; ok {
				cmd.Attr = at.Wire()
			}
			recs = append(recs, wire.Record{K: wire.KCommand, Cmd: &cmd})
		case al := <-a.alerts:
			recs = append(recs, wire.Record{K: wire.KAlert, Alert: &al})
		default:
			break drain
		}
	}
	recs = append(recs, a.pendingAudit...)
	recs = append(recs, a.pendingMetric...)
	a.pendingAudit, a.pendingMetric = nil, nil
	a.seq++
	a.buf.Add(a.seq, recs)
	a.prevProcs, a.lastCollect = a.procs, now
}

// procRecords reports active processes (usage this interval, or RSS
// changed) plus the top N per metric; everything else is summed into
// one row with Pid 0 (spec §6.2).
func (a *agent) procRecords(now time.Time, dt time.Duration) []wire.Record {
	// Rates first on a small row per process; wire records only for the
	// reported ones (most processes sleep and fold into "other").
	type row struct {
		id             procfs.PID
		p              *procfs.Proc
		cpu, ior, iow  float64
		tcpRx, tcpTx   float64
		active, report bool
	}
	rows := make([]row, 0, len(a.procs))
	a.cpuRates = make(map[procfs.PID]float64, len(a.procs))
	for id, p := range a.procs {
		r := row{id: id, p: p}
		if t, ok := a.tcpRates[id]; ok {
			r.tcpRx, r.tcpTx = t.Rx, t.Tx
		}
		// rates over the time between the two readings: an idle process
		// is re-read only every few seconds (procfs.Scanner)
		if prev := a.prevProcs[id]; prev != nil && p.ReadAt.After(prev.ReadAt) {
			secs := p.ReadAt.Sub(prev.ReadAt).Seconds()
			if p.CPUNs >= prev.CPUNs {
				r.cpu = float64(p.CPUNs-prev.CPUNs) / secs / 1e9
			}
			if p.IORead >= prev.IORead {
				r.ior = float64(p.IORead-prev.IORead) / secs
			}
			if p.IOWrite >= prev.IOWrite {
				r.iow = float64(p.IOWrite-prev.IOWrite) / secs
			}
			r.active = r.cpu > 0 || r.ior > 0 || r.iow > 0 || r.tcpRx > 0 || r.tcpTx > 0 || p.RSS != prev.RSS
		}
		a.cpuRates[id] = r.cpu
		rows = append(rows, r)
	}
	for _, key := range []func(r *row) float64{
		func(r *row) float64 { return r.cpu },
		func(r *row) float64 { return float64(r.p.RSS) },
		func(r *row) float64 { return r.ior + r.iow },
	} {
		sort.Slice(rows, func(i, j int) bool { return key(&rows[i]) > key(&rows[j]) })
		for i := 0; i < len(rows) && i < a.cfg.TopN && key(&rows[i]) > 0; i++ {
			rows[i].report = true
		}
	}
	var out []wire.Record
	other := wire.Proc{TS: now.UTC()}
	for i := range rows {
		r := &rows[i]
		if !r.active && !r.report {
			other.CPU += r.cpu
			other.RSS += r.p.RSS
			other.IOR += r.ior
			other.IOW += r.iow
			continue
		}
		wp := &wire.Proc{TS: now.UTC(), Pid: r.id.Pid, Start: r.id.Start, CPU: r.cpu, RSS: r.p.RSS,
			IOR: r.ior, IOW: r.iow, TCPRx: r.tcpRx, TCPTx: r.tcpTx, GPU: a.gpuProcs[r.id], RDMA: a.rdmaProcs[r.id]}
		if at, ok := a.attrib[r.id]; ok {
			wp.Attr = at.Wire()
		}
		out = append(out, wire.Record{K: wire.KProc, P: wp})
	}
	if len(rows) > 0 {
		out = append(out, wire.Record{K: wire.KProc, P: &other})
	}
	return out
}

func (a *agent) snapshotInput() SnapshotInput {
	return SnapshotInput{ProcRoot: a.env.ProcRoot, Procs: a.procs, Attrib: a.attrib, CPU: a.cpuRates, Sockets: a.sockets, PVCMounts: a.cfg.PVCMounts}
}

// fullProcs queues the 5-second full process list (memory only, §5.10).
func (a *agent) fullProcs(now time.Time) {
	in := a.snapshotInput()
	ids := make([]procfs.PID, 0, len(a.procs))
	for id := range a.procs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return a.cpuRates[ids[i]] > a.cpuRates[ids[j]] })
	a.pendingMetric = append(a.pendingMetric, wire.Record{K: wire.KFullProcs, Full: &wire.FullProcs{TS: now.UTC(), Procs: SnapProcs(in, ids)}})
}

func (a *agent) snapshot(now time.Time) {
	a.safe("snapshot", func() {
		in := a.snapshotInput()
		// listening ports change rarely; the read walks the node's TCP table
		if a.listenSocks == nil || a.listenGate.due(now) {
			start := time.Now()
			a.listenSocks = ReadTCPSocks(a.env.ProcRoot)
			a.listenGate.done(now, time.Since(start))
		}
		in.TCPSocks = a.listenSocks
		s := CollectSnapshot(in)
		s.TS = now.UTC()
		a.pendingMetric = append(a.pendingMetric, wire.Record{K: wire.KSnapshot, Snap: &s})
	})
}

func (a *agent) info(now time.Time) wire.Info {
	var gpus []wire.GPU
	if a.cfg.GPU && a.env.GPU != nil {
		a.safe("gpu", func() { gpus, _ = a.env.GPU.Latest() })
	}
	in := CollectInfo(a.env.FSRoot, a.cg, gpus, a.rdma, a.collectorStatus())
	in.TS = now.UTC()
	return in
}

// queueInfo adds Info when it changed (ignoring the timestamp).
func (a *agent) queueInfo(now time.Time, force bool) {
	in := a.info(now)
	cmp := in
	cmp.TS = time.Time{}
	b, _ := json.Marshal(cmp)
	if !force && string(b) == a.lastInfo {
		return
	}
	a.lastInfo = string(b)
	a.pendingMetric = append(a.pendingMetric, wire.Record{K: wire.KInfo, Info: &in})
}

func (a *agent) alive(pid int) bool {
	_, err := os.Stat(filepath.Join(a.env.ProcRoot, strconv.Itoa(pid)))
	return err == nil
}

type noGPUCollector struct{}

func (noGPUCollector) Latest() ([]wire.GPU, bool) { return nil, false }
func (noGPUCollector) Close()                     {}

// Run is the telemetry agent (spec §5.5): it collects on the configured
// intervals and uploads until ctx ends.
//
// The agent runs inside the supervisor (PID 1): a panic on any goroutine
// it starts cancels the others and comes back from Run as an error
// (IsPanic) for the supervisor to restart it; a panic on Run's own loop
// unwinds through the same cleanup to the supervisor's recover.
func Run(ctx context.Context, cfg config.Config, env Env) (err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel(nil)
		waitAtMost(&wg, 5*time.Second)
		if c := context.Cause(ctx); err == nil && IsPanic(c) {
			err = c
		}
	}()
	if env.GPU == nil {
		env.GPU = noGPUCollector{}
		if cfg.GPU {
			path, err := exec.LookPath("nvidia-smi")
			if err != nil {
				path = "/usr/bin/nvidia-smi"
			}
			env.GPU = NewNvidiaSMI(ctx, path)
		}
	}
	defer env.GPU.Close()
	if env.TCP == nil {
		env.TCP = procfs.NewSockDiag()
	}
	a := newAgent(cfg, env)
	var url atomic.Value
	url.Store(cfg.IngestURL)
	up := &Uploader{URL: func() string { return url.Load().(string) }, TokenPath: env.TokenPath}
	goGuarded(&wg, "uploader", cancel, func() { up.Run(ctx, a.header, a.buf) })
	if cfg.Hooks {
		hr := &HookReader{Dirs: env.HookDirs, BootID: a.bootID, ProcRoot: env.ProcRoot}
		goGuarded(&wg, "hook reader", cancel, func() { hr.Run(ctx, a.alive, a.cmds, a.alerts) })
	}
	scan := time.NewTicker(cfg.ProcScanInterval)
	metrics := time.NewTicker(cfg.MetricsInterval)
	slow := time.NewTicker(time.Second)
	defer scan.Stop()
	defer metrics.Stop()
	defer slow.Stop()
	now := time.Now()
	a.scanProcs(now)
	a.queueInfo(now, true)
	var lastFull, lastSnap, lastInfo, lastReload = now, now, now, now
	for {
		select {
		case <-ctx.Done():
			return nil
		case now = <-scan.C:
			a.scanProcs(now)
		case now = <-metrics.C:
			a.collect(now)
		case now = <-slow.C:
			if now.Sub(lastFull) >= fullProcsEvery {
				a.fullProcs(now)
				lastFull = now
			}
			if now.Sub(lastSnap) >= snapshotEvery {
				a.snapshot(now)
				lastSnap = now
			}
			if now.Sub(lastInfo) >= infoEvery {
				a.queueInfo(now, false)
				lastInfo = now
			}
			if now.Sub(lastReload) >= reloadEvery {
				if c, err := config.LoadConfig(env.ConfigPath); err == nil {
					url.Store(c.IngestURL)
					a.cfg.TopN, a.cfg.PVCMounts = c.TopN, c.PVCMounts
				}
				lastReload = now
			}
		}
	}
}

func newAttributor(sshdPid, workloadPid int, gpu GPUCollector) *procfs.Attributor {
	a := procfs.NewAttributor(sshdPid, SessionExe)
	a.WorkloadPid = workloadPid
	a.SelfPid = os.Getpid()
	if c, ok := gpu.(interface{ ChildPid() int }); ok {
		a.Infra = func(pid int) bool { return pid > 0 && c.ChildPid() == pid }
	}
	return a
}
