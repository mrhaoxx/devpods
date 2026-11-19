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
// nsenter's probe (`-- true`) succeeds only when enter is set.
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
	write("nsenter", `[ "$5" = true ] && { `+probe+`; }; echo "nsenter $*"`+"\n")
	write("chroot", `echo "chroot $*"`+"\n")
	return bin
}

// login runs the login script as sshd would: no command (an interactive
// login) or -c CMD.
func login(t *testing.T, bin, hostRoot, want, cmd string) string {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", loginScript(bin, hostRoot), "devpod-login")
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
	if !strings.HasPrefix(got, "nsenter -t 1 -a -- /bin/sh -c ") || !strings.HasSuffix(got, " sh -l") {
		t.Fatalf("login: %q", got)
	}
	if got := login(t, bin, "/nonexistent", "host", "uname -a"); !strings.HasSuffix(got, " sh -c uname -a") || !strings.HasPrefix(got, "nsenter -t 1 -a --") {
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

func TestLogin_CommandFromArgs(t *testing.T) {
	if got := loginCommand([]string{"-c", "ls -l"}); got != "ls -l" {
		t.Fatalf("-c: %q", got)
	}
	if got := loginCommand(nil); got != "" {
		t.Fatalf("interactive: %q", got)
	}
}
