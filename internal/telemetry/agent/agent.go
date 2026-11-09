// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	pssEvery       = 10 * time.Second
	reloadEvery    = 30 * time.Second
	pssTop         = 10
	collectorFails = 3
)

// Env is where the agent reads from; DefaultEnv is the real container.
type Env struct {
	ProcRoot, SysRoot, DevRoot, CgroupRoot, FSRoot string
	SSHDPid                                        func() int
	GPU                                            GPUCollector
	TCP                                            procfs.TCPStats
	TokenPath, ConfigPath                          string
	HookDirs                                       []string
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
	pss              map[procfs.PID]uint64
	sockets          *procfs.SocketMap
	gpuProcs         map[procfs.PID][]int
	rdmaProcs        map[procfs.PID]bool
	lastSockMap      time.Time
	prevTCP          map[uint64]procfs.TCPBytes
	tcpRates         map[procfs.PID]procfs.TCPRate
	reportedConns    map[procfs.PID]string
	net              NetCounters
	rdma             []RDMADev
	rdmaPrev         map[string]RDMACounters
	lastCollect      time.Time

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
		attr:          newAttributor(env.SSHDPid()),
		procs:         map[procfs.PID]*procfs.Proc{},
		attrib:        map[procfs.PID]procfs.Attribution{},
		cpuRates:      map[procfs.PID]float64{},
		pss:           map[procfs.PID]uint64{},
		reportedConns: map[procfs.PID]string{},
		rdmaPrev:      map[string]RDMACounters{},
		cmds:          make(chan wire.Command, 1024),
		alerts:        make(chan wire.Alert, 64),
		collectors:    map[string]*collectorState{},
	}
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

// scanProcs runs every ProcScanInterval: scan, sessions, attribution.
func (a *agent) scanProcs(now time.Time) {
	if !a.cfg.ProcessTracking {
		return
	}
	a.safe("procs", func() {
		procs, err := a.scanner.Scan()
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
		refresh := a.sockets == nil || now.Sub(a.lastSockMap) >= a.cfg.SocketMapInterval
		conns := map[procfs.PID]string{}
		if !refresh {
			conns = procfs.SSHConns(a.env.ProcRoot, a.sockets, sessions)
			// A new session is not in the map yet: refresh early (≥ 1 s).
			refresh = len(conns) < len(sessions) && now.Sub(a.lastSockMap) >= time.Second
		}
		if refresh {
			a.sockets, _ = procfs.BuildSocketMap(a.env.ProcRoot, procs)
			a.gpuProcs = ProcGPUs(a.env.ProcRoot, procs)
			a.rdmaProcs = ProcRDMA(a.env.ProcRoot, procs)
			a.lastSockMap = now
			conns = procfs.SSHConns(a.env.ProcRoot, a.sockets, sessions)
		}
		events, attrib := a.attr.Update(procs, conns)
		a.attrib = attrib
		for i := range events {
			e := events[i]
			e.TS = now.UTC()
			a.pendingAudit = append(a.pendingAudit, wire.Record{K: wire.KProcEvent, PE: &e})
		}
		for c, peer := range conns {
			if a.reportedConns[c] != peer {
				a.reportedConns[c] = peer
				a.pendingAudit = append(a.pendingAudit, wire.Record{K: wire.KSSHConn, SC: &wire.SSHConn{TS: now.UTC(), Pid: c.Pid, Start: c.Start, Peer: peer}})
			}
		}
		for c, peer := range a.reportedConns {
			if _, alive := procs[c]; !alive {
				delete(a.reportedConns, c)
				a.pendingAudit = append(a.pendingAudit, wire.Record{K: wire.KSSHConn, SC: &wire.SSHConn{TS: now.UTC(), Pid: c.Pid, Start: c.Start, Peer: peer, Closed: true}})
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
		cur, err := a.env.TCP.Bytes()
		if err != nil {
			a.tcpRates = nil
			return
		}
		if a.sockets != nil {
			a.tcpRates = procfs.PerProcessTCP(a.prevTCP, cur, a.sockets, dt)
		}
		a.prevTCP = cur
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
	type row struct {
		id     procfs.PID
		p      wire.Proc
		active bool
	}
	rows := make([]row, 0, len(a.procs))
	a.cpuRates = map[procfs.PID]float64{}
	for id, p := range a.procs {
		r := row{id: id, p: wire.Proc{TS: now.UTC(), Pid: id.Pid, Start: id.Start, RSS: p.RSS, PSS: a.pss[id], GPU: a.gpuProcs[id], RDMA: a.rdmaProcs[id]}}
		if at, ok := a.attrib[id]; ok {
			r.p.Attr = at.Wire()
		}
		if t, ok := a.tcpRates[id]; ok {
			r.p.TCPRx, r.p.TCPTx = t.Rx, t.Tx
		}
		if prev := a.prevProcs[id]; prev != nil && dt > 0 {
			secs := dt.Seconds()
			if p.CPUNs >= prev.CPUNs {
				r.p.CPU = float64(p.CPUNs-prev.CPUNs) / secs / 1e9
			}
			if p.IORead >= prev.IORead {
				r.p.IOR = float64(p.IORead-prev.IORead) / secs
			}
			if p.IOWrite >= prev.IOWrite {
				r.p.IOW = float64(p.IOWrite-prev.IOWrite) / secs
			}
			r.active = r.p.CPU > 0 || r.p.IOR > 0 || r.p.IOW > 0 || r.p.TCPRx > 0 || r.p.TCPTx > 0 || p.RSS != prev.RSS
		}
		a.cpuRates[id] = r.p.CPU
		rows = append(rows, r)
	}
	keep := map[procfs.PID]bool{}
	for _, key := range []func(r row) float64{
		func(r row) float64 { return r.p.CPU },
		func(r row) float64 { return float64(r.p.RSS) },
		func(r row) float64 { return r.p.IOR + r.p.IOW },
	} {
		sort.Slice(rows, func(i, j int) bool { return key(rows[i]) > key(rows[j]) })
		for i := 0; i < len(rows) && i < a.cfg.TopN && key(rows[i]) > 0; i++ {
			keep[rows[i].id] = true
		}
	}
	var out []wire.Record
	other := wire.Proc{TS: now.UTC()}
	for i := range rows {
		r := rows[i]
		if r.active || keep[r.id] {
			out = append(out, wire.Record{K: wire.KProc, P: &r.p})
			continue
		}
		other.CPU += r.p.CPU
		other.RSS += r.p.RSS
		other.IOR += r.p.IOR
		other.IOW += r.p.IOW
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
		s := CollectSnapshot(a.snapshotInput())
		s.TS = now.UTC()
		a.pendingMetric = append(a.pendingMetric, wire.Record{K: wire.KSnapshot, Snap: &s})
	})
}

// refreshPSS reads smaps_rollup for the top RSS processes.
func (a *agent) refreshPSS() {
	ids := make([]procfs.PID, 0, len(a.procs))
	for id := range a.procs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return a.procs[ids[i]].RSS > a.procs[ids[j]].RSS })
	pss := map[procfs.PID]uint64{}
	for i := 0; i < len(ids) && i < pssTop; i++ {
		pss[ids[i]] = a.scanner.PSS(ids[i].Pid)
	}
	a.pss = pss
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

// Run is the telemetry process (spec §5.5): it collects on the
// configured intervals and uploads until ctx ends.
func Run(ctx context.Context, cfg config.Config, env Env) error {
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
	go up.Run(ctx, a.header, a.buf)
	if cfg.Hooks {
		hr := &HookReader{Dirs: env.HookDirs, BootID: a.bootID, ProcRoot: env.ProcRoot}
		go hr.Run(ctx, a.alive, a.cmds, a.alerts)
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
	var lastFull, lastSnap, lastInfo, lastPSS, lastReload = now, now, now, now, now
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
			if now.Sub(lastPSS) >= pssEvery {
				a.refreshPSS()
				lastPSS = now
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

func newAttributor(sshdPid int) *procfs.Attributor {
	a := procfs.NewAttributor(sshdPid, SessionExe)
	a.SelfPid = os.Getpid()
	return a
}
