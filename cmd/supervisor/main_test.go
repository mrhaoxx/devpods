// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func buildSupervisor(t *testing.T) string {
	t.Helper()
	out := t.TempDir() + "/supervisor"
	cmd := exec.Command("go", "build", "-o", out, "github.com/mrhaoxx/devpod/cmd/supervisor")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, b)
	}
	return out
}

func TestSupervisor_ExitsWhenChildExitsZero_FollowEnabled(t *testing.T) {
	sup := buildSupervisor(t)
	cmd := exec.Command(sup, "/usr/bin/true")
	cmd.Env = append(cmd.Environ(),
		// sh -c 'sleep 60' -- ignores trailing -o flags the supervisor
		// appends from prepareShellArgs (real sshd accepts them; sleep
		// doesn't, so we wrap to tolerate the extras).
		"SUPERVISOR_SSHD_PATH=/bin/sh", "SUPERVISOR_SSHD_ARGS=-c;exec sleep 60;--",
		"SUPERVISOR_EXIT_ON_USER_CMD=true",
	)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("supervisor exit code = %d, want 0", ee.ExitCode())
		}
		t.Fatalf("run supervisor: %v", err)
	}
}

func TestSupervisor_PropagatesChildNonzeroExit_FollowEnabled(t *testing.T) {
	sup := buildSupervisor(t)
	cmd := exec.Command(sup, "/bin/sh", "-c", "exit 7")
	cmd.Env = append(cmd.Environ(),
		"SUPERVISOR_SSHD_PATH=/bin/sh", "SUPERVISOR_SSHD_ARGS=-c;exec sleep 60;--",
		"SUPERVISOR_EXIT_ON_USER_CMD=true",
	)
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T %v", err, err)
	}
	if ee.ExitCode() != 7 {
		t.Errorf("exit = %d, want 7", ee.ExitCode())
	}
}

// Default (follow disabled): user cmd dies, supervisor stays up
// waiting for sshd. Verify by running both with short timeouts and
// asserting the supervisor outlives the user cmd.
func TestSupervisor_DefaultIgnoresUserCmdExit(t *testing.T) {
	sup := buildSupervisor(t)
	cmd := exec.Command(sup, "/usr/bin/true")
	cmd.Env = append(cmd.Environ(),
		// sshd-stub: sleep 2 so we know it dies AFTER user-cmd dies.
		// sh -c wrapper tolerates the -o overrides appended by
		// prepareShellArgs.
		"SUPERVISOR_SSHD_PATH=/bin/sh", "SUPERVISOR_SSHD_ARGS=-c;exec sleep 2;--",
		// no SUPERVISOR_EXIT_ON_USER_CMD → default false
	)
	start := time.Now()
	err := cmd.Run()
	dt := time.Since(start)
	// Supervisor should survive user cmd's instant exit and only return
	// after sshd-stub's 2s sleep ends. Exit code = sshd's exit (signaled
	// or 0 depending on stub's behavior; we assert timing instead).
	if dt < 1500*time.Millisecond {
		t.Errorf("supervisor exited in %v; expected to wait for sshd (~2s)", dt)
	}
	_ = err // exit code is sshd-side, not interesting here
}

func TestSupervisor_NoUserCommand_RunsOnlySshd(t *testing.T) {
	sup := buildSupervisor(t)
	cmd := exec.Command(sup) // no user args
	cmd.Env = append(cmd.Environ(), "SUPERVISOR_SSHD_PATH=/bin/sh", "SUPERVISOR_SSHD_ARGS=-c;echo sshd-marker")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("supervisor: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "sshd-marker") {
		t.Errorf("stdout missing sshd marker: %s", out)
	}
}

func TestSupervisor_ForwardsSIGTERM(t *testing.T) {
	// Production use is PID 1 in a Linux container. macOS BSD signal
	// semantics + Go runtime's TTY foreground-group handling make this
	// test flaky on darwin in ways that don't reflect the real path.
	// The real PID-1 SIGTERM forwarding gets validated by hack/e2e-v2.sh.
	if runtime.GOOS != "linux" {
		t.Skipf("SIGTERM forwarding test only valid on linux; see hack/e2e-v2.sh for live coverage")
	}
	sup := buildSupervisor(t)
	script := "trap 'exit 42' TERM; sleep 60 & wait"
	cmd := exec.Command(sup, "/bin/sh", "-c", script)
	cmd.Env = append(cmd.Environ(),
		"SUPERVISOR_SSHD_PATH=/bin/sh", "SUPERVISOR_SSHD_ARGS=-c;exec sleep 60;--",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	err := cmd.Wait()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T %v", err, err)
	}
	if ee.ExitCode() != 42 {
		t.Errorf("exit = %d, want 42 (SIGTERM-handled)", ee.ExitCode())
	}
}

func TestSupervisor_Bootstrap_CopiesTree(t *testing.T) {
	sup := buildSupervisor(t)
	src := t.TempDir()
	dst := t.TempDir()
	// Create a fake /opt/devpod tree at src.
	if err := os.MkdirAll(filepath.Join(src, "sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sbin", "fake-sshd"), []byte("#!/bin/sh\necho hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "etc", "sshd_config"), []byte("Port 2222\n"), 0o644); err != nil {
		// etc dir doesn't exist; create.
		_ = os.MkdirAll(filepath.Join(src, "etc"), 0o755)
		if err2 := os.WriteFile(filepath.Join(src, "etc", "sshd_config"), []byte("Port 2222\n"), 0o644); err2 != nil {
			t.Fatal(err2)
		}
	}
	cmd := exec.Command(sup, "bootstrap")
	cmd.Env = append(cmd.Environ(),
		"SUPERVISOR_BOOTSTRAP_SRC="+src,
		"SUPERVISOR_BOOTSTRAP_DST="+dst,
	)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap: %v: %s", err, b)
	}
	// Verify files copied.
	if _, err := os.Stat(filepath.Join(dst, "sbin", "fake-sshd")); err != nil {
		t.Errorf("fake-sshd not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "etc", "sshd_config")); err != nil {
		t.Errorf("sshd_config not copied: %v", err)
	}
	// Verify mode preserved on the executable.
	if st, err := os.Stat(filepath.Join(dst, "sbin", "fake-sshd")); err == nil {
		if st.Mode()&0o111 == 0 {
			t.Errorf("fake-sshd lost exec mode: %v", st.Mode())
		}
	}
}

// TestSupervisor_ForwardsShellOverridesToSshd uses a fake sshd
// (printenv-style) to assert the supervisor invokes sshd with the
// -o SetEnv / ForceCommand flags computed from DEVPOD_SHELL.
func TestSupervisor_ForwardsShellOverridesToSshd(t *testing.T) {
	sup := buildSupervisor(t)
	// Fake sshd: print all our args to stdout and exit 0.
	cmd := exec.Command(sup) // no user args
	cmd.Env = append(cmd.Environ(),
		"DEVPOD_SHELL=zsh",
		// Echo argv via /bin/sh; the supervisor will append -o flags
		// after the configured sshd args. We use sh -c "echo $@" as
		// the fake sshd binary so its stdout reveals what sshd would
		// have received.
		"SUPERVISOR_SSHD_PATH=/bin/sh",
		`SUPERVISOR_SSHD_ARGS=-c;printf '%s\n' "$@";--`,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("supervisor: %v: %s", err, out)
	}
	s := string(out)
	for _, want := range []string{
		"-o",
		"DEVPOD_ACTIVE_SHELL=zsh",
		":/opt/devpod/bin",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("stdout missing %q\nfull:\n%s", want, s)
		}
	}
	if strings.Contains(s, "ForceCommand=") {
		t.Errorf("stdout unexpectedly contains ForceCommand= — chosen shell lands via /etc/passwd, not ForceCommand:\n%s", s)
	}
}
