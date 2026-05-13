// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"os"
	"strings"
)

// allowedShells are the shells provided by the supervisor bundle.
// CRD enum on DevPod.spec.shell normally enforces this; the supervisor
// re-validates for defense in depth.
var allowedShells = map[string]string{
	"bash": "/opt/devpod/bin/bash",
	"zsh":  "/opt/devpod/bin/zsh",
	"fish": "/opt/devpod/bin/fish",
}

// nologinShells are common "no interactive login" shells that should be
// treated as "no usable shell" by the auto-fallback path.
var nologinShells = map[string]struct{}{
	"/sbin/nologin":     {},
	"/usr/sbin/nologin": {},
	"/bin/false":        {},
	"/usr/bin/false":    {},
}

// shellArgsForChosen returns the sshd `-o` flags that set PATH /
// TERMINFO defaults and report the chosen shell via
// DEVPOD_ACTIVE_SHELL for diagnostics. The chosen string is whatever
// resolveShell decided — short name when forced, /etc/passwd path
// when the user image's shell is being honored.
//
// sshd accepts multiple env entries on ONE SetEnv line (space-
// separated NAME=VALUE pairs). Multiple `-o SetEnv=...` flags are
// NOT additive — only the first wins — so we merge.
func shellArgsForChosen(chosen string) []string {
	// /opt/devpod/bin is appended (not prepended) so the user image's
	// tools win whenever present — busybox / GNU coreutils only fill
	// gaps (typical distroless case). Prepending would shadow Debian's
	// /usr/bin/run-parts with busybox's incompatible run-parts and
	// break Debian's login profile-d sourcing.
	setEnv := strings.Join([]string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/devpod/bin",
		"TERMINFO=/opt/devpod/share/terminfo",
		"TERMINFO_DIRS=/opt/devpod/share/terminfo:/usr/share/terminfo:/etc/terminfo:/lib/terminfo",
		"DEVPOD_ACTIVE_SHELL=" + chosen,
	}, " ")
	return []string{"-o", "SetEnv=" + setEnv}
}

// prepareShellArgs is the convenience entry point used by tests: it
// resolves once and returns the sshd flag slice. Production callers in
// main.go resolve once at startup (so they can patch /etc/passwd with
// the SAME decision) and then call shellArgsForChosen directly.
//
// getenv, passwd, and stat are injected for testability. In production,
// callers pass os.Getenv, defaultPasswdReader, os.Stat.
func prepareShellArgs(
	getenv func(string) string,
	passwd func() ([]byte, error),
	stat func(string) (fs.FileInfo, error),
) []string {
	chosen, _ := resolveShell(getenv, passwd, stat)
	return shellArgsForChosen(chosen)
}

// resolveShell decides what to advertise as the active shell, and
// whether sshd needs a ForceCommand override.
//
// Returns (chosen, forcePath):
//   - chosen is the value to expose via DEVPOD_ACTIVE_SHELL — either a
//     short name (bash|zsh|fish) when we forced the bundle, or the
//     /etc/passwd shell path when we leave sshd's default flow alone.
//   - forcePath is non-empty iff we want sshd to bypass /etc/passwd
//     and exec that binary as the login program.
func resolveShell(
	getenv func(string) string,
	passwd func() ([]byte, error),
	stat func(string) (fs.FileInfo, error),
) (chosen string, forcePath string) {
	if want := getenv("DEVPOD_SHELL"); want != "" {
		if p, ok := allowedShells[want]; ok {
			return want, p
		}
		return "bash", allowedShells["bash"]
	}
	if passwd != nil {
		if data, err := passwd(); err == nil {
			if rootShell := rootShellFromPasswd(data); rootShell != "" {
				if _, blacklisted := nologinShells[rootShell]; !blacklisted {
					if fi, err := stat(rootShell); err == nil && fi.Mode()&0o111 != 0 {
						return rootShell, ""
					}
				}
			}
		}
	}
	return "bash", allowedShells["bash"]
}

// rootShellFromPasswd extracts the shell field of the root entry.
// Returns "" when not found or malformed.
func rootShellFromPasswd(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "root:") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			return ""
		}
		return fields[6]
	}
	return ""
}

// rewriteRootShell returns a new /etc/passwd byte slice with root's
// shell field set to want. If no root line exists, one is prepended.
// The second return value reports whether the file actually changed.
func rewriteRootShell(data []byte, want string) ([]byte, bool) {
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "root:") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		if fields[6] == want {
			return data, false
		}
		fields[6] = want
		lines[i] = strings.Join(fields, ":")
		return []byte(strings.Join(lines, "\n")), true
	}
	// No root line — prepend a synthetic one. We write back at least
	// one trailing newline so sshd's pwd parser doesn't choke on a
	// non-terminated final entry.
	root := "root:x:0:0:root:/root:" + want
	if len(data) == 0 {
		return []byte(root + "\n"), true
	}
	return append([]byte(root+"\n"), data...), true
}

// patchPasswdRootShell rewrites /etc/passwd so root's shell field is
// `want`. sshd validates `pw_shell` existence at pre-auth time —
// without this, a distroless image's `/sbin/nologin` (which is itself
// absent on disk) causes sshd to reject the connection.
//
// No-op when want is empty (caller decided not to force a bundle
// shell).
//
// readPasswd and writePasswd are injected for testability; production
// callers pass defaultPasswdReader and defaultPasswdWriter.
func patchPasswdRootShell(
	want string,
	readPasswd func() ([]byte, error),
	writePasswd func([]byte) error,
) error {
	if want == "" {
		return nil
	}
	data, err := readPasswd()
	if err != nil {
		// No /etc/passwd at all — write a synthetic one so sshd has
		// something to read. Real distroless ships one; this branch
		// covers the FROM-scratch user-image edge case.
		data = nil
	}
	patched, changed := rewriteRootShell(data, want)
	if !changed {
		return nil
	}
	return writePasswd(patched)
}

// defaultPasswdReader is the production passwd source.
func defaultPasswdReader() ([]byte, error) {
	return os.ReadFile("/etc/passwd")
}

// defaultPasswdWriter is the production passwd writer.
func defaultPasswdWriter(data []byte) error {
	return os.WriteFile("/etc/passwd", data, 0o644)
}

// profileShContent is the bash/sh login-shell hook. Distros' /etc/profile
// unconditionally resets PATH, so sshd's SetEnv-injected PATH gets lost
// the moment bash starts as a login shell. Re-adding /opt/devpod/bin
// from /etc/profile.d means our fallback tools are reachable on `ssh`
// interactive sessions on any distro that sources /etc/profile.d.
const profileShContent = `# DevPod supervisor: re-add bundle to PATH after /etc/profile resets it.
case ":${PATH}:" in
    *:/opt/devpod/bin:*) ;;
    *) export PATH="${PATH}:/opt/devpod/bin" ;;
esac
export TERMINFO="${TERMINFO:-/opt/devpod/share/terminfo}"
case ":${TERMINFO_DIRS}:" in
    *:/opt/devpod/share/terminfo:*) ;;
    *) export TERMINFO_DIRS="/opt/devpod/share/terminfo:${TERMINFO_DIRS:-/usr/share/terminfo:/etc/terminfo:/lib/terminfo}" ;;
esac
`

// profileFishContent is the same for fish. fish does NOT source
// /etc/profile; it has its own conf.d.
const profileFishContent = `# DevPod supervisor: ensure bundle is on PATH for fish login shells.
if not contains /opt/devpod/bin $PATH
    set -gx PATH $PATH /opt/devpod/bin
end
set -gx TERMINFO /opt/devpod/share/terminfo
if not contains /opt/devpod/share/terminfo $TERMINFO_DIRS
    set -gx TERMINFO_DIRS /opt/devpod/share/terminfo /usr/share/terminfo /etc/terminfo /lib/terminfo
end
`

// installProfileScripts writes /etc/profile.d/devpod.sh and
// /etc/fish/conf.d/devpod.fish so PATH supplementation survives the
// shell's login-time profile sourcing. Failures are non-fatal — the
// user can still ssh in; only the bundle's `ls`/`fish`/etc. command
// resolution requires this to work.
//
// writeFile and mkdirAll are injected for testability.
func installProfileScripts(
	mkdirAll func(string, os.FileMode) error,
	writeFile func(string, []byte, os.FileMode) error,
) error {
	type entry struct {
		dir, file, body string
	}
	entries := []entry{
		{"/etc/profile.d", "devpod.sh", profileShContent},
		{"/etc/fish/conf.d", "devpod.fish", profileFishContent},
	}
	var firstErr error
	for _, e := range entries {
		if err := mkdirAll(e.dir, 0o755); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		path := e.dir + "/" + e.file
		if err := writeFile(path, []byte(e.body), 0o644); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
