// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-supervisor is PID 1 inside a DevPod's user target
// container. It spawns the in-container sshd plus (optionally) the
// user's original command, reaps zombies, forwards signals, and exits
// when either tracked child exits so kubelet restarts the container.
//
// It also exposes a `bootstrap` subcommand used by the initContainer
// to populate the shared binaries emptyDir.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
)

const (
	defaultSshdPath = "/opt/devpod/sbin/sshd"
	defaultSshdArgs = "-D;-e;-f;/opt/devpod/etc/sshd_config"

	defaultBootstrapSrc = "/opt/devpod"
	defaultBootstrapDst = "/devpod-bin"
)

type child struct {
	name string
	cmd  *exec.Cmd
	// exit is non-nil once this child has been reaped; holds the
	// propagated exit code (signal-killed children encode 128+sig).
	exit *int
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "bootstrap" {
		os.Exit(runBootstrap())
	}

	// Register signal handling before forking so a SIGTERM racing
	// startup doesn't terminate PID 1 with the default action.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)

	// OpenSSH's privilege-separation chroot path; must exist before
	// sshd starts. We build with --with-privsep-user=root, so the
	// directory just has to exist (no special ownership needed).
	if err := os.MkdirAll("/var/empty", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "supervisor: mkdir /var/empty: %v\n", err)
	}

	sshdPath := envOr("SUPERVISOR_SSHD_PATH", defaultSshdPath)
	sshdArgs := splitSemis(envOr("SUPERVISOR_SSHD_ARGS", defaultSshdArgs))
	// SUPERVISOR_SSHD_PORT overrides the listen port without
	// touching sshd_config; the controller sets this per-DevPod when
	// the user pod uses hostNetwork (so each DevPod gets a distinct
	// node port).
	if port := os.Getenv("SUPERVISOR_SSHD_PORT"); port != "" {
		sshdArgs = append(sshdArgs, "-p", port)
	}
	// Resolve the shell decision ONCE, then use it to (a) patch
	// /etc/passwd so sshd's pre-auth `pw_shell` check passes, and (b)
	// inject sshd SetEnv flags. Doing both from the same decision
	// avoids the loop where patching the passwd file would make the
	// post-patch resolveShell report the patched path as "chosen".
	// Patching only runs when we're PID 1 (inside the user container);
	// otherwise we'd clobber the host's /etc/passwd in unit tests.
	chosen, force := resolveShell(os.Getenv, defaultPasswdReader, os.Stat)
	if os.Getpid() == 1 {
		if err := patchPasswdRootShell(force, os.Getenv("HOME"), defaultPasswdReader, defaultPasswdWriter); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: patch /etc/passwd: %v\n", err)
		}
		// Make /opt/devpod/bin reachable on PATH after /etc/profile
		// resets it (typical Debian/Ubuntu/Kali behaviour). Non-fatal.
		if err := installProfileScripts(os.MkdirAll, os.WriteFile); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: install profile scripts: %v\n", err)
		}
	}
	sshdArgs = append(sshdArgs, shellArgsForChosen(chosen, containerEnvForSetEnv(os.Environ()), filepath.Glob)...)
	sshdCmd := exec.Command(sshdPath, sshdArgs...)
	sshdCmd.Stdout = os.Stdout
	sshdCmd.Stderr = os.Stderr
	sshdCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sshdCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "supervisor: start sshd %q: %v\n", sshdPath, err)
		os.Exit(1)
	}
	children := []*child{{name: "sshd", cmd: sshdCmd}}

	if userArgs := os.Args[1:]; len(userArgs) > 0 {
		uc := exec.Command(userArgs[0], userArgs[1:]...)
		uc.Stdout = os.Stdout
		uc.Stderr = os.Stderr
		uc.Stdin = os.Stdin
		uc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := uc.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: start user cmd %q: %v\n", userArgs[0], err)
			_ = syscall.Kill(-sshdCmd.Process.Pid, syscall.SIGKILL)
			os.Exit(1)
		}
		children = append(children, &child{name: "user", cmd: uc})
	}

	// exitOnUserCmd controls whether the supervisor exits when the
	// user command exits. Default false (the dev-box-friendly choice):
	// sshd stays up so an operator can ssh in to debug a crashed
	// app. Set SUPERVISOR_EXIT_ON_USER_CMD=true (controller injects
	// it from spec.exitOnUserCommandExit) to revert to first-death-
	// wins, useful for service-style workloads.
	exitOnUserCmd := os.Getenv("SUPERVISOR_EXIT_ON_USER_CMD") == "true"

	// shuttingDown is set the first time we forward a terminal signal.
	// Under shutdown we wait for all tracked children so we can prefer
	// the user command's exit code.
	var shuttingDown atomic.Bool

	go func() {
		for sig := range sigCh {
			shuttingDown.Store(true)
			s := sig.(syscall.Signal)
			for _, c := range children {
				if c.cmd.Process != nil {
					_ = syscall.Kill(-c.cmd.Process.Pid, s)
				}
			}
		}
	}()

	os.Exit(waitForChildren(children, &shuttingDown, exitOnUserCmd))
}

// waitForChildren reaps tracked + orphan children until the supervisor
// decides to exit. Decision rules:
//   - sshd's death always exits the supervisor (it's the only path
//     for new SSH sessions; without it the Pod has no purpose).
//   - user-cmd's death exits the supervisor only when exitOnUserCmd
//     is true (the spec.exitOnUserCommandExit knob). Otherwise we
//     log it and keep waiting for sshd, so an operator can still
//     ssh in and investigate.
//   - During shutdown (we forwarded a terminal signal), wait for all
//     tracked children and prefer the user command's exit code.
func waitForChildren(children []*child, shuttingDown *atomic.Bool, exitOnUserCmd bool) int {
	var firstTrackedExit *int
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.ECHILD) {
				if firstTrackedExit != nil {
					return *firstTrackedExit
				}
				return 0
			}
			return 1
		}
		var hit *child
		for _, c := range children {
			if c.exit == nil && c.cmd.Process != nil && c.cmd.Process.Pid == pid {
				hit = c
				break
			}
		}
		if hit == nil {
			continue // untracked orphan
		}
		code := waitStatusToExit(ws)
		hit.exit = &code
		fmt.Fprintf(os.Stderr, "supervisor: %s (pid %d) exited code=%d\n", hit.name, pid, code)
		if firstTrackedExit == nil {
			firstTrackedExit = &code
		}

		if shuttingDown.Load() {
			if userReaped(children) || allTrackedReaped(children) {
				return userExitOr(children, *firstTrackedExit)
			}
			continue
		}

		// Non-shutdown: decide based on which child died and whether
		// the user opted into following user-cmd exits.
		switch {
		case hit.name == "sshd":
			// sshd is the load-bearing process; always exit so kubelet
			// restarts the container.
			for _, c := range children {
				if c.exit == nil && c.cmd.Process != nil {
					_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
				}
			}
			drainReapsBlocking(children)
			return code
		case hit.name == "user" && exitOnUserCmd:
			// User opted in to "first death wins" semantics.
			for _, c := range children {
				if c.exit == nil && c.cmd.Process != nil {
					_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
				}
			}
			drainReapsBlocking(children)
			return code
		default:
			// User cmd died but follow-user-cmd is off: keep waiting
			// for sshd so debugging stays possible.
			fmt.Fprintf(os.Stderr, "supervisor: user command exited with %d; keeping sshd alive (set spec.exitOnUserCommandExit=true to follow)\n", code)
		}
	}
}

func waitStatusToExit(ws syscall.WaitStatus) int {
	switch {
	case ws.Exited():
		return ws.ExitStatus()
	case ws.Signaled():
		return 128 + int(ws.Signal())
	default:
		return 1
	}
}

func userReaped(children []*child) bool {
	for _, c := range children {
		if c.name == "user" {
			return c.exit != nil
		}
	}
	return false
}

func allTrackedReaped(children []*child) bool {
	for _, c := range children {
		if c.exit == nil {
			return false
		}
	}
	return true
}

func userExitOr(children []*child, fallback int) int {
	for _, c := range children {
		if c.name == "user" && c.exit != nil {
			return *c.exit
		}
	}
	return fallback
}

func drainReapsBlocking(children []*child) {
	for {
		any := false
		for _, c := range children {
			if c.exit == nil && c.cmd.Process != nil {
				any = true
				break
			}
		}
		if !any {
			return
		}
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return
		}
		for _, c := range children {
			if c.exit == nil && c.cmd.Process != nil && c.cmd.Process.Pid == pid {
				code := waitStatusToExit(ws)
				c.exit = &code
				break
			}
		}
	}
}

// runBootstrap recursively copies SUPERVISOR_BOOTSTRAP_SRC (default
// /opt/devpod) to SUPERVISOR_BOOTSTRAP_DST (default /devpod-bin),
// preserving file modes. Used as the initContainer entrypoint.
func runBootstrap() int {
	src := envOr("SUPERVISOR_BOOTSTRAP_SRC", defaultBootstrapSrc)
	dst := envOr("SUPERVISOR_BOOTSTRAP_DST", defaultBootstrapDst)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "supervisor bootstrap: %v\n", err)
		return 1
	}
	return 0
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitSemis(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ";")
}
