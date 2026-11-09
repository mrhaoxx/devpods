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

// RDMACounters are the cumulative per-port counters of the previous sample.
type RDMACounters map[int][4]uint64 // port → xmit bytes, rcv bytes, xmit pkts, rcv pkts

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

// Sample reads every port's counters. Data counters count 4-byte words.
func (d RDMADev) Sample(sysRoot string, prev *RDMACounters, dt time.Duration) ([]wire.RDMA, RDMACounters, error) {
	cur := RDMACounters{}
	var out []wire.RDMA
	for _, port := range d.Ports {
		dir := filepath.Join(sysRoot, "class", "infiniband", d.IBDev, "ports", strconv.Itoa(port))
		cnt := filepath.Join(dir, "counters")
		c := [4]uint64{
			readUint(cnt, "port_xmit_data") * 4, readUint(cnt, "port_rcv_data") * 4,
			readUint(cnt, "port_xmit_packets"), readUint(cnt, "port_rcv_packets"),
		}
		cur[port] = c
		r := wire.RDMA{IBDev: d.IBDev, Port: port, Scope: d.Scope}
		if prev != nil && dt > 0 {
			if p, ok := (*prev)[port]; ok {
				secs := dt.Seconds()
				rate := func(a, b uint64) float64 {
					if a < b {
						return 0
					}
					return float64(a-b) / secs
				}
				r.TxBps, r.RxBps = rate(c[0], p[0]), rate(c[1], p[1])
				r.TxPps, r.RxPps = rate(c[2], p[2]), rate(c[3], p[3])
			}
		}
		if ents, err := os.ReadDir(filepath.Join(dir, "hw_counters")); err == nil {
			r.Health = map[string]uint64{}
			for _, e := range ents {
				r.Health[e.Name()] = readUint(filepath.Join(dir, "hw_counters"), e.Name())
			}
		}
		out = append(out, r)
	}
	return out, cur, nil
}

// ProcRDMA marks processes holding a /dev/infiniband/uverbs* device open
// (spec §6.2: per-process RDMA is "in use", bytes are not split).
func ProcRDMA(root string, procs map[procfs.PID]*procfs.Proc) map[procfs.PID]bool {
	out := map[procfs.PID]bool{}
	for id := range procs {
		dir := filepath.Join(root, strconv.Itoa(id.Pid), "fd")
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if t, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && strings.HasPrefix(t, "/dev/infiniband/uverbs") {
				out[id] = true
				break
			}
		}
	}
	return out
}
