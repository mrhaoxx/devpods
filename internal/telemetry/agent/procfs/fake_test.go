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
)

// fp describes one fake /proc/<pid>.
type fp struct {
	pid, ppid, sid, tty int
	start               uint64
	exe, cmd            string
	env                 []string
	cpuTicks, rssPages  uint64 // utime+stime, rss — both from stat
	ioR, ioW            uint64
	uid                 int
	threads             int
	state               string
	tgid                int // 0 = pid (a process); else a thread of tgid
	pgrp                int // 0 = pid
}

func writeProc(t *testing.T, root string, p fp) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(p.pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if p.state == "" {
		p.state = "S"
	}
	if p.threads == 0 {
		p.threads = 1
	}
	comm := filepath.Base(p.exe)
	tgid := p.tgid
	if tgid == 0 {
		tgid = p.pid
	}
	pgrp := p.pgrp
	if pgrp == 0 {
		pgrp = p.pid
	}
	// pid (comm) state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice num_threads
	// itrealvalue starttime …
	stat := fmt.Sprintf("%d (%s) %s %d %d %d %d -1 0 0 0 0 0 %d 0 0 0 20 0 %d 0 %d 0 %d\n",
		p.pid, comm, p.state, p.ppid, pgrp, p.sid, p.tty, p.cpuTicks, p.threads, p.start, p.rssPages)
	files := map[string]string{
		"stat":    stat,
		"cmdline": strings.ReplaceAll(p.cmd, " ", "\x00") + "\x00",
		"environ": strings.Join(p.env, "\x00"),
		"io":      fmt.Sprintf("rchar: 1\nwchar: 1\nread_bytes: %d\nwrite_bytes: %d\n", p.ioR, p.ioW),
		"status":  fmt.Sprintf("Name:\t%s\nTgid:\t%d\nUid:\t%d\t%d\t%d\t%d\n", comm, tgid, p.uid, p.uid, p.uid, p.uid),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p.exe != "" {
		_ = os.Remove(filepath.Join(dir, "exe"))
		if err := os.Symlink(p.exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
}

func removeProc(t *testing.T, root string, pid int) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, fmt.Sprint(pid))); err != nil {
		t.Fatal(err)
	}
}
