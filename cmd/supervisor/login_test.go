// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin makes bash, nsenter and chroot that print what they were asked;
// nsenter's probe (PROG true) succeeds only when enter is set, and, like
// the bundle's (BusyBox's), it has no -a.
func fakeBin(t *testing.T, enter bool) string {
	t.Helper()
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("bash", `echo "bash $*"; [ "$3" = missing ] && exit 127; exit 0`+"\n")
	probe := "exit 1"
	if enter {
		probe = "exit 0"
	}
	write("nsenter", `skip=
for a; do
  if [ -n "$skip" ]; then skip=; continue; fi
  case "$a" in -a) echo "nsenter: unrecognized option: a" >&2; exit 1 ;; -t) skip=1 ;; -*) ;; *) break ;; esac
done
for last; do :; done
[ "$last" = true ] && { `+probe+`; }
echo "nsenter $*"`+"\n")
	write("chroot", `echo "chroot $*"`+"\n")
	return bin
}

// fakeProc is a /proc whose PID 1 is in a mount namespace of its own (the
// node's, the Pod sharing the host's PIDs) or in this one (the
// container's own PID 1).
func fakeProc(t *testing.T, nodes bool) string {
	t.Helper()
	proc := t.TempDir()
	for _, d := range []string{"1/ns", "self/ns"} {
		if err := os.MkdirAll(filepath.Join(proc, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(proc, "self/ns/mnt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	if nodes {
		err = os.WriteFile(filepath.Join(proc, "1/ns/mnt"), nil, 0o644)
	} else {
		err = os.Link(filepath.Join(proc, "self/ns/mnt"), filepath.Join(proc, "1/ns/mnt"))
	}
	if err != nil {
		t.Fatal(err)
	}
	return proc
}

// login runs the login script as sshd would: no command (an interactive
// login) or -c CMD; PID 1 is the node's.
func login(t *testing.T, bin, hostRoot, want, cmd string) string {
	t.Helper()
	return loginIn(t, bin, hostRoot, fakeProc(t, true), want, cmd)
}

func loginIn(t *testing.T, bin, hostRoot, proc, want, cmd string) string {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", loginScript(bin, hostRoot, proc), "devpod-login")
	c.Env = []string{"PATH=/usr/bin:/bin", "DEVPOD_LOGIN=" + want, "DEVPOD_LOGIN_CMD=" + cmd}
	out, _ := c.CombinedOutput()
	return strings.TrimSpace(string(out))
}

func TestLogin_CustomCommand(t *testing.T) {
	bin := fakeBin(t, false)
	// an interactive login: a login bash that becomes the command (exec)
	if got := login(t, bin, "/nonexistent", "tmux new -A -s main", ""); !strings.HasPrefix(got, "bash -l -c ") || !strings.Contains(got, `exec $DEVPOD_LOGIN`) {
		t.Fatalf("login: %q", got)
	}
	// ssh … cmd (and scp, sftp) keep a plain shell
	if got := login(t, bin, "/nonexistent", "tmux new -A -s main", "ls /"); got != "bash -c ls /" {
		t.Fatalf("command: %q", got)
	}
}

// customLogin replaces itself with the command — it is the session's own
// process, on the pty — or, when it cannot be started, says so and
// replaces itself with bash.
func TestLogin_CustomBecomesTheCommand(t *testing.T) {
	run := func(want string) string {
		bin := fakeBin(t, false)
		c := exec.Command("/bin/sh", "-c", customLogin, filepath.Join(bin, "bash"))
		c.Env = []string{"PATH=/usr/bin:/bin", "DEVPOD_LOGIN=" + want}
		out, _ := c.CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	if got := run(`echo "it is" the command`); got != "it is the command" {
		t.Fatalf("exec: %q", got)
	}
	got := run("missing-program --flag")
	if !strings.Contains(got, "could not start") || !strings.HasSuffix(got, "bash -l") {
		t.Fatalf("fallback: %q", got)
	}
}

func TestLogin_HostEntersTheNode(t *testing.T) {
	bin := fakeBin(t, true)
	got := login(t, bin, "/nonexistent", "host", "")
	if !strings.HasPrefix(got, "nsenter -t 1 -m -u -i -n -p /bin/sh -c ") || !strings.HasSuffix(got, " sh -l") {
		t.Fatalf("login: %q", got)
	}
	if got := login(t, bin, "/nonexistent", "host", "uname -a"); !strings.HasSuffix(got, " sh -c uname -a") || !strings.HasPrefix(got, "nsenter -t 1 -m -u -i -n -p /bin/sh") {
		t.Fatalf("command: %q", got)
	}
	// the bundle's own programs (sftp-server) stay in the container
	if got := login(t, bin, "/nonexistent", "host", "/opt/devpod/libexec/sftp-server"); got != "bash -c /opt/devpod/libexec/sftp-server" {
		t.Fatalf("sftp: %q", got)
	}
}

func TestLogin_HostFallsBack(t *testing.T) {
	bin := fakeBin(t, false) // not privileged: nsenter cannot enter
	host := t.TempDir()
	if err := os.MkdirAll(filepath.Join(host, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "bin", "sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// the node's / mounted: chroot into it
	if got := login(t, bin, host, "host", ""); !strings.HasPrefix(got, "chroot "+host+" /bin/sh -c ") {
		t.Fatalf("chroot: %q", got)
	}
	// neither: said, then the container's bash
	got := login(t, bin, "/nonexistent", "host", "")
	if !strings.Contains(got, "cannot enter the node") || !strings.HasSuffix(got, "bash -l") {
		t.Fatalf("container: %q", got)
	}
}

// Without the host's PIDs, PID 1 is the container's own (the supervisor):
// entering its namespaces is no way onto the node, however privileged.
func TestLogin_HostOwnPID1IsNotTheNode(t *testing.T) {
	bin := fakeBin(t, true)
	got := loginIn(t, bin, "/nonexistent", fakeProc(t, false), "host", "")
	if strings.Contains(got, "nsenter") || !strings.Contains(got, "cannot enter the node") || !strings.HasSuffix(got, "bash -l") {
		t.Fatalf("own PID 1: %q", got)
	}
}

func TestInUserContainer(t *testing.T) {
	for _, c := range []struct {
		pid  int
		exe  string
		want bool
	}{
		{1, "/tmp/x/supervisor", true},                // PID 1
		{4242, "/opt/devpod/devpod-supervisor", true}, // the host's PIDs (hostPID)
		{4242, "/tmp/x/supervisor", false},            // a test's build
	} {
		if got := inUserContainer(c.pid, c.exe); got != c.want {
			t.Errorf("inUserContainer(%d, %q) = %v", c.pid, c.exe, got)
		}
	}
}

func TestLogin_CommandFromArgs(t *testing.T) {
	if got := loginCommand([]string{"-c", "ls -l"}); got != "ls -l" {
		t.Fatalf("-c: %q", got)
	}
	if got := loginCommand(nil); got != "" {
		t.Fatalf("interactive: %q", got)
	}
}
