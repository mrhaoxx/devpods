// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

func TestHookRecord_Parse(t *testing.T) {
	recs, rest := splitHookRecords([]byte(
		"77\x1f\x1f\x1f/tmp\x1f100.5\x1f101.25\x1f\x1f1\x1ffalse\x00" +
			"77\x1f\x1f\x1f/root\x1f\x1f\x1f1500\x1f0\x1fprintf 'a\nb'\x1e x\x00" +
			"77\x1fpartial"))
	if len(recs) != 2 || string(rest) != "77\x1fpartial" {
		t.Fatalf("records %q rest %q", recs, rest)
	}
	r, err := parseHookRecord(recs[0])
	if err != nil || r.pid != 77 || r.cwd != "/tmp" || r.exit != 1 || r.cmd != "false" || r.start != 100.5 || r.end != 101.25 {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = parseHookRecord(recs[1])
	if err != nil || r.cmd != "printf 'a\nb'\x1e x" || r.durMs != 1500 || r.start != 0 {
		t.Fatalf("fish-style record: %+v %v", r, err)
	}
}

func runReader(t *testing.T, dir string, alive func(int) bool) (chan wire.Command, chan wire.Alert, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmds, alerts := make(chan wire.Command, 64), make(chan wire.Alert, 8)
	h := &HookReader{Dirs: []string{dir}, BootID: "boot", ProcRoot: t.TempDir(), Sweep: 100 * time.Millisecond}
	go h.Run(ctx, alive, cmds, alerts)
	time.Sleep(100 * time.Millisecond) // watcher up
	return cmds, alerts, cancel
}

func collect(ch chan wire.Command, n int, d time.Duration) []wire.Command {
	var out []wire.Command
	deadline := time.After(d)
	for len(out) < n {
		select {
		case c := <-ch:
			out = append(out, c)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestHooks_RealBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	dir := t.TempDir()
	cmds, _, cancel := runReader(t, dir, func(int) bool { return true })
	defer cancel()
	hook := filepath.Join(t.TempDir(), "hook.bash")
	if err := os.WriteFile(hook, []byte(BashHook), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	c := exec.Command(bash, "--norc", "--noprofile", "-i")
	c.Dir = work
	c.Env = []string{"HOME=" + work, "PATH=/usr/bin:/bin", "DEVPOD_HOOK_DIR=" + dir, "PS1=$ "}
	c.Stdin = strings.NewReader(". " + hook + "\ncd /tmp\nfalse\necho hi > " + filepath.Join(work, "f") + "\n")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("bash: %v: %s", err, out)
	}
	got := collect(cmds, 3, 5*time.Second)
	if len(got) != 3 {
		t.Fatalf("want 3 commands, got %+v", got)
	}
	if got[0].Cmd != "cd /tmp" || got[0].Exit != 0 || !strings.HasSuffix(got[0].Cwd, "/tmp") {
		t.Fatalf("cd: %+v", got[0])
	}
	if got[1].Cmd != "false" || got[1].Exit != 1 {
		t.Fatalf("false: %+v", got[1])
	}
	if !strings.HasPrefix(got[2].Cmd, "echo hi") || got[2].Exit != 0 || got[2].EventID == "" || got[2].EventID == got[1].EventID {
		t.Fatalf("echo: %+v", got[2])
	}
}

func TestHooks_TruncateAndGap(t *testing.T) {
	dir := t.TempDir()
	_, alerts, cancel := runReader(t, dir, func(int) bool { return true })
	defer cancel()
	f := filepath.Join(dir, "4242")
	if err := os.WriteFile(f, []byte(strings.Repeat("x", 2<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-alerts:
		if a.Kind != "gap" {
			t.Fatalf("%+v", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no gap alert for a 2 MiB hook file")
	}
	if st, err := os.Stat(f); err != nil || st.Size() != 0 {
		t.Fatalf("file must be truncated: %v %v", st, err)
	}
}

func TestHooks_DeleteOnShellExit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, strconv.Itoa(5151))
	if err := os.WriteFile(f, []byte("5151\x1f\x1f\x1f/\x1f\x1f\x1f\x1f0\x1fls\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, _, cancel := runReader(t, dir, func(pid int) bool { return pid != 5151 })
	defer cancel()
	if got := collect(cmds, 1, 2*time.Second); len(got) != 1 || got[0].Cmd != "ls" {
		t.Fatalf("pending record must be read before deletion: %+v", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("hook file of an exited shell was not removed")
}
