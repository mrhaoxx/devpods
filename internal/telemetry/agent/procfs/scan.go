// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package procfs reads the container's /proc: processes, their metrics,
// sockets, and attributes processes to SSH connections (spec §5.3–5.4).
package procfs

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PID identifies a process for its lifetime: pids are reused, starttime
// (clock ticks since boot) disambiguates.
type PID struct {
	Pid   int
	Start uint64
}

// Proc is one process as seen in one scan. Fields that could not be read
// (exited, EACCES under hidepid or a non-root container user) are empty.
type Proc struct {
	ID            PID
	Parent        PID // Parent.Start is 0 when the parent is not visible
	PPid          int
	Exe           string
	Cmdline       string
	UID           int
	SID, TTY      int
	State         string
	Threads       int
	SSHConnection string // from environ, "" when absent or unreadable
	CPUNs         uint64
	RSS, PSS      uint64
	IORead        uint64
	IOWrite       uint64
}

// Scanner reads processes under Root (normally /proc).
type Scanner struct {
	Root     string
	PageSize int    // default os.Getpagesize()
	ClkTck   uint64 // default 100
}

// Scan reads every visible process. A process that exits mid-scan is
// skipped; unreadable files leave fields empty.
func (s *Scanner) Scan() (map[PID]*Proc, error) {
	ents, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	byPid := map[int]*Proc{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if p := s.read(pid); p != nil {
			byPid[pid] = p
		}
	}
	out := make(map[PID]*Proc, len(byPid))
	for _, p := range byPid {
		p.Parent = PID{Pid: p.PPid}
		if parent := byPid[p.PPid]; parent != nil {
			p.Parent = parent.ID
		}
		out[p.ID] = p
	}
	return out, nil
}

func (s *Scanner) file(pid int, name string) []byte {
	b, err := os.ReadFile(filepath.Join(s.Root, strconv.Itoa(pid), name))
	if err != nil {
		return nil
	}
	return b
}

func (s *Scanner) read(pid int) *Proc {
	stat := string(s.file(pid, "stat"))
	open, close := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || close < open || close+2 > len(stat) {
		return nil
	}
	f := strings.Fields(stat[close+2:]) // f[0] is field 3 (state)
	if len(f) < 20 {
		return nil
	}
	num := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
	inum := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
	p := &Proc{
		ID:      PID{Pid: pid, Start: num(19)},
		PPid:    inum(1),
		SID:     inum(3),
		TTY:     inum(4),
		State:   f[0],
		Threads: inum(17),
	}
	if exe, err := os.Readlink(filepath.Join(s.Root, strconv.Itoa(pid), "exe")); err == nil {
		p.Exe = exe
	}
	if b := s.file(pid, "cmdline"); len(b) > 0 {
		p.Cmdline = strings.TrimSpace(string(bytes.ReplaceAll(bytes.TrimRight(b, "\x00"), []byte{0}, []byte{' '})))
	}
	for _, kv := range bytes.Split(s.file(pid, "environ"), []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("SSH_CONNECTION=")); ok {
			p.SSHConnection = string(v)
			break
		}
	}
	if b := s.file(pid, "schedstat"); b != nil {
		if fs := strings.Fields(string(b)); len(fs) > 0 {
			p.CPUNs, _ = strconv.ParseUint(fs[0], 10, 64)
		}
	} else {
		tck := s.ClkTck
		if tck == 0 {
			tck = 100
		}
		p.CPUNs = (num(11) + num(12)) * (1e9 / tck)
	}
	if fs := strings.Fields(string(s.file(pid, "statm"))); len(fs) > 1 {
		pages, _ := strconv.ParseUint(fs[1], 10, 64)
		ps := s.PageSize
		if ps == 0 {
			ps = os.Getpagesize()
		}
		p.RSS = pages * uint64(ps)
	}
	sc := bufio.NewScanner(bytes.NewReader(s.file(pid, "io")))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ": ")
		if !ok {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		switch k {
		case "read_bytes":
			p.IORead = n
		case "write_bytes":
			p.IOWrite = n
		}
	}
	sc = bufio.NewScanner(bytes.NewReader(s.file(pid, "status")))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			if fs := strings.Fields(v); len(fs) > 0 {
				p.UID, _ = strconv.Atoi(fs[0])
			}
			break
		}
	}
	return p
}

// PSS reads smaps_rollup (expensive; the agent calls it every 10 s for
// the top RSS processes only).
func (s *Scanner) PSS(pid int) uint64 {
	sc := bufio.NewScanner(bytes.NewReader(s.file(pid, "smaps_rollup")))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Pss:"); ok {
			fs := strings.Fields(v)
			if len(fs) > 0 {
				kb, _ := strconv.ParseUint(fs[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}
