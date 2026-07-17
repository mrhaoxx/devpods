// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"os"
	"path/filepath"
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
// TERMINFO defaults, report the chosen shell via DEVPOD_ACTIVE_SHELL,
// and forward container environment variables to SSH sessions.
//
// sshd accepts multiple env entries on ONE SetEnv line (space-
// separated NAME=VALUE pairs). Multiple `-o SetEnv=...` flags are
// NOT additive — only the first wins — so we merge.
//
// containerEnv should be the filtered output of containerEnvForSetEnv;
// pass nil to skip forwarding (e.g. in tests).
//
// glob resolves the bundled zsh functions directory (production callers
// pass filepath.Glob); it is injected for testability.
func shellArgsForChosen(chosen string, containerEnv []string, glob func(string) ([]string, error)) []string {
	// /opt/devpod/bin is appended (not prepended) so the user image's
	// tools win whenever present — busybox / GNU coreutils only fill
	// gaps (typical distroless case). Prepending would shadow Debian's
	// /usr/bin/run-parts with busybox's incompatible run-parts and
	// break Debian's login profile-d sourcing.
	envPairs := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/devpod/bin",
		"TERMINFO=/opt/devpod/share/terminfo",
		"TERMINFO_DIRS=/opt/devpod/share/terminfo:/usr/share/terminfo:/etc/terminfo:/lib/terminfo",
	}
	// Only set FPATH when the bundled functions dir is actually present.
	// FPATH overrides zsh's compiled-in fpath, which already points at
	// the bundle — so a wrong or bundle-less FPATH breaks every autoload
	// (add-zsh-hook, is-at-least, ...) rather than merely adding nothing.
	if dir := bundledZshFunctionsDir(glob); dir != "" {
		envPairs = append(envPairs,
			"FPATH="+dir+":/usr/share/zsh/functions:/usr/local/share/zsh/site-functions")
	}
	envPairs = append(envPairs, "DEVPOD_ACTIVE_SHELL="+chosen)
	envPairs = append(envPairs, containerEnv...)
	setEnv := strings.Join(envPairs, " ")
	return []string{"-o", "SetEnv=" + setEnv}
}

// zshFunctionsGlob matches the zsh functions directory bundled in the
// supervisor image. The version segment is whatever the Dockerfile's
// ZSH_VERSION produced — deliberately NOT hardcoded here, because the
// two live in different files and silently drift apart (a hardcoded
// "5.9" survived the Dockerfile's bump to 5.9.1 and broke autoload).
const zshFunctionsGlob = "/opt/devpod/share/zsh/*/functions"

// bundledZshFunctionsDir returns the bundled zsh functions directory,
// or "" when the bundle is absent. The image ships exactly one version
// directory; when several somehow match, the last of filepath.Glob's
// sorted output is taken purely for determinism (lexicographic, not
// semver).
func bundledZshFunctionsDir(glob func(string) ([]string, error)) string {
	matches, err := glob(zshFunctionsGlob)
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
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
	return shellArgsForChosen(chosen, nil, filepath.Glob)
}

// skipEnvExact lists environment variable names managed by the
// supervisor or Kubernetes runtime that must not be forwarded.
var skipEnvExact = map[string]bool{
	"PATH": true, "TERMINFO": true, "TERMINFO_DIRS": true,
	"FPATH": true, "HOME": true, "HOSTNAME": true,
	"PWD": true, "SHLVL": true, "OLDPWD": true, "_": true,
	"TERM": true,
}

// containerEnvForSetEnv filters os.Environ()-style KEY=VALUE entries,
// removing supervisor-managed and Kubernetes-internal variables, and
// returns the remainder as SetEnv-safe entries (values with spaces are
// double-quoted for sshd's parser).
func containerEnvForSetEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if skipEnvExact[k] {
			continue
		}
		if strings.HasPrefix(k, "SUPERVISOR_") ||
			strings.HasPrefix(k, "DEVPOD_") ||
			strings.HasPrefix(k, "KUBERNETES_") {
			continue
		}
		if strings.ContainsAny(v, "\n\r") {
			continue
		}
		out = append(out, quoteSetEnvPair(k, v))
	}
	return out
}

// quoteSetEnvPair formats a single SetEnv entry KEY=VALUE, wrapping
// the value in double quotes when it contains spaces or quotes so
// sshd's space-delimited parser doesn't split it.
func quoteSetEnvPair(k, v string) string {
	if !strings.ContainsAny(v, " \t\"\\") {
		return k + "=" + v
	}
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "\"", "\\\"")
	return k + "=\"" + v + "\""
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

// rewriteRootPasswd returns a new /etc/passwd byte slice with root's
// shell field (index 6) set to wantShell and, when wantHome is non-empty,
// home directory field (index 5) set to wantHome. If no root line exists,
// one is prepended.
func rewriteRootPasswd(data []byte, wantShell, wantHome string) ([]byte, bool) {
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "root:") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		changed := false
		if wantShell != "" && fields[6] != wantShell {
			fields[6] = wantShell
			changed = true
		}
		if wantHome != "" && fields[5] != wantHome {
			fields[5] = wantHome
			changed = true
		}
		if !changed {
			return data, false
		}
		lines[i] = strings.Join(fields, ":")
		return []byte(strings.Join(lines, "\n")), true
	}
	home := wantHome
	if home == "" {
		home = "/root"
	}
	shell := wantShell
	if shell == "" {
		shell = "/bin/bash"
	}
	root := "root:x:0:0:root:" + home + ":" + shell
	if len(data) == 0 {
		return []byte(root + "\n"), true
	}
	return append([]byte(root+"\n"), data...), true
}

// patchPasswdRootShell rewrites /etc/passwd so root's shell field is
// `wantShell` and (when non-empty) root's home directory field is
// `wantHome`. sshd validates `pw_shell` existence at pre-auth time —
// without this, a distroless image's `/sbin/nologin` (which is itself
// absent on disk) causes sshd to reject the connection. The home
// field ensures sshd sets HOME correctly for login shells.
//
// No-op when both wantShell and wantHome are empty.
func patchPasswdRootShell(
	wantShell string,
	wantHome string,
	readPasswd func() ([]byte, error),
	writePasswd func([]byte) error,
) error {
	if wantShell == "" && wantHome == "" {
		return nil
	}
	data, err := readPasswd()
	if err != nil {
		data = nil
	}
	patched, changed := rewriteRootPasswd(data, wantShell, wantHome)
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
