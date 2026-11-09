// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
)

// NetCounters are the cumulative pod-NIC counters of the previous sample.
type NetCounters struct {
	valid                            bool
	rx, tx, rxPkts, txPkts, errs, dr uint64
}

// NetSample is the container's kernel-stack network throughput (§6.3).
type NetSample struct {
	Rx, Tx           float64
	TCPRx, TCPTx     float64
	OtherRx, OtherTx float64
	RxPkts, TxPkts   float64
	Errs, Drops      float64
	Kind             string // pod | tcp-only
}

// NetTotals computes network rates. A normal pod reads its own NIC
// counters (/proc/net/dev without lo) and reports Other = total − TCP; a
// hostNetwork pod would see the host's NICs there, so it reports the sum
// of its own processes' TCP sockets instead (Kind "tcp-only", UDP
// unavailable until node-agent).
func NetTotals(procRoot string, hostNetwork bool, tcp map[procfs.PID]procfs.TCPRate, prev *NetCounters, dt time.Duration) (NetSample, NetCounters, error) {
	var s NetSample
	for _, r := range tcp {
		s.TCPRx += r.Rx
		s.TCPTx += r.Tx
	}
	if hostNetwork {
		s.Rx, s.Tx, s.Kind = s.TCPRx, s.TCPTx, "tcp-only"
		return s, NetCounters{}, nil
	}
	s.Kind = "pod"
	cur, err := readNetDev(filepath.Join(procRoot, "net", "dev"))
	if err != nil {
		return s, NetCounters{}, err
	}
	if prev != nil && prev.valid && dt > 0 {
		secs := dt.Seconds()
		rate := func(c, p uint64) float64 {
			if c < p {
				return 0
			}
			return float64(c-p) / secs
		}
		s.Rx, s.Tx = rate(cur.rx, prev.rx), rate(cur.tx, prev.tx)
		s.RxPkts, s.TxPkts = rate(cur.rxPkts, prev.rxPkts), rate(cur.txPkts, prev.txPkts)
		s.Errs, s.Drops = rate(cur.errs, prev.errs), rate(cur.dr, prev.dr)
		s.OtherRx, s.OtherTx = max(s.Rx-s.TCPRx, 0), max(s.Tx-s.TCPTx, 0)
	}
	return s, cur, nil
}

func readNetDev(path string) (NetCounters, error) {
	f, err := os.Open(path)
	if err != nil {
		return NetCounters{}, err
	}
	defer f.Close()
	c := NetCounters{valid: true}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) == "lo" || strings.Contains(name, "|") {
			continue
		}
		v := strings.Fields(rest)
		if len(v) < 12 {
			continue
		}
		n := func(i int) uint64 { x, _ := strconv.ParseUint(v[i], 10, 64); return x }
		c.rx += n(0)
		c.rxPkts += n(1)
		c.errs += n(2) + n(10)
		c.dr += n(3) + n(11)
		c.tx += n(8)
		c.txPkts += n(9)
	}
	return c, sc.Err()
}
