// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// RDMADev is an IB device visible to this container (spec §6.4).
type RDMADev struct {
	IBDev string
	Ports []int
	Scope string // vf_exclusive | pf_shared
}

// RDMACounters are the per-port counters of the previous sample.
type RDMACounters map[int]RDMAPort

// RDMAPort: a port's cumulative counters as last read, whether it was
// reported (it moved) then, and how many samples it is left unread.
type RDMAPort struct {
	Data     [4]uint64 // xmit bytes, rcv bytes, xmit pkts, rcv pkts
	Health   map[string]uint64
	Reported bool
	Wait     int // samples still to skip before the next read
	Skipped  int // samples skipped since the last read (for the rate)
}

// rdmaIdleEvery: an idle port is read every this many samples.
const rdmaIdleEvery = 10

// rdmaRead reads one counter file (a variable for the tests).
var rdmaRead = readUint

// DiscoverRDMA lists the IB devices behind the container's
// /dev/infiniband/uverbs* nodes; other devices on the host are ignored.
func DiscoverRDMA(devRoot, sysRoot string) ([]RDMADev, error) {
	nodes, _ := filepath.Glob(filepath.Join(devRoot, "infiniband", "uverbs*"))
	var out []RDMADev
	seen := map[string]bool{}
	for _, n := range nodes {
		ib := strings.TrimSpace(read(filepath.Join(sysRoot, "class", "infiniband_verbs", filepath.Base(n)), "ibdev"))
		if ib == "" || seen[ib] {
			continue
		}
		seen[ib] = true
		d := RDMADev{IBDev: ib, Scope: rdmaScope(sysRoot, ib)}
		ents, _ := os.ReadDir(filepath.Join(sysRoot, "class", "infiniband", ib, "ports"))
		for _, e := range ents {
			if p, err := strconv.Atoi(e.Name()); err == nil {
				d.Ports = append(d.Ports, p)
			}
		}
		sort.Ints(d.Ports)
		out = append(out, d)
	}
	return out, nil
}

// rdmaScope: an SR-IOV VF (device/physfn exists) is this container's
// alone; otherwise the PF is shared and counters include other tenants.
func rdmaScope(sysRoot, ib string) string {
	if exists(filepath.Join(sysRoot, "class", "infiniband", ib, "device", "physfn")) {
		return "vf_exclusive"
	}
	return "pf_shared"
}

// Sample reports the ports that moved — their data counters changed since
// they were last read. A node has hundreds of RDMA ports (representors and
// the like), nearly all idle, and each counter read asks the NIC's firmware
// (all of b300's 14 000 counter files take a second): a port that moved is
// read every sample, an idle one every rdmaIdleEvery, and its health
// counters only when it moved. One that goes idle is reported once more,
// at zero. The first sample reports nothing (no rates yet). Data counters
// count 4-byte words.
func (d RDMADev) Sample(sysRoot string, prev *RDMACounters, dt time.Duration) ([]wire.RDMA, RDMACounters, error) {
	cur := RDMACounters{}
	var out []wire.RDMA
	for _, port := range d.Ports {
		var p RDMAPort
		known := false
		if prev != nil {
			p, known = (*prev)[port]
		}
		if known && p.Wait > 0 { // idle, not due yet
			p.Wait--
			p.Skipped++
			cur[port] = p
			continue
		}
		dir := filepath.Join(sysRoot, "class", "infiniband", d.IBDev, "ports", strconv.Itoa(port))
		cnt := filepath.Join(dir, "counters")
		now := RDMAPort{Data: [4]uint64{
			rdmaRead(cnt, "port_xmit_data") * 4, rdmaRead(cnt, "port_rcv_data") * 4,
			rdmaRead(cnt, "port_xmit_packets"), rdmaRead(cnt, "port_rcv_packets"),
		}, Health: p.Health}
		moved := known && now.Data != p.Data
		if moved {
			if ents, err := os.ReadDir(filepath.Join(dir, "hw_counters")); err == nil {
				now.Health = map[string]uint64{}
				for _, e := range ents {
					now.Health[e.Name()] = rdmaRead(filepath.Join(dir, "hw_counters"), e.Name())
				}
			}
		}
		if known && dt > 0 && (moved || p.Reported) {
			r := wire.RDMA{IBDev: d.IBDev, Port: port, Scope: d.Scope, Health: now.Health}
			if moved {
				secs := dt.Seconds() * float64(p.Skipped+1) // since its last read
				rate := func(a, b uint64) float64 {
					if a < b {
						return 0
					}
					return float64(a-b) / secs
				}
				r.TxBps, r.RxBps = rate(now.Data[0], p.Data[0]), rate(now.Data[1], p.Data[1])
				r.TxPps, r.RxPps = rate(now.Data[2], p.Data[2]), rate(now.Data[3], p.Data[3])
			}
			out = append(out, r)
		}
		now.Reported = moved
		if known && !moved {
			now.Wait = rdmaIdleEvery - 1
		}
		cur[port] = now
	}
	return out, cur, nil
}

// ProcRDMA marks processes holding a /dev/infiniband/uverbs* device open
// (spec §6.2: per-process RDMA is "in use", bytes are not split).
func ProcRDMA(root string, procs map[procfs.PID]*procfs.Proc) map[procfs.PID]bool {
	out := map[procfs.PID]bool{}
	procfs.WalkFDs(root, procs, func(id procfs.PID, t string) {
		if isUverbs(t) {
			out[id] = true
		}
	})
	return out
}

func isUverbs(t string) bool { return strings.HasPrefix(t, "/dev/infiniband/uverbs") }
