// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package procfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestParseProcNetTCP_V4V6(t *testing.T) {
	v4 := tcpHeader +
		"   0: 00000000:08AE 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 111 1 0000000000000000 100 0 0 10 0\n" +
		"   1: 0101010A:08AE 0900000A:A028 01 00000000:00000000 00:00000000 00000000     0        0 999 1 0000000000000000 20 4 30 10 -1\n"
	socks, err := ParseProcNetTCP(strings.NewReader(v4))
	if err != nil || len(socks) != 2 {
		t.Fatalf("%v %v", socks, err)
	}
	if socks[0].State != StateListen || socks[0].Local.Port() != 2222 || socks[0].Inode != 111 {
		t.Fatalf("listen: %+v", socks[0])
	}
	if socks[1].Remote.String() != "10.0.0.9:41000" || socks[1].Local.String() != "10.1.1.1:2222" || socks[1].State != StateEstablished {
		t.Fatalf("established: %+v", socks[1])
	}
	v6 := tcpHeader +
		"   0: 0000000000000000FFFF00000101010A:08AE 0000000000000000FFFF00000900000A:A028 01 00000000:00000000 00:00000000 00000000     0        0 555 1\n"
	socks, err = ParseProcNetTCP(strings.NewReader(v6))
	if err != nil || len(socks) != 1 || socks[0].Remote.String() != "10.0.0.9:41000" {
		t.Fatalf("v4-mapped v6 must normalise to v4: %+v %v", socks, err)
	}
}

func fdLink(t *testing.T, root string, pid, fd int, target string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid), "fd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, fmt.Sprint(fd))); err != nil {
		t.Fatal(err)
	}
}

func TestSSHConns_PeerFromSessionNetns(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, fp{pid: 50, ppid: 10, sid: 50, start: 100, exe: "/opt/devpod/libexec/sshd-session", cmd: "sshd-session"})
	writeProc(t, root, fp{pid: 60, ppid: 50, sid: 60, start: 110, exe: "/bin/bash", cmd: "bash"})
	fdLink(t, root, 50, 3, "socket:[999]")
	fdLink(t, root, 50, 4, "/dev/null")
	fdLink(t, root, 60, 0, "/dev/pts/0")
	netDir := filepath.Join(root, "50", "net")
	_ = os.MkdirAll(netDir, 0o755)
	_ = os.WriteFile(filepath.Join(netDir, "tcp"), []byte(tcpHeader+
		"   0: 0101010A:08AE 0900000A:A028 01 00000000:00000000 00:00000000 00000000     0        0 999 1\n"+
		"   1: 0101010A:08AE 0A00000A:A029 01 00000000:00000000 00:00000000 00000000     0        0 1000 1\n"), 0o644)
	procs, _ := (&Scanner{Root: root}).Scan()
	sm, err := BuildSocketMap(root, procs)
	if err != nil {
		t.Fatal(err)
	}
	if sm.Owner(999) != (PID{50, 100}) {
		t.Fatalf("inode 999 owner: %+v", sm.Owner(999))
	}
	got := SSHConns(root, sm, []PID{{50, 100}})
	if len(got) != 1 || got[PID{50, 100}] != "10.0.0.9:41000" {
		t.Fatalf("%v", got)
	}
}

func TestPerProcessTCP_Deltas(t *testing.T) {
	sm := &SocketMap{byInode: map[uint64]PID{7: {9, 1}, 8: {9, 1}, 9: {10, 2}}}
	prev := map[uint64]TCPBytes{7: {Acked: 1000, Received: 500}, 9: {Acked: 50}}
	cur := map[uint64]TCPBytes{7: {Acked: 3000, Received: 1500}, 8: {Acked: 400, Received: 100}}
	got := PerProcessTCP(prev, cur, sm, time.Second)
	// inode 7: +2000 tx, +1000 rx; inode 8 opened within the interval: counted whole;
	// inode 9 closed: contributes nothing.
	if r := got[PID{9, 1}]; r.Tx != 2400 || r.Rx != 1100 {
		t.Fatalf("pid 9: %+v", r)
	}
	if _, ok := got[PID{10, 2}]; ok {
		t.Fatalf("closed socket must contribute nothing: %+v", got)
	}
	if first := PerProcessTCP(nil, cur, sm, time.Second); len(first) != 0 {
		t.Fatalf("no previous sample → no rates, got %+v", first)
	}
}
