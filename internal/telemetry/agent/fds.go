// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"sort"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
)

// fdEntry is what one process's fds held at its last walk.
type fdEntry struct {
	cpu     uint64 // CPUNs at the walk
	sockets []uint64
	gpus    []int
	rdma    bool
}

// refreshFDs rebuilds the socket map and the GPU / RDMA holders. A
// process is walked again only if it ran since its last walk (CPU time
// moved) or is new: a sleeping process cannot open or close fds.
func (a *agent) refreshFDs(procs map[procfs.PID]*procfs.Proc) {
	next := make(map[procfs.PID]fdEntry, len(procs))
	for id, p := range procs {
		if e, ok := a.fdCache[id]; ok && e.cpu == p.CPUNs {
			next[id] = e
			continue
		}
		e := fdEntry{cpu: p.CPUNs}
		gpus := map[int]bool{}
		a.walkFDs(a.env.ProcRoot, id, func(_ procfs.PID, t string) {
			if ino, ok := procfs.SocketInode(t); ok {
				e.sockets = append(e.sockets, ino)
			} else if m, ok := gpuMinor(t); ok {
				gpus[m] = true
			} else if isUverbs(t) {
				e.rdma = true
			}
		})
		for m := range gpus {
			e.gpus = append(e.gpus, m)
		}
		sort.Ints(e.gpus)
		next[id] = e
	}
	a.fdCache = next
	sm := procfs.NewSocketMap(nil)
	gpuProcs, rdmaProcs := map[procfs.PID][]int{}, map[procfs.PID]bool{}
	for id, e := range next {
		for _, ino := range e.sockets {
			sm.AddInode(id, ino)
		}
		if len(e.gpus) > 0 {
			gpuProcs[id] = e.gpus
		}
		if e.rdma {
			rdmaProcs[id] = true
		}
	}
	a.sockets, a.gpuProcs, a.rdmaProcs = sm, gpuProcs, rdmaProcs
}
