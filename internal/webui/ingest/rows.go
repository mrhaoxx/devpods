// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

const sourceSupervisor = "supervisor"

func ver(t time.Time) uint64 { return uint64(t.UnixMilli()) }

func at(ts, fallback time.Time) time.Time {
	if ts.IsZero() {
		return fallback
	}
	return ts.UTC()
}

func how(a *wire.Attr) string {
	if a == nil || a.How == "" {
		return "unattributed"
	}
	return a.How
}

func peer(a *wire.Attr) string {
	if a == nil {
		return ""
	}
	return a.Peer
}

// commandAttribution maps the agent's rule to the commands table's
// exact / inferred / pending / unattributed.
func commandAttribution(a *wire.Attr, sessionID string) string {
	switch h := how(a); {
	case sessionID == Pending:
		return Pending
	case h == "tree" || h == "ssh_connection" || h == "sid_tty":
		return "exact"
	case h == "inferred":
		return "inferred"
	default:
		return "unattributed"
	}
}

// rows converts one batch to table rows. Alert rows (supervisor alerts,
// untracked entries, unattributed counts) also become k8s Events.
func (h *Handler) rows(hd wire.Header, recs []wire.Record) store.TelemetryBatch {
	var b store.TelemetryBatch
	uid, boot := hd.DevPodUID, hd.BootID
	fallback := at(hd.TS, time.Now())
	unattributed := 0
	var lastUnattributed time.Time
	for _, r := range recs {
		switch r.K {
		case wire.KContainer:
			c := r.C
			b.Containers = append(b.Containers, store.ContainerMetricRow{TS: at(c.TS, fallback), DevPodUID: uid, BootID: boot,
				CPU: float32(c.CPU), CPULimit: float32(c.CPULimit), ThrottledUsec: c.ThrottledUsec,
				MemWS: c.MemWorkingSet, MemAnon: c.MemAnon, MemFile: c.MemFile, MemShmem: c.MemShmem, MemLimit: c.MemLimit, OOMKills: c.OOMKills,
				IOR: float32(c.IOReadBps), IOW: float32(c.IOWriteBps), IORIops: float32(c.IOReadIops), IOWIops: float32(c.IOWriteIops),
				NetRx: float32(c.NetRx), NetTx: float32(c.NetTx), NetTCPRx: float32(c.NetTCPRx), NetTCPTx: float32(c.NetTCPTx),
				NetOtherRx: float32(c.NetOtherRx), NetOtherTx: float32(c.NetOtherTx), NetRxPkts: float32(c.NetRxPkts), NetTxPkts: float32(c.NetTxPkts),
				NetErrs: float32(c.NetErrs), NetDrops: float32(c.NetDrops), NetKind: c.NetKind, Pids: uint32(c.Pids)})
		case wire.KGPU:
			g := r.G
			var shared uint8
			if g.Shared {
				shared = 1
			}
			b.GPUs = append(b.GPUs, store.GPUMetricRow{TS: at(g.TS, fallback), DevPodUID: uid, GPUUUID: g.UUID, Index: uint8(g.Index), Name: g.Name,
				Util: float32(g.Util), MemUsed: float32(g.MemUsed), MemTotal: float32(g.MemTotal), Power: float32(g.Power), Temp: float32(g.Temp),
				SMClock: float32(g.SMClock), ProcCount: uint16(g.ProcCount), Shared: shared})
		case wire.KRDMA:
			x := r.R
			b.RDMAs = append(b.RDMAs, store.RDMAMetricRow{TS: at(x.TS, fallback), DevPodUID: uid, IBDev: x.IBDev, Port: uint8(x.Port),
				TxBps: float32(x.TxBps), RxBps: float32(x.RxBps), TxPps: float32(x.TxPps), RxPps: float32(x.RxPps), Health: x.Health, Scope: x.Scope})
		case wire.KProc:
			p := r.P
			sid, _ := h.Resolver.Resolve(uid, peer(p.Attr))
			gpus := make([]uint8, len(p.GPU))
			for i, g := range p.GPU {
				gpus[i] = uint8(g)
			}
			var rdma uint8
			if p.RDMA {
				rdma = 1
			}
			b.Procs = append(b.Procs, store.ProcMetricRow{TS: at(p.TS, fallback), DevPodUID: uid, BootID: boot, Pid: uint32(p.Pid), StartTime: p.Start,
				CPU: float32(p.CPU), RSS: p.RSS, PSS: p.PSS, IOR: float32(p.IOR), IOW: float32(p.IOW), TCPRx: float32(p.TCPRx), TCPTx: float32(p.TCPTx),
				GPU: gpus, RDMA: rdma, SessionID: sid, Peer: peer(p.Attr)})
		case wire.KProcEvent:
			e := r.PE
			ts := at(e.TS, fallback)
			switch e.Event {
			case "process_start", "process_exit":
				sid, user := h.Resolver.Resolve(uid, peer(e.Attr))
				row := store.ProcessRow{DevPodUID: uid, BootID: boot, Pid: uint32(e.Pid), StartTime: e.Start, PPid: uint32(e.PPid),
					Exe: e.Exe, Cmdline: e.Cmdline, UID: uint32(e.UID), FirstSeen: at(e.FirstSeen, ts), Source: sourceSupervisor,
					SessionID: sid, User: user, Peer: peer(e.Attr), Attribution: how(e.Attr), Version: ver(ts)}
				if e.Event == "process_exit" {
					row.ExitSeen = &ts
				} else if row.Attribution == "unattributed" && e.Pid != 1 {
					unattributed++
					lastUnattributed = ts
				}
				b.ProcRows = append(b.ProcRows, row)
			case "untracked_entry":
				b.Alerts = append(b.Alerts, store.AlertRow{TS: ts, DevPodUID: uid, Kind: "untracked_entry", Source: sourceSupervisor,
					Detail: fmt.Sprintf("pid %d entered without a session: %s", e.Pid, firstNonEmpty(e.Cmdline, e.Exe))})
			}
		case wire.KSSHConn:
			c := r.SC
			ts := at(c.TS, fallback)
			sid, user := h.Resolver.Resolve(uid, c.Peer)
			row := store.SSHConnRow{DevPodUID: uid, BootID: boot, Pid: uint32(c.Pid), StartTime: c.Start, PeerAddr: c.Peer,
				FirstSeen: at(c.FirstSeen, ts), SessionID: sid, User: user, Version: ver(ts)}
			if c.Closed {
				row.ClosedAt = &ts
			}
			b.SSHConns = append(b.SSHConns, row)
		case wire.KCommand:
			c := r.Cmd
			ts := at(c.TS, fallback)
			sid, user := h.Resolver.Resolve(uid, peer(c.Attr))
			exit := int32(c.Exit)
			var dur int64
			if !c.End.IsZero() && c.End.After(ts) {
				dur = c.End.Sub(ts).Milliseconds()
			}
			b.Commands = append(b.Commands, store.CommandRow{EventID: c.EventID, TS: ts, SessionID: sid, DevPodUID: uid, User: user,
				Source: "shell_hook", PID: int32(c.ShellPid), Cwd: c.Cwd, Cmdline: c.Cmd, ExitCode: &exit, DurationMS: dur,
				Attribution: commandAttribution(c.Attr, sid), Version: 1})
		case wire.KInfo:
			ts := at(r.Info.TS, fallback)
			j, _ := json.Marshal(r.Info)
			b.Infos = append(b.Infos, store.ContainerInfoRow{TS: ts, DevPodUID: uid, BootID: boot, Info: string(j), Version: ver(ts)})
		case wire.KSnapshot:
			j, _ := json.Marshal(r.Snap)
			b.Snapshots = append(b.Snapshots, store.SnapshotRow{TS: at(r.Snap.TS, fallback), DevPodUID: uid, Data: string(j)})
		case wire.KAlert:
			a := r.Alert
			b.Alerts = append(b.Alerts, store.AlertRow{TS: at(a.TS, fallback), DevPodUID: uid, Kind: a.Kind, Source: sourceSupervisor, Detail: a.Detail})
		}
	}
	if unattributed > 0 {
		b.Alerts = append(b.Alerts, store.AlertRow{TS: lastUnattributed, DevPodUID: uid, Kind: "unattributed", Source: sourceSupervisor,
			Detail: fmt.Sprintf("%d new process(es) could not be attributed to a session", unattributed)})
	}
	return b
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
