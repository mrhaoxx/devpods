// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package live

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// Stale is how old a sample may be before it counts as "no data".
const Stale = 5 * time.Second

// procStale: the full process list comes every 5 s.
const procStale = 2 * Stale

type connKey struct {
	pid   int
	start uint64
}

type podState struct {
	boot   string    // agent boot the data belongs to
	at     time.Time // last container sample
	procAt time.Time // last process data
	c      wire.Container
	gpus   map[string]wire.GPU
	rdma   map[string]wire.RDMA
	procs  map[connKey]wire.Proc // latest per-second sample per process
	full   *wire.FullProcs
	conns  map[connKey]string // live sshd-session → peer
}

// State holds the latest telemetry per DevPod UID.
type State struct {
	// ResolveUser maps (DevPod UID, session peer) to the session's user;
	// set by the ingest wiring, nil = unknown.
	ResolveUser func(uid, peer string) string

	now  func() time.Time
	mu   sync.RWMutex
	pods map[string]*podState
}

// NewState creates an empty state.
func NewState(now func() time.Time) *State {
	if now == nil {
		now = time.Now
	}
	return &State{now: now, pods: map[string]*podState{}}
}

// Apply folds one ingested batch in; later records win.
func (s *State) Apply(uid string, recs []wire.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pods[uid]
	if p == nil {
		p = newPodState()
		s.pods[uid] = p
	}
	sawGPU, sawRDMA, sawProc := false, false, false
	for _, r := range recs {
		switch r.K {
		case wire.KContainer:
			p.c, p.at = *r.C, s.now()
		case wire.KGPU:
			if !sawGPU {
				p.gpus, sawGPU = map[string]wire.GPU{}, true
			}
			p.gpus[r.G.UUID] = *r.G
		case wire.KRDMA:
			if !sawRDMA {
				p.rdma, sawRDMA = map[string]wire.RDMA{}, true
			}
			p.rdma[r.R.IBDev+"/"+strconv.Itoa(int(r.R.Port))] = *r.R
		case wire.KProc:
			if !sawProc {
				p.procs, sawProc = map[connKey]wire.Proc{}, true
			}
			p.procs[connKey{r.P.Pid, r.P.Start}] = *r.P
			p.procAt = s.now()
		case wire.KFullProcs:
			f := *r.Full
			p.full, p.procAt = &f, s.now()
		case wire.KSSHConn:
			k := connKey{r.SC.Pid, r.SC.Start}
			if r.SC.Closed {
				delete(p.conns, k)
			} else {
				p.conns[k] = r.SC.Peer
			}
		}
	}
}

// LastSample returns when the DevPod's last container sample arrived.
func (s *State) LastSample(uid string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p := s.pods[uid]; p != nil && !p.at.IsZero() {
		return p.at, true
	}
	return time.Time{}, false
}

// Retain drops every DevPod for which keep is false (deleted ones).
func (s *State) Retain(keep func(uid string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uid := range s.pods {
		if !keep(uid) {
			delete(s.pods, uid)
		}
	}
}

// SetBoot records which agent boot the next batches come from; a new
// boot (container or agent restart) drops the old one's sessions and
// processes, whose close and exit events never came.
func (s *State) SetBoot(uid, boot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pods[uid]
	if p == nil {
		p = newPodState()
		s.pods[uid] = p
	}
	if p.boot != boot {
		p.boot = boot
		p.conns, p.procs, p.full = map[connKey]string{}, map[connKey]wire.Proc{}, nil
	}
}

func newPodState() *podState {
	return &podState{gpus: map[string]wire.GPU{}, rdma: map[string]wire.RDMA{}, procs: map[connKey]wire.Proc{}, conns: map[connKey]string{}}
}

// Forget drops a DevPod (deleted).
func (s *State) Forget(uid string) {
	s.mu.Lock()
	delete(s.pods, uid)
	s.mu.Unlock()
}

func f64(v float64) *float64 { return &v }

// devpodLive renders one DevPod; callers hold s.mu.
func (s *State) devpodLive(ref DevPodRef, now time.Time) DevPodLive {
	d := DevPodLive{CPULimit: ref.CPULimit, MemLimit: ref.MemLimit, GPU: []GPULive{}, Sessions: []SessionLive{}}
	p := s.pods[ref.UID]
	if p == nil || p.at.IsZero() || now.Sub(p.at) > Stale {
		return d
	}
	c := p.c
	if c.CPULimit > 0 {
		d.CPULimit = c.CPULimit
	}
	if c.MemLimit > 0 {
		d.MemLimit = float64(c.MemLimit)
	}
	d.CPU, d.Mem = f64(c.CPU), f64(float64(c.MemWorkingSet))
	d.NetRx, d.NetTx, d.NetKind = f64(c.NetRx), f64(c.NetTx), c.NetKind
	d.IOR, d.IOW = f64(c.IOReadBps), f64(c.IOWriteBps)
	for _, g := range p.gpus {
		d.GPU = append(d.GPU, GPULive{Index: g.Index, UUID: g.UUID, Name: g.Name, Util: g.Util, MemUsed: g.MemUsed, MemTotal: g.MemTotal, Power: g.Power})
	}
	sort.Slice(d.GPU, func(i, j int) bool { return d.GPU[i].Index < d.GPU[j].Index })
	if len(p.rdma) > 0 {
		var rx, tx float64
		kind := "vf"
		for _, r := range p.rdma {
			rx, tx = rx+r.RxBps, tx+r.TxBps
			if r.Scope != "vf_exclusive" {
				kind = "shared"
			}
		}
		d.RDMARx, d.RDMATx, d.RDMAKind = f64(rx), f64(tx), kind
	}
	seen := map[string]bool{}
	for _, peer := range p.conns {
		u := ""
		if s.ResolveUser != nil {
			u = s.ResolveUser(ref.UID, peer)
		}
		if u != "" && !seen[u] {
			seen[u] = true
			d.Sessions = append(d.Sessions, SessionLive{User: u})
		}
	}
	sort.Slice(d.Sessions, func(i, j int) bool { return d.Sessions[i].User < d.Sessions[j].User })
	return d
}

// DevPodSample is a DevPod's full latest sample (detail page "metrics"
// event): the container sample plus every GPU and RDMA port.
type DevPodSample struct {
	TS        time.Time      `json:"ts"`
	Container wire.Container `json:"container"`
	GPUs      []wire.GPU     `json:"gpus"`
	RDMA      []wire.RDMA    `json:"rdma"`
}

// Sample returns the latest sample unless it is stale.
func (s *State) Sample(uid string) (DevPodSample, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.pods[uid]
	if p == nil || p.at.IsZero() || s.now().Sub(p.at) > Stale {
		return DevPodSample{}, false
	}
	out := DevPodSample{TS: p.at.UTC(), Container: p.c, GPUs: []wire.GPU{}, RDMA: []wire.RDMA{}}
	for _, g := range p.gpus {
		out.GPUs = append(out.GPUs, g)
	}
	sort.Slice(out.GPUs, func(i, j int) bool { return out.GPUs[i].Index < out.GPUs[j].Index })
	for _, r := range p.rdma {
		out.RDMA = append(out.RDMA, r)
	}
	sort.Slice(out.RDMA, func(i, j int) bool {
		if out.RDMA[i].IBDev != out.RDMA[j].IBDev {
			return out.RDMA[i].IBDev < out.RDMA[j].IBDev
		}
		return out.RDMA[i].Port < out.RDMA[j].Port
	})
	return out, true
}

// Processes lists a DevPod's processes: the 5-second full list merged
// with the latest per-second metrics; all=false keeps the top 10 by CPU.
func (s *State) Processes(uid string, all bool) []ProcLive {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.pods[uid]
	if p == nil || p.procAt.IsZero() || s.now().Sub(p.procAt) > procStale {
		return []ProcLive{}
	}
	byKey := map[connKey]*ProcLive{}
	var out []*ProcLive
	add := func(k connKey) *ProcLive {
		if pl := byKey[k]; pl != nil {
			return pl
		}
		pl := &ProcLive{Pid: k.pid, Start: k.start}
		byKey[k] = pl
		out = append(out, pl)
		return pl
	}
	resolve := func(pl *ProcLive, a *wire.Attr) {
		if a == nil {
			return
		}
		pl.How = a.How
		if s.ResolveUser != nil && a.Peer != "" {
			pl.User = s.ResolveUser(uid, a.Peer)
		}
	}
	if p.full != nil {
		for _, sp := range p.full.Procs {
			pl := add(connKey{sp.Pid, sp.Start})
			pl.PPid, pl.Exe, pl.Cmdline, pl.CPU, pl.RSS = sp.PPid, sp.Exe, sp.Cmdline, sp.CPU, sp.RSS
			resolve(pl, sp.Attr)
		}
	}
	for k, pr := range p.procs {
		if k.pid == 0 {
			continue // the "other" row
		}
		pl := add(k)
		pl.CPU, pl.RSS, pl.IOR, pl.IOW, pl.TCPRx, pl.TCPTx, pl.GPU = pr.CPU, pr.RSS, pr.IOR, pr.IOW, pr.TCPRx, pr.TCPTx, pr.GPU
		if pr.Attr != nil {
			resolve(pl, pr.Attr)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPU != out[j].CPU {
			return out[i].CPU > out[j].CPU
		}
		return out[i].RSS > out[j].RSS
	})
	if !all && len(out) > 10 {
		out = out[:10]
	}
	res := make([]ProcLive, len(out))
	for i, pl := range out {
		res[i] = *pl
	}
	return res
}
