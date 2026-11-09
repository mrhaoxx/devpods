// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package procfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sessExe = "/opt/devpod/libexec/sshd-session"

// base: PID 1 supervisor, sshd 10 (child of 1), legit session 50 (child
// of sshd) with peer 10.0.0.9:41000.
func base(t *testing.T) string {
	root := t.TempDir()
	writeProc(t, root, fp{pid: 1, ppid: 0, sid: 1, start: 1, exe: "/opt/devpod/devpod-supervisor", cmd: "devpod-supervisor"})
	writeProc(t, root, fp{pid: 10, ppid: 1, sid: 10, start: 5, exe: "/opt/devpod/sbin/sshd", cmd: "sshd -D"})
	writeProc(t, root, fp{pid: 50, ppid: 10, sid: 50, start: 100, exe: sessExe, cmd: "sshd-session: root@pts/0"})
	return root
}

var conns = map[PID]string{{Pid: 50, Start: 100}: "10.0.0.9:41000"}

func update(t *testing.T, root string, a *Attributor, c map[PID]string) (map[PID]Attribution, []string) {
	t.Helper()
	procs, err := (&Scanner{Root: root}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	evs, attr := a.Update(procs, c)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Event)
	}
	return attr, kinds
}

func TestAttributor_TreeRule(t *testing.T) {
	root := base(t)
	writeProc(t, root, fp{pid: 60, ppid: 50, sid: 60, tty: 34816, start: 110, exe: "/bin/bash", cmd: "-bash"})
	writeProc(t, root, fp{pid: 61, ppid: 60, sid: 60, tty: 34816, start: 120, exe: "/usr/bin/python3", cmd: "python3 x.py"})
	attr, _ := update(t, root, NewAttributor(10, sessExe), conns)
	for _, pid := range []int{50, 60, 61} {
		a := attr[PID{Pid: pid, Start: map[int]uint64{50: 100, 60: 110, 61: 120}[pid]}]
		if a.How != "tree" || a.Conn != (PID{Pid: 50, Start: 100}) || a.Peer != "10.0.0.9:41000" {
			t.Fatalf("pid %d: %+v", pid, a)
		}
	}
}

func TestAttributor_FakeSessionExeRejected(t *testing.T) {
	root := base(t)
	// right exe, wrong parent (not the supervisor's sshd)
	writeProc(t, root, fp{pid: 70, ppid: 1, sid: 70, start: 130, exe: sessExe, cmd: "sshd-session"})
	writeProc(t, root, fp{pid: 71, ppid: 70, sid: 70, start: 131, exe: "/bin/sh", cmd: "sh"})
	// right parent, wrong exe
	writeProc(t, root, fp{pid: 72, ppid: 10, sid: 72, start: 132, exe: "/tmp/sshd-session", cmd: "sshd-session"})
	writeProc(t, root, fp{pid: 73, ppid: 72, sid: 72, start: 133, exe: "/bin/sh", cmd: "sh"})
	attr, _ := update(t, root, NewAttributor(10, sessExe), map[PID]string{}) // no live conns → no inference
	for _, id := range []PID{{71, 131}, {73, 133}} {
		if attr[id].How == "tree" {
			t.Fatalf("pid %d attributed by tree to a fake session: %+v", id.Pid, attr[id])
		}
	}
}

func TestAttributor_SSHConnectionForDaemonised(t *testing.T) {
	root := base(t)
	// tmux server re-parented to 1 in its own session, environ inherited.
	writeProc(t, root, fp{pid: 80, ppid: 1, sid: 80, start: 140, exe: "/usr/bin/tmux", cmd: "tmux new -d",
		env: []string{"HOME=/root", "SSH_CONNECTION=10.0.0.9 41000 10.1.1.1 2222"}})
	attr, _ := update(t, root, NewAttributor(10, sessExe), conns)
	a := attr[PID{80, 140}]
	if a.How != "ssh_connection" || a.Peer != "10.0.0.9:41000" || a.Conn != (PID{50, 100}) {
		t.Fatalf("%+v", a)
	}
	// The connection is gone later: the peer still identifies the gateway session.
	removeProc(t, root, 50)
	writeProc(t, root, fp{pid: 81, ppid: 1, sid: 81, start: 150, exe: "/usr/bin/nohup", cmd: "nohup x",
		env: []string{"SSH_CONNECTION=10.0.0.9 41000 10.1.1.1 2222"}})
	attr, _ = update(t, root, NewAttributor(10, sessExe), map[PID]string{})
	if a := attr[PID{81, 150}]; a.How != "ssh_connection" || a.Peer != "10.0.0.9:41000" || a.Conn.Pid != 0 {
		t.Fatalf("closed connection: %+v", a)
	}
}

func TestAttributor_StickyAcrossReparentAndPidReuse(t *testing.T) {
	root := base(t)
	writeProc(t, root, fp{pid: 60, ppid: 50, sid: 60, start: 110, exe: "/bin/bash", cmd: "-bash"})
	writeProc(t, root, fp{pid: 90, ppid: 60, sid: 60, start: 160, exe: "/usr/bin/sleep", cmd: "sleep 600"})
	a := NewAttributor(10, sessExe)
	update(t, root, a, conns)
	// bash and the session exit; sleep is re-parented to 1.
	removeProc(t, root, 50)
	removeProc(t, root, 60)
	writeProc(t, root, fp{pid: 90, ppid: 1, sid: 60, start: 160, exe: "/usr/bin/sleep", cmd: "sleep 600"})
	attr, kinds := update(t, root, a, map[PID]string{})
	if got := attr[PID{90, 160}]; got.How != "tree" || got.Peer != "10.0.0.9:41000" {
		t.Fatalf("attribution must stick after reparent: %+v", got)
	}
	if count(kinds, "process_exit") != 2 {
		t.Fatalf("want exits for 50 and 60, got %v", kinds)
	}
	// pid 90 reused by an unrelated process with a new starttime.
	writeProc(t, root, fp{pid: 90, ppid: 1, sid: 90, start: 999, exe: "/usr/bin/cron", cmd: "cron"})
	attr, kinds = update(t, root, a, map[PID]string{})
	if got := attr[PID{90, 999}]; got.How != "unattributed" {
		t.Fatalf("reused pid must not inherit: %+v", got)
	}
	if count(kinds, "process_exit") != 1 || count(kinds, "process_start") != 1 {
		t.Fatalf("pid reuse = exit old + start new, got %v", kinds)
	}
}

func TestAttributor_UnreadableEnvironFallsBack(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads mode-000 files")
	}
	root := base(t)
	writeProc(t, root, fp{pid: 60, ppid: 50, sid: 60, tty: 34816, start: 110, exe: "/bin/bash", cmd: "-bash"})
	// daemonised child in the same session (SID 60) with unreadable environ
	writeProc(t, root, fp{pid: 95, ppid: 1, sid: 60, start: 170, exe: "/usr/bin/redis", cmd: "redis",
		env: []string{"SSH_CONNECTION=10.0.0.9 41000 10.1.1.1 2222"}})
	if err := os.Chmod(filepath.Join(root, "95", "environ"), 0); err != nil {
		t.Fatal(err)
	}
	attr, _ := update(t, root, NewAttributor(10, sessExe), conns)
	if a := attr[PID{95, 170}]; a.How != "sid_tty" || a.Peer != "10.0.0.9:41000" {
		t.Fatalf("SID fallback: %+v", a)
	}
	// unrelated, unreadable, one live connection → inferred
	writeProc(t, root, fp{pid: 96, ppid: 1, sid: 96, start: 180, exe: "/usr/bin/x", cmd: "x"})
	_ = os.Chmod(filepath.Join(root, "96", "environ"), 0)
	attr, _ = update(t, root, NewAttributor(10, sessExe), conns)
	if a := attr[PID{96, 180}]; a.How != "inferred" {
		t.Fatalf("single connection → inferred: %+v", a)
	}
	// no live connection → unattributed
	attr, _ = update(t, root, NewAttributor(10, sessExe), map[PID]string{})
	if a := attr[PID{96, 180}]; a.How != "unattributed" {
		t.Fatalf("no connection → unattributed: %+v", a)
	}
}

func TestAttributor_UntrackedEntry(t *testing.T) {
	root := base(t)
	writeProc(t, root, fp{pid: 77, ppid: 0, sid: 77, start: 190, exe: "/bin/sh", cmd: "sh"})
	a := NewAttributor(10, sessExe)
	_, kinds := update(t, root, a, conns)
	if count(kinds, "untracked_entry") != 1 {
		t.Fatalf("want one untracked_entry, got %v", kinds)
	}
	_, kinds = update(t, root, a, conns)
	if count(kinds, "untracked_entry") != 0 {
		t.Fatalf("untracked_entry repeated: %v", kinds)
	}
}

func TestScanner_PerProcessMetrics(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, fp{pid: 5, ppid: 1, sid: 5, start: 42, exe: "/bin/x", cmd: "x --flag", cpuTicks: 12, rssPages: 10, ioR: 4096, ioW: 8192, uid: 1000, threads: 4, state: "Z"})
	procs, err := (&Scanner{Root: root, PageSize: 4096}).Scan()
	if err != nil {
		t.Fatal(err)
	}
	p := procs[PID{5, 42}]
	if p == nil || p.CPUNs != 120_000_000 || p.RSS != 40960 || p.IORead != 4096 || p.IOWrite != 8192 ||
		p.UID != 1000 || p.Cmdline != "x --flag" || p.Exe != "/bin/x" || p.Threads != 4 || p.State != "Z" || p.Parent.Pid != 1 {
		t.Fatalf("%+v", p)
	}
}

func count(xs []string, s string) int {
	n := 0
	for _, x := range xs {
		if x == s {
			n++
		}
	}
	return n
}

func TestAttributor_InfrastructureIsSystem(t *testing.T) {
	root := base(t)
	// the telemetry agent (pid 20, child of PID 1) and an nvidia-smi it spawned
	writeProc(t, root, fp{pid: 20, ppid: 1, sid: 1, start: 6, exe: "/opt/devpod/devpod-supervisor", cmd: "devpod-supervisor telemetry"})
	writeProc(t, root, fp{pid: 80, ppid: 20, sid: 1, start: 140, exe: "/usr/bin/nvidia-smi", cmd: "nvidia-smi -q"})
	// a user daemon re-parented to PID 1 before the first scan
	writeProc(t, root, fp{pid: 81, ppid: 1, sid: 81, start: 150, exe: "/usr/bin/sleep", cmd: "sleep 1000"})
	a := NewAttributor(10, sessExe)
	a.SelfPid = 20
	attr, _ := update(t, root, a, conns)
	for _, id := range []PID{{1, 1}, {10, 5}, {20, 6}, {80, 140}} {
		if attr[id].How != "system" {
			t.Errorf("%v: infrastructure must be system, got %+v", id, attr[id])
		}
	}
	// one live connection: the re-parented daemon is inferred, not system
	if attr[PID{81, 150}].How != "inferred" {
		t.Fatalf("orphan of PID 1 must not inherit system: %+v", attr[PID{81, 150}])
	}
}

// Each scan reads only stat for an unchanged process: exe/cmdline/environ/
// status once per process (again after an exec, seen as a new comm), io
// only when its CPU time moved (spec §14 item 4: < 2 % of a core).
func TestScanner_ReadsLittlePerScan(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/bin/bash", cmd: "bash", cpuTicks: 1, ioR: 100})
	s := &Scanner{Root: root, PageSize: 4096}
	scan := func() *Proc {
		t.Helper()
		procs, err := s.Scan()
		if err != nil {
			t.Fatal(err)
		}
		return procs[PID{5, 42}]
	}
	if p := scan(); p.Cmdline != "bash" || p.IORead != 100 {
		t.Fatalf("first scan %+v", p)
	}
	dir := filepath.Join(root, "5")
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("cmdline", "changed\x00")
	must("io", "read_bytes: 900\nwrite_bytes: 0\n")
	if p := scan(); p.Cmdline != "bash" || p.IORead != 100 {
		t.Fatalf("idle process re-read: %+v", p)
	}
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/bin/bash", cmd: "bash", cpuTicks: 2, ioR: 900})
	if p := scan(); p.IORead != 900 || p.Cmdline != "bash" {
		t.Fatalf("CPU moved → io re-read, static kept: %+v", p)
	}
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/usr/bin/python3", cmd: "python3 train.py", cpuTicks: 3, ioR: 900})
	if p := scan(); p.Cmdline != "python3 train.py" || p.Exe != "/usr/bin/python3" {
		t.Fatalf("exec (new comm) → static re-read: %+v", p)
	}
}

func TestScanner_ScanNewReadsOnlyNewPids(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/bin/bash", cmd: "bash", cpuTicks: 1})
	s := &Scanner{Root: root}
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/bin/bash", cmd: "bash", cpuTicks: 9})
	writeProc(t, root, fp{pid: 6, ppid: 5, start: 50, exe: "/bin/ls", cmd: "ls"})
	procs, err := s.ScanNew()
	if err != nil {
		t.Fatal(err)
	}
	if p := procs[PID{5, 42}]; p == nil || p.CPUNs != 10_000_000 {
		t.Fatalf("a known pid keeps its last reading in a light scan: %+v", p)
	}
	if p := procs[PID{6, 50}]; p == nil || p.Cmdline != "ls" || p.Parent != (PID{5, 42}) {
		t.Fatalf("a new pid is read: %+v", p)
	}
	if err := os.RemoveAll(filepath.Join(root, "6")); err != nil {
		t.Fatal(err)
	}
	procs, _ = s.ScanNew()
	if _, ok := procs[PID{6, 50}]; ok || len(procs) != 1 {
		t.Fatalf("an exited pid disappears in a light scan: %v", procs)
	}
	procs, _ = s.Scan()
	if p := procs[PID{5, 42}]; p.CPUNs != 90_000_000 {
		t.Fatalf("a full scan refreshes: %+v", p)
	}
}

// A process whose CPU and RSS did not move for 3 full scans is re-read
// only every 10th full scan; ReadAt says when its numbers are from.
func TestScanner_IdleProcessesReadLessOften(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, fp{pid: 7, ppid: 1, start: 70, exe: "/bin/sleep", cmd: "sleep 1000", cpuTicks: 1})
	writeProc(t, root, fp{pid: 8, ppid: 1, start: 80, exe: "/bin/yes", cmd: "yes", cpuTicks: 1})
	now := time.Unix(1000, 0)
	s := &Scanner{Root: root, Clock: func() time.Time { return now }}
	reads := 0
	var sleeper *Proc
	for i := 0; i < 12; i++ {
		now = now.Add(time.Second)
		writeProc(t, root, fp{pid: 8, ppid: 1, start: 80, exe: "/bin/yes", cmd: "yes", cpuTicks: uint64(2 + i)}) // busy
		procs, err := s.Scan()
		if err != nil {
			t.Fatal(err)
		}
		if busy := procs[PID{8, 80}]; !busy.ReadAt.Equal(now) {
			t.Fatalf("scan %d: a busy process is read every scan: %v", i, busy.ReadAt)
		}
		sleeper = procs[PID{7, 70}]
		if sleeper.ReadAt.Equal(now) {
			reads++
		}
	}
	if reads < 3 || reads > 5 {
		t.Fatalf("idle process read %d times in 12 scans, want 3 (to go idle) + ~1", reads)
	}
	// it wakes up: seen within 10 scans, with the read time of that reading
	writeProc(t, root, fp{pid: 7, ppid: 1, start: 70, exe: "/bin/sleep", cmd: "sleep 1000", cpuTicks: 51})
	for i := 0; i < 10; i++ {
		now = now.Add(time.Second)
		procs, _ := s.Scan()
		if p := procs[PID{7, 70}]; p.CPUNs == 510_000_000 {
			if !p.ReadAt.Equal(now) {
				t.Fatalf("ReadAt %v, want %v", p.ReadAt, now)
			}
			return
		}
	}
	t.Fatal("a woken process was not re-read within 10 scans")
}

func TestScanner_ChangedOnMembershipOnly(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/bin/bash", cmd: "bash"})
	s := &Scanner{Root: root}
	if _, _ = s.Scan(); !s.Changed() {
		t.Fatal("first scan is a change")
	}
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/bin/bash", cmd: "bash", cpuTicks: 5})
	if _, _ = s.Scan(); s.Changed() {
		t.Fatal("new numbers are not a membership change")
	}
	writeProc(t, root, fp{pid: 6, ppid: 5, start: 43, exe: "/bin/ls", cmd: "ls"})
	if _, _ = s.ScanNew(); !s.Changed() {
		t.Fatal("a new process is a change")
	}
	writeProc(t, root, fp{pid: 6, ppid: 5, start: 99, exe: "/bin/ls", cmd: "ls"}) // pid reused
	if _, _ = s.Scan(); !s.Changed() {
		t.Fatal("a reused pid is a change")
	}
}

// /proc/loadavg's last field is the last pid created in the reader's pid
// namespace: unchanged means no process was born, so a light scan need
// not list /proc (exits are caught by the next full scan).
func TestScanner_ScanNewSkipsListingWithoutNewPid(t *testing.T) {
	root := t.TempDir()
	loadavg := func(last int) {
		if err := os.WriteFile(filepath.Join(root, "loadavg"), []byte(fmt.Sprintf("0.10 0.20 0.30 1/312 %d\n", last)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/bin/bash", cmd: "bash"})
	loadavg(5)
	s := &Scanner{Root: root}
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	writeProc(t, root, fp{pid: 6, ppid: 5, start: 43, exe: "/bin/ls", cmd: "ls"}) // appears, loadavg not updated
	procs, _ := s.ScanNew()
	if _, seen := procs[PID{6, 43}]; seen || s.Changed() {
		t.Fatal("listed /proc although no pid was created")
	}
	loadavg(6)
	procs, _ = s.ScanNew()
	if _, seen := procs[PID{6, 43}]; !seen || !s.Changed() {
		t.Fatal("a new last pid must trigger a listing")
	}
}

// When pids were created, a light scan probes just the new range
// (pids are allocated in order) instead of listing /proc; thread ids
// share the range and are skipped.
func TestScanner_ScanNewProbesNewPids(t *testing.T) {
	root := t.TempDir()
	loadavg := func(last int) {
		if err := os.WriteFile(filepath.Join(root, "loadavg"), []byte(fmt.Sprintf("0 0 0 1/9 %d\n", last)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProc(t, root, fp{pid: 5, ppid: 1, start: 42, exe: "/bin/bash", cmd: "bash"})
	loadavg(5)
	s := &Scanner{Root: root}
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	writeProc(t, root, fp{pid: 6, ppid: 5, start: 43, exe: "/bin/make", cmd: "make"})
	writeProc(t, root, fp{pid: 7, ppid: 5, start: 44, exe: "/bin/make", cmd: "make", tgid: 6}) // a thread of 6
	writeProc(t, root, fp{pid: 30, ppid: 5, start: 45, exe: "/bin/x", cmd: "x"})               // outside the range
	loadavg(8)
	procs, _ := s.ScanNew()
	_, has6 := procs[PID{6, 43}]
	_, has7 := procs[PID{7, 44}]
	_, has30 := procs[PID{30, 45}]
	if !has6 || has7 || has30 || len(procs) != 2 {
		t.Fatalf("probe (5, 8]: got %v", procs)
	}
	loadavg(200) // a jump: list /proc
	procs, _ = s.ScanNew()
	if _, ok := procs[PID{30, 45}]; !ok {
		t.Fatalf("after a large jump the listing finds everything: %v", procs)
	}
}

func TestReadSmall(t *testing.T) {
	dir := t.TempDir()
	buf := make([]byte, 8)
	for _, tc := range []struct{ body, want string }{{"", ""}, {"abc", "abc"}, {"12345678", "12345678"}, {"123456789", "12345678"}} {
		p := filepath.Join(dir, "f")
		if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := ReadSmall(p, buf)
		if !ok || string(got) != tc.want {
			t.Errorf("%q: got %q %v", tc.body, got, ok)
		}
	}
	if _, ok := ReadSmall(filepath.Join(dir, "missing"), buf); ok {
		t.Error("missing file reported ok")
	}
}
