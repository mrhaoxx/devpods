// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package procfs

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TCP states as printed in /proc/net/tcp.
const (
	StateEstablished uint8 = 0x01
	StateListen      uint8 = 0x0A
)

// TCPSock is one line of /proc/<pid>/net/tcp{,6}.
type TCPSock struct {
	Inode         uint64
	Local, Remote netip.AddrPort
	State         uint8
}

// ParseProcNetTCP parses /proc/net/tcp or tcp6 (v4-mapped addresses are
// normalised to v4, matching sshd's SSH_CONNECTION).
func ParseProcNetTCP(r io.Reader) ([]TCPSock, error) {
	var out []TCPSock
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		local, err := parseHexAddr(f[1])
		if err != nil {
			return nil, err
		}
		remote, err := parseHexAddr(f[2])
		if err != nil {
			return nil, err
		}
		st, _ := strconv.ParseUint(f[3], 16, 8)
		inode, _ := strconv.ParseUint(f[9], 10, 64)
		out = append(out, TCPSock{Inode: inode, Local: local, Remote: remote, State: uint8(st)})
	}
	return out, sc.Err()
}

// parseHexAddr decodes "0100007F:1F90": each 32-bit word of the address
// is printed in host (little-endian) byte order.
func parseHexAddr(s string) (netip.AddrPort, error) {
	ipHex, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("bad address %q", s)
	}
	raw, err := hex.DecodeString(ipHex)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, fmt.Errorf("bad address %q", s)
	}
	for i := 0; i < len(raw); i += 4 {
		binary.LittleEndian.PutUint32(raw[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("bad port %q", s)
	}
	addr, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), nil
}

// SocketMap maps socket inodes to the process holding them.
type SocketMap struct {
	byInode map[uint64]PID
	byPID   map[PID][]uint64
}

// NewSocketMap builds a map from inode owners (tests, callers that
// already know the mapping).
func NewSocketMap(byInode map[uint64]PID) *SocketMap {
	m := &SocketMap{byInode: map[uint64]PID{}, byPID: map[PID][]uint64{}}
	for ino, id := range byInode {
		m.byInode[ino] = id
		m.byPID[id] = append(m.byPID[id], ino)
	}
	return m
}

// Owner returns the process holding inode (zero PID when unknown).
func (m *SocketMap) Owner(inode uint64) PID { return m.byInode[inode] }

// Inodes returns the socket inodes held by p.
func (m *SocketMap) Inodes(p PID) []uint64 { return m.byPID[p] }

// BuildSocketMap reads /proc/<pid>/fd links of every process. Unreadable
// fd directories (another user's process) are skipped.
func BuildSocketMap(root string, procs map[PID]*Proc) (*SocketMap, error) {
	m := NewSocketMap(nil)
	WalkFDs(root, procs, m.Add)
	return m, nil
}

// CountedTCP reports whether a TCP socket carries network traffic: a
// loopback connection would be counted once per end (and a forward
// through sshd three times).
func CountedTCP(src, dst net.IP) bool {
	return !src.IsLoopback() && !dst.IsLoopback()
}

// SocketInode parses an fd target "socket:[inode]".
func SocketInode(t string) (uint64, bool) {
	v, ok := strings.CutPrefix(t, "socket:[")
	if !ok {
		return 0, false
	}
	inode, err := strconv.ParseUint(strings.TrimSuffix(v, "]"), 10, 64)
	return inode, err == nil
}

// Add records fd target t ("socket:[inode]") of process id; other
// targets are ignored.
func (m *SocketMap) Add(id PID, t string) {
	if inode, ok := SocketInode(t); ok {
		m.AddInode(id, inode)
	}
}

// AddInode records that process id holds socket inode (the first holder
// owns it).
func (m *SocketMap) AddInode(id PID, inode uint64) {
	if _, dup := m.byInode[inode]; !dup {
		m.byInode[inode] = id
	}
	m.byPID[id] = append(m.byPID[id], inode)
}

// WalkFDs calls visit with every readable fd target of every process:
// one pass over /proc/*/fd serves the socket map and the GPU and RDMA
// holders.
func WalkFDs(root string, procs map[PID]*Proc, visit func(id PID, target string)) {
	for id := range procs {
		WalkProcFDs(root, id, visit)
	}
}

// WalkProcFDs calls visit with each readable fd target of process id.
func WalkProcFDs(root string, id PID, visit func(id PID, target string)) {
	dir := filepath.Join(root, strconv.Itoa(id.Pid), "fd")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
			visit(id, target)
		}
	}
}

// SSHConns returns each session's established TCP peer, read from that
// process's own /proc/<pid>/net view (its network namespace, so
// hostNetwork works too).
func SSHConns(root string, sm *SocketMap, sessions []PID) map[PID]string {
	out := map[PID]string{}
	for _, s := range sessions {
		held := map[uint64]bool{}
		for _, ino := range sm.Inodes(s) {
			held[ino] = true
		}
		if len(held) == 0 {
			continue
		}
		for _, name := range []string{"tcp", "tcp6"} {
			f, err := os.Open(filepath.Join(root, strconv.Itoa(s.Pid), "net", name))
			if err != nil {
				continue
			}
			socks, _ := ParseProcNetTCP(f)
			_ = f.Close()
			for _, sk := range socks {
				if held[sk.Inode] && sk.State == StateEstablished {
					out[s] = sk.Remote.String()
				}
			}
			if _, ok := out[s]; ok {
				break
			}
		}
	}
	return out
}

// TCPBytes are a socket's cumulative byte counters (tcpi_bytes_*).
type TCPBytes struct {
	Acked, Received uint64
}

// TCPStats returns cumulative per-socket TCP counters keyed by inode.
type TCPStats interface {
	Bytes() (map[uint64]TCPBytes, error)
}

// TCPRate is a process's TCP throughput in bytes per second.
type TCPRate struct {
	Rx, Tx float64
}

// PerProcessTCP turns two counter snapshots into per-process rates. A
// socket new in cur was opened within the interval and counts whole; a
// socket missing from cur was closed and counts nothing (spec §6.3's
// known limitation). Without prev there are no rates.
func PerProcessTCP(prev, cur map[uint64]TCPBytes, sm *SocketMap, dt time.Duration) map[PID]TCPRate {
	out := map[PID]TCPRate{}
	if prev == nil || dt <= 0 {
		return out
	}
	secs := dt.Seconds()
	for ino, c := range cur {
		owner, ok := sm.byInode[ino]
		if !ok {
			continue
		}
		p := prev[ino]
		r := out[owner]
		if c.Acked >= p.Acked {
			r.Tx += float64(c.Acked-p.Acked) / secs
		}
		if c.Received >= p.Received {
			r.Rx += float64(c.Received-p.Received) / secs
		}
		out[owner] = r
	}
	return out
}
