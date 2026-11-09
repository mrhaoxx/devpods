// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package procfs

import (
	"net"
	"sort"
	"strings"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// Attribution of a process to an SSH connection (spec §5.3).
type Attribution struct {
	Conn PID    // the sshd-session; zero when only Peer is known
	Peer string // gateway backend_local_addr
	How  string // tree | ssh_connection | sid_tty | inferred | unattributed | system | workload | untracked
}

// Wire converts to the wire form.
func (a Attribution) Wire() *wire.Attr {
	return &wire.Attr{ConnPid: a.Conn.Pid, ConnStart: a.Conn.Start, Peer: a.Peer, How: a.How}
}

func (a Attribution) attributed() bool {
	switch a.How {
	case "", "unattributed", "system", "workload", "untracked":
		return false
	}
	return true
}

// Attributor keeps sticky attributions across scans.
type Attributor struct {
	SSHDPid    int
	SessionExe string
	// SelfPid is the telemetry agent; it and its children (nvidia-smi …)
	// are infrastructure like PID 1 and the sshd.
	SelfPid int
	// WorkloadPid is the DevPod's own command, started by the supervisor
	// in its own process group: that tree is "workload", never a
	// session's and never unattributed.
	WorkloadPid int

	known     map[PID]Attribution
	peers     map[PID]string // sshd-session → peer, remembered after it exits
	untracked map[PID]bool
}

// NewAttributor attributes to sessions of the sshd with pid sshdPid.
func NewAttributor(sshdPid int, sessionExe string) *Attributor {
	return &Attributor{SSHDPid: sshdPid, SessionExe: sessionExe,
		known: map[PID]Attribution{}, peers: map[PID]string{}, untracked: map[PID]bool{}}
}

// legit: the real sshd-session binary, started by our sshd (§5.3 rule 1).
func (a *Attributor) legit(p *Proc) bool {
	return p.Exe == a.SessionExe && p.PPid == a.SSHDPid && a.SSHDPid > 0
}

// Update folds one scan in. conns maps live sshd-sessions to their peer
// address. It returns process_start / process_exit / untracked_entry
// events (TS left for the caller) and the attribution of every current
// process.
func (a *Attributor) Update(procs map[PID]*Proc, conns map[PID]string) ([]wire.ProcEvent, map[PID]Attribution) {
	for c, peer := range conns {
		if peer != "" {
			a.peers[c] = peer
		}
	}
	var events []wire.ProcEvent
	for id, at := range a.known {
		if _, alive := procs[id]; !alive {
			events = append(events, wire.ProcEvent{Event: "process_exit", Pid: id.Pid, Start: id.Start, Attr: a.resolve(at).Wire()})
			delete(a.known, id)
			delete(a.untracked, id)
		}
	}
	// Parents start before children: evaluate in start order so a
	// child can inherit its parent's attribution.
	ids := make([]PID, 0, len(procs))
	for id := range procs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].Start != ids[j].Start {
			return ids[i].Start < ids[j].Start
		}
		return ids[i].Pid < ids[j].Pid
	})
	for _, id := range ids {
		p := procs[id]
		if _, seen := a.known[id]; !seen {
			at := a.decide(p, procs, conns)
			a.known[id] = at
			events = append(events, wire.ProcEvent{Event: "process_start", Pid: id.Pid, Start: id.Start, PPid: p.PPid,
				Exe: p.Exe, Cmdline: p.Cmdline, UID: p.UID, Attr: a.resolve(at).Wire()})
		}
		if p.PPid == 0 && id.Pid != 1 && !a.untracked[id] {
			a.untracked[id] = true
			events = append(events, wire.ProcEvent{Event: "untracked_entry", Pid: id.Pid, Start: id.Start,
				Exe: p.Exe, Cmdline: p.Cmdline, UID: p.UID})
		}
	}
	out := make(map[PID]Attribution, len(procs))
	for _, id := range ids {
		out[id] = a.resolve(a.known[id])
	}
	return events, out
}

// resolve fills Peer from the remembered session peers.
func (a *Attributor) resolve(at Attribution) Attribution {
	if at.Peer == "" && at.Conn.Pid != 0 {
		at.Peer = a.peers[at.Conn]
	}
	return at
}

func (a *Attributor) decide(p *Proc, procs map[PID]*Proc, conns map[PID]string) Attribution {
	// PID 1 (the supervisor), the sshd and the telemetry agent's own
	// subtree are infrastructure ("system"), never a session's; orphans
	// re-parented to PID 1 or the sshd must not inherit from them.
	if p.ID.Pid == 1 || p.ID.Pid == a.SSHDPid || (a.SelfPid > 1 && (p.ID.Pid == a.SelfPid || p.PPid == a.SelfPid)) {
		return Attribution{How: "system"}
	}
	if p.PPid != 1 && p.PPid != a.SSHDPid && a.known[p.Parent].How == "system" {
		return Attribution{How: "system"}
	}
	// The DevPod's command and its process group (orphans included).
	if a.WorkloadPid > 1 && (p.ID.Pid == a.WorkloadPid || p.PGID == a.WorkloadPid ||
		(p.PPid != 1 && a.known[p.Parent].How == "workload")) {
		return Attribution{How: "workload"}
	}
	// Entered from outside (kubectl exec: PPid 0), and its children:
	// never inferred to whichever SSH session happens to be open.
	if p.PPid == 0 || (p.PPid != 1 && p.PPid != a.SSHDPid && a.known[p.Parent].How == "untracked") {
		return Attribution{How: "untracked"}
	}
	// 1. process tree: the session itself, or inherit from the parent.
	if a.legit(p) {
		return Attribution{Conn: p.ID, How: "tree"}
	}
	if parent := procs[p.Parent]; parent != nil {
		if a.legit(parent) {
			return Attribution{Conn: parent.ID, How: "tree"}
		}
		if at, ok := a.known[parent.ID]; ok && at.attributed() {
			return at
		}
	}
	// 2. SSH_CONNECTION set by sshd and inherited (tmux, nohup, setsid).
	if f := strings.Fields(p.SSHConnection); len(f) == 4 {
		peer := net.JoinHostPort(f[0], f[1])
		for c, cp := range a.peers {
			if cp == peer {
				if _, live := conns[c]; live {
					return Attribution{Conn: c, Peer: peer, How: "ssh_connection"}
				}
			}
		}
		return Attribution{Peer: peer, How: "ssh_connection"}
	}
	// 3. same session id (not PID 1's) or controlling terminal.
	for _, q := range procs {
		if q.ID == p.ID {
			continue
		}
		at, ok := a.known[q.ID]
		if !ok || !at.attributed() {
			continue
		}
		if (p.SID > 1 && q.SID == p.SID) || (p.TTY != 0 && q.TTY == p.TTY) {
			at.How = "sid_tty"
			return at
		}
	}
	// 4. time window: only one active connection, and the process
	// started after it → that connection, marked inferred.
	if len(conns) == 1 {
		for c, peer := range conns {
			if p.ID.Start >= c.Start {
				return Attribution{Conn: c, Peer: peer, How: "inferred"}
			}
		}
	}
	return Attribution{How: "unattributed"}
}
