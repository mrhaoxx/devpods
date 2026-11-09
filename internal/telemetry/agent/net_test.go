// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent/procfs"
)

func netDev(lo, eth0Rx, eth0Tx uint64) string {
	return "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		fmt.Sprintf("    lo: %d 10 0 0 0 0 0 0 %d 10 0 0 0 0 0 0\n", lo, lo) +
		fmt.Sprintf("  eth0: %d 100 1 2 0 0 0 0 %d 50 0 0 0 0 0 0\n", eth0Rx, eth0Tx)
}

func TestNet_PodUsesNetDevMinusLo(t *testing.T) {
	root := writeTree(t, map[string]string{"net/dev": netDev(5000, 1000, 500)})
	_, c, err := NetTotals(root, false, nil, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, root, "net/dev", netDev(99999, 4000, 1500))
	tcp := map[procfs.PID]procfs.TCPRate{{Pid: 9, Start: 1}: {Rx: 1500, Tx: 200}, {Pid: 10, Start: 2}: {Rx: 500, Tx: 0}}
	s, _, err := NetTotals(root, false, tcp, &c, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.Rx != 3000 || s.Tx != 1000 || s.TCPRx != 2000 || s.OtherRx != 1000 || s.OtherTx != 800 || s.Kind != "pod" {
		t.Fatalf("%+v", s)
	}
}

func TestNet_HostNetworkUsesTCPSum(t *testing.T) {
	// /proc/net/dev in a hostNetwork pod is the host's: huge, unrelated counters.
	root := writeTree(t, map[string]string{"net/dev": netDev(1, 1<<40, 1<<40)})
	_, c, _ := NetTotals(root, true, nil, nil, time.Second)
	writeTreeFile(t, root, "net/dev", netDev(1, 1<<41, 1<<41))
	tcp := map[procfs.PID]procfs.TCPRate{{Pid: 9, Start: 1}: {Rx: 700, Tx: 300}}
	s, _, err := NetTotals(root, true, tcp, &c, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.Rx != 700 || s.Tx != 300 || s.OtherRx != 0 || s.Kind != "tcp-only" {
		t.Fatalf("hostNetwork must report the TCP sum only: %+v", s)
	}
}
