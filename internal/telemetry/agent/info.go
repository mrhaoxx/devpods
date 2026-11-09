// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// Version is the agent version reported in Info (set with -ldflags).
var Version = "dev"

// topSnapshot is the length of the snapshot's top lists (§6.5).
const topSnapshot = 20

// CollectInfo describes the container (§6.5 static information). fsRoot
// is the container's filesystem root ("/" in production).
func CollectInfo(fsRoot string, cg *Cgroup, gpus []wire.GPU, rdma []RDMADev, collectors map[string]string) wire.Info {
	in := wire.Info{TS: time.Now().UTC(), AgentVersion: Version, Collectors: collectors, Shell: os.Getenv("DEVPOD_SHELL")}
	sc := bufio.NewScanner(strings.NewReader(read(filepath.Join(fsRoot, "etc"), "os-release")))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			in.OS = strings.Trim(v, `"'`)
		}
	}
	in.Kernel = read(filepath.Join(fsRoot, "proc", "sys", "kernel"), "osrelease")
	if cg != nil {
		if s, err := cg.Sample(nil, time.Now()); err == nil {
			in.CPULimit, in.Cpuset, in.MemLimit = s.CPULimit, s.Cpuset, s.MemLimit
		}
	}
	for _, g := range gpus {
		in.GPUs = append(in.GPUs, wire.GPUInfo{UUID: g.UUID, Name: g.Name, MemTotal: g.MemTotal})
	}
	for _, d := range rdma {
		for _, p := range d.Ports {
			in.RDMA = append(in.RDMA, wire.RDMAInfo{IBDev: d.IBDev, Port: p, Scope: d.Scope})
		}
	}
	return in
}

// SnapshotInput is what CollectSnapshot works from.
type SnapshotInput struct {
	ProcRoot  string
	Procs     map[procfs.PID]*procfs.Proc
	Attrib    map[procfs.PID]procfs.Attribution
	CPU       map[procfs.PID]float64 // cores, last interval
	Sockets   *procfs.SocketMap
	PVCMounts []string
	// TCPSocks are /proc/net/tcp and tcp6 by name; nil reads them now
	// (the agent caches them: the read walks the node's TCP table).
	TCPSocks map[string][]procfs.TCPSock
}

// ReadTCPSocks reads /proc/net/tcp and tcp6 under procRoot.
func ReadTCPSocks(procRoot string) map[string][]procfs.TCPSock {
	out := map[string][]procfs.TCPSock{}
	for _, name := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join(procRoot, "net", name))
		if err != nil {
			continue
		}
		socks, _ := procfs.ParseProcNetTCP(f)
		_ = f.Close()
		out[name] = socks
	}
	return out
}

// SnapProcs renders processes as snapshot lines (also used for the
// 5-second full list).
func SnapProcs(in SnapshotInput, ids []procfs.PID) []wire.SnapProc {
	out := make([]wire.SnapProc, 0, len(ids))
	for _, id := range ids {
		p := in.Procs[id]
		sp := wire.SnapProc{Pid: id.Pid, Start: id.Start, PPid: p.PPid, Exe: p.Exe, Cmdline: p.Cmdline, CPU: in.CPU[id], RSS: p.RSS}
		if a, ok := in.Attrib[id]; ok {
			sp.Attr = a.Wire()
		}
		out = append(out, sp)
	}
	return out
}

// CollectSnapshot builds the 15-second overview (§6.5): top processes by
// CPU and RSS, counts, listening ports and PVC usage.
func CollectSnapshot(in SnapshotInput) wire.Snapshot {
	s := wire.Snapshot{TS: time.Now().UTC(), Procs: len(in.Procs)}
	ids := make([]procfs.PID, 0, len(in.Procs))
	for id, p := range in.Procs {
		ids = append(ids, id)
		s.Threads += p.Threads
		if p.State == "Z" {
			s.Zombies++
		}
	}
	top := func(less func(a, b procfs.PID) bool) []wire.SnapProc {
		cp := append([]procfs.PID(nil), ids...)
		sort.Slice(cp, func(i, j int) bool { return less(cp[i], cp[j]) })
		if len(cp) > topSnapshot {
			cp = cp[:topSnapshot]
		}
		return SnapProcs(in, cp)
	}
	s.TopCPU = top(func(a, b procfs.PID) bool { return in.CPU[a] > in.CPU[b] })
	s.TopRSS = top(func(a, b procfs.PID) bool { return in.Procs[a].RSS > in.Procs[b].RSS })
	tcp := in.TCPSocks
	if tcp == nil {
		tcp = ReadTCPSocks(in.ProcRoot)
	}
	for _, name := range []string{"tcp", "tcp6"} {
		for _, sk := range tcp[name] {
			if sk.State != procfs.StateListen {
				continue
			}
			l := wire.Listen{Port: int(sk.Local.Port()), Proto: name, Addr: sk.Local.Addr().String()}
			if in.Sockets != nil {
				l.Pid = in.Sockets.Owner(sk.Inode).Pid
			}
			s.Listen = append(s.Listen, l)
		}
	}
	for _, m := range in.PVCMounts {
		var st unix.Statfs_t
		if unix.Statfs(m, &st) != nil {
			continue
		}
		bs := uint64(st.Bsize)
		s.Storage = append(s.Storage, wire.Storage{Mount: m, Size: uint64(st.Blocks) * bs, Used: (uint64(st.Blocks) - uint64(st.Bfree)) * bs})
	}
	return s
}
