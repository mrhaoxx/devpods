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
	"syscall"
	"time"
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
	// ReadAt is when CPUNs/RSS/IO were read; an idle process keeps its
	// previous reading (and ReadAt) between re-reads.
	ReadAt time.Time
}

// Scanner reads processes under Root (normally /proc). It is stateful:
// per scan it reads only /proc/<pid>/stat for a process it already knows
// — exe, cmdline, environ and status once per process (and again after
// an exec, which changes comm), io only when the process's CPU time
// moved. A container with ~300 processes scanned twice a second thus
// costs ~600 small reads/s instead of ~5000 (spec §14 item 4). Not safe
// for concurrent Scans.
type Scanner struct {
	Root     string
	PageSize int              // default os.Getpagesize()
	ClkTck   uint64           // default 100
	Clock    func() time.Time // stamps ReadAt; default time.Now

	static  map[PID]static
	dyn     map[PID]dyn
	last    map[int]*Proc // previous result by pid (for ScanNew)
	buf     []byte
	round   int
	changed bool
	lastPid string // last field of /proc/loadavg at the last listing
}

// maxProbe: more new pids than this since the last scan → list /proc.
const maxProbe = 64

// isProcess reports whether pid exists and is a process, not a thread
// (thread ids share the pid range but are not listed in /proc).
func (s *Scanner) isProcess(pid int) bool {
	b := s.small(pid, "status")
	if len(b) == 0 {
		return false
	}
	i := bytes.Index(b, []byte("\nTgid:"))
	if i < 0 {
		return true
	}
	rest := b[i+len("\nTgid:"):]
	if j := bytes.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[:j]
	}
	tgid, err := strconv.Atoi(string(bytes.TrimSpace(rest)))
	return err != nil || tgid == pid
}

// newestPid returns the last pid created in this pid namespace, from
// /proc/loadavg ("" when unavailable).
func (s *Scanner) newestPid() string {
	var buf [256]byte
	b, ok := ReadSmall(filepath.Join(s.Root, "loadavg"), buf[:])
	if !ok {
		return ""
	}
	f := bytes.Fields(b)
	if len(f) < 5 {
		return ""
	}
	return string(f[4])
}

// Changed reports whether the last scan's set of processes (pid and
// start time) differs from the one before: callers skip the work that
// only depends on membership (attribution, events) when it did not.
func (s *Scanner) Changed() bool { return s.changed }

// An idle process (CPU time and RSS unchanged for idleAfter full scans)
// is re-read only every idleEvery-th full scan (its CPU and RSS are then
// up to 10 s old; it was idle): most of a container's
// processes sleep, and the kernel's stat generation is the agent's main
// cost (spec §14 item 4).
const (
	idleAfter = 3
	idleEvery = 10
)

// static is what a process keeps until it execs.
type static struct {
	comm, exe, cmdline, sshConn string
	uid                         int
}

type dyn struct {
	ticks      uint64
	rss        uint64
	ioR, ioW   uint64
	haveCounts bool
	idle       int // consecutive full scans without change
}

// Scan reads every visible process. A process that exits mid-scan is
// skipped; unreadable files leave fields empty.
func (s *Scanner) Scan() (map[PID]*Proc, error) {
	s.lastPid = s.newestPid()
	ents, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	if s.buf == nil {
		s.buf = make([]byte, 4096)
	}
	nextStatic := make(map[PID]static, len(s.static))
	nextDyn := make(map[PID]dyn, len(s.dyn))
	byPid := map[int]*Proc{}
	s.round++
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if prev, ok := s.last[pid]; ok {
			if d := s.dyn[prev.ID]; d.idle >= idleAfter && (s.round+pid)%idleEvery != 0 {
				byPid[pid], nextStatic[prev.ID], nextDyn[prev.ID] = prev, s.static[prev.ID], d
				continue
			}
		}
		if p := s.read(pid, nextStatic, nextDyn); p != nil {
			byPid[pid] = p
		}
	}
	s.static, s.dyn = nextStatic, nextDyn
	return s.result(byPid), nil
}

// ScanNew is the cheap scan between metric passes: it lists the pids and
// reads stat only for those it has not seen; known pids keep their last
// reading (exits show as missing pids; a reused pid is noticed by the
// next Scan).
func (s *Scanner) ScanNew() (map[PID]*Proc, error) {
	if s.last == nil {
		return s.Scan()
	}
	// No process born since the last listing: nothing new to find
	// (listing /proc makes the kernel build a dentry per pid).
	np := s.newestPid()
	if np != "" && np == s.lastPid {
		s.changed = false
		out := make(map[PID]*Proc, len(s.last))
		for _, p := range s.last {
			out[p.ID] = p
		}
		return out, nil
	}
	// Pids are allocated in order: probe just the new ones (a wrap or a
	// burst falls back to listing).
	from, err1 := strconv.Atoi(s.lastPid)
	to, err2 := strconv.Atoi(np)
	if err1 == nil && err2 == nil && to > from && to-from <= maxProbe {
		s.lastPid = np
		byPid := make(map[int]*Proc, len(s.last)+to-from)
		for pid, p := range s.last {
			byPid[pid] = p
		}
		for pid := from + 1; pid <= to; pid++ {
			if _, known := byPid[pid]; known || !s.isProcess(pid) {
				continue
			}
			if p := s.read(pid, s.static, s.dyn); p != nil {
				byPid[pid] = p
			}
		}
		return s.result(byPid), nil
	}
	s.lastPid = np
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
		if p, ok := s.last[pid]; ok {
			byPid[pid] = p
		} else if p := s.read(pid, s.static, s.dyn); p != nil {
			byPid[pid] = p
		}
	}
	return s.result(byPid), nil
}

func (s *Scanner) result(byPid map[int]*Proc) map[PID]*Proc {
	s.changed = s.last == nil || len(s.last) != len(byPid)
	if !s.changed {
		for pid, p := range byPid {
			if old, ok := s.last[pid]; !ok || old.ID != p.ID {
				s.changed = true
				break
			}
		}
	}
	s.last = byPid
	out := make(map[PID]*Proc, len(byPid))
	for _, p := range byPid {
		p.Parent = PID{Pid: p.PPid}
		if parent := byPid[p.PPid]; parent != nil {
			p.Parent = parent.ID
		}
		out[p.ID] = p
	}
	return out
}

func (s *Scanner) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func (s *Scanner) file(pid int, name string) []byte {
	b, err := os.ReadFile(filepath.Join(s.Root, strconv.Itoa(pid), name))
	if err != nil {
		return nil
	}
	return b
}

// small reads a short /proc file into s.buf (see ReadSmall).
func (s *Scanner) small(pid int, name string) []byte {
	b, _ := ReadSmall(filepath.Join(s.Root, strconv.Itoa(pid), name), s.buf)
	return b
}

// ReadSmall reads a small SINGLE-RECORD file (/proc/<pid>/stat, io,
// status, /proc/loadavg, cgroup interface files) into buf with bare
// open/read/close — os.ReadFile adds an fstat, poller registration, an
// allocation and a final read just to see EOF. A short read ends it:
// such files are generated whole and returned by the first read, and a
// regular file reads short only at EOF. Multi-record seq files
// (/proc/net/dev, /proc/net/tcp) may come a page at a time: not for them. Content beyond len(buf)
// is cut. On this agent's hot path syscalls are the cost (spec §14
// item 4).
func ReadSmall(path string, buf []byte) ([]byte, bool) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	defer syscall.Close(fd)
	n := 0
	for n < len(buf) {
		m, err := syscall.Read(fd, buf[n:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil || m <= 0 {
			break
		}
		n += m
		if n < len(buf) {
			break // short read: everything there was
		}
	}
	return buf[:n], true
}

func (s *Scanner) read(pid int, nextStatic map[PID]static, nextDyn map[PID]dyn) *Proc {
	b := s.small(pid, "stat")
	open, close := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || close < open || close+2 > len(b) {
		return nil
	}
	// Fields after "(comm) ", split in place (this runs for every process
	// every second; strings.Fields would allocate ~50 strings each).
	var f [22][]byte // f[0] is field 3 (state) … f[21] is field 24 (rss)
	n, rest := 0, b[close+2:]
	for n < len(f) {
		for len(rest) > 0 && rest[0] == ' ' {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			break
		}
		end := bytes.IndexByte(rest, ' ')
		if end < 0 {
			end = len(rest)
		}
		f[n], rest = bytes.TrimRight(rest[:end], "\n"), rest[end:]
		n++
	}
	if n < len(f) {
		return nil
	}
	num := func(i int) uint64 {
		var v uint64
		for _, c := range f[i] {
			if c < '0' || c > '9' {
				break
			}
			v = v*10 + uint64(c-'0')
		}
		return v
	}
	inum := func(i int) int {
		if len(f[i]) > 0 && f[i][0] == '-' {
			return -1 // tty_nr / ppid are never meaningfully negative here
		}
		return int(num(i))
	}
	comm := string(b[open+1 : close])
	tck := s.ClkTck
	if tck == 0 {
		tck = 100
	}
	ps := s.PageSize
	if ps == 0 {
		ps = os.Getpagesize()
	}
	ticks := num(11) + num(12) // utime + stime
	p := &Proc{
		ID:      PID{Pid: pid, Start: num(19)},
		PPid:    inum(1),
		SID:     inum(3),
		TTY:     inum(4),
		State:   string(f[0]),
		Threads: inum(17),
		CPUNs:   ticks * (1e9 / tck),
		RSS:     num(21) * uint64(ps),
		ReadAt:  s.now(),
	}
	st, known := s.static[p.ID]
	if !known || st.comm != comm {
		st = s.readStatic(pid, comm)
	}
	nextStatic[p.ID] = st
	p.Exe, p.Cmdline, p.SSHConnection, p.UID = st.exe, st.cmdline, st.sshConn, st.uid

	d, seen := s.dyn[p.ID]
	if !seen || !d.haveCounts || d.ticks != ticks {
		d.ioR, d.ioW, d.haveCounts = s.readIO(pid)
	}
	if seen && d.ticks == ticks && d.rss == p.RSS {
		d.idle++
	} else {
		d.idle = 0
	}
	d.ticks, d.rss = ticks, p.RSS
	nextDyn[p.ID] = d
	p.IORead, p.IOWrite = d.ioR, d.ioW
	return p
}

func (s *Scanner) readStatic(pid int, comm string) static {
	st := static{comm: comm}
	if exe, err := os.Readlink(filepath.Join(s.Root, strconv.Itoa(pid), "exe")); err == nil {
		st.exe = exe
	}
	if b := s.file(pid, "cmdline"); len(b) > 0 {
		st.cmdline = strings.TrimSpace(string(bytes.ReplaceAll(bytes.TrimRight(b, "\x00"), []byte{0}, []byte{' '})))
	}
	for _, kv := range bytes.Split(s.file(pid, "environ"), []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("SSH_CONNECTION=")); ok {
			st.sshConn = string(v)
			break
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(s.file(pid, "status")))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			if fs := strings.Fields(v); len(fs) > 0 {
				st.uid, _ = strconv.Atoi(fs[0])
			}
			break
		}
	}
	return st
}

// readIO returns read_bytes / write_bytes; ok is false when io is
// unreadable (another user's process), so it is retried next scan.
func (s *Scanner) readIO(pid int) (r, w uint64, ok bool) {
	b := s.small(pid, "io")
	if len(b) == 0 {
		return 0, 0, false
	}
	field := func(key string) uint64 {
		i := bytes.Index(b, []byte(key))
		if i < 0 {
			return 0
		}
		rest := b[i+len(key):]
		if j := bytes.IndexByte(rest, '\n'); j >= 0 {
			rest = rest[:j]
		}
		v, _ := strconv.ParseUint(string(bytes.TrimSpace(rest)), 10, 64)
		return v
	}
	return field("\nread_bytes:"), field("\nwrite_bytes:"), true
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
