// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// The login program for a shell the bundle does not have: host (the
// node's own shell) or a login command of the user's (/usr/bin/zsh,
// tmux new -A -s main, …). Bootstrap links the supervisor in as
// /opt/devpod/bin/devpod-login, root's shell in /etc/passwd; sshd runs it
// with no arguments for an interactive login and with -c CMD for
// `ssh … cmd`, scp and sftp.

// loginPath is the login program's path inside the container.
const loginPath = "/opt/devpod/bin/devpod-login"

// loginCommand: the command sshd asks for (-c CMD), "" for an interactive login.
func loginCommand(args []string) string {
	if len(args) >= 2 && args[0] == "-c" {
		return args[1]
	}
	return ""
}

// loginScript is what the bundle's bash runs, with DEVPOD_LOGIN (the shell
// asked for) and DEVPOD_LOGIN_CMD (the command, if any) in its env.
//
//   - host: into the node's namespaces through its PID 1 (nsenter; needs a
//     privileged DevPod sharing the host's PID), its bash or sh; else
//     chroot into the node's / mounted at hostRoot; else, said, a shell in
//     the container. PID 1 is the node's only when its mount namespace is
//     not ours (procRoot): without the host's PIDs it is the container's
//     own. nsenter names the namespaces one by one: the bundle's (BusyBox)
//     has no -a. The bundle's own programs (sftp-server) run in the
//     container: the node does not have them.
//   - a login command: for an interactive login a login bash (its profile
//     read) that becomes the command (customLogin); a command keeps a
//     plain bash, so tools that run commands over ssh still work.
//
// It is POSIX sh: the bundle's bash runs it, tests run it with /bin/sh.
func loginScript(bin, hostRoot, procRoot string) string {
	return `B=` + shQuote(bin) + `; H=` + shQuote(hostRoot) + `; P=` + shQuote(procRoot) + `
want=$DEVPOD_LOGIN
cmd=$DEVPOD_LOGIN_CMD
if [ "$want" = host ]; then
  case "$cmd" in /opt/devpod/*) exec "$B/bash" -c "$cmd" ;; esac
  node='if [ -x /bin/bash ]; then exec /bin/bash "$@"; fi; exec /bin/sh "$@"'
  if [ -n "$cmd" ]; then set -- -c "$cmd"; else set -- -l; fi
  if ! [ "$P/1/ns/mnt" -ef "$P/self/ns/mnt" ] && "$B/nsenter" -t 1 -m -u -i -n -p true 2>/dev/null; then
    exec "$B/nsenter" -t 1 -m -u -i -n -p /bin/sh -c "$node" sh "$@"
  fi
  if [ -x "$H/bin/sh" ]; then exec "$B/chroot" "$H" /bin/sh -c "$node" sh "$@"; fi
  echo "devpod: cannot enter the node (it needs a privileged DevPod sharing the host's PID): a shell in the container" >&2
  exec "$B/bash" "$@"
fi
if [ -n "$cmd" ]; then exec "$B/bash" -c "$cmd"; fi
exec "$B/bash" -l -c ` + shQuote(customLogin) + ` "$B/bash"
`
}

// customLogin is what a login bash runs for an interactive login with a
// command of the user's ($DEVPOD_LOGIN): it becomes the command — exec,
// so the command is the session's own process on the pty, nothing
// between it and sshd — or, the command not found (or not to be run),
// says so and becomes a login bash. $0 is the bundle's bash.
const customLogin = `shopt -s execfail 2>/dev/null
set -f
set -- $DEVPOD_LOGIN
set +f
if command -v "$1" >/dev/null 2>&1; then eval "exec $DEVPOD_LOGIN"; fi
echo "devpod: could not start the login command ($DEVPOD_LOGIN): bash instead" >&2
exec "$0" -l`

// shQuote quotes s for sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runLogin is the supervisor run as devpod-login: it hands over to the
// bundle's bash running loginScript.
func runLogin(args []string) int {
	want := os.Getenv("DEVPOD_ACTIVE_SHELL") // sshd's SetEnv
	if want == "" {
		want = pid1Env("DEVPOD_SHELL")
	}
	env := withEnv(os.Environ(), "DEVPOD_LOGIN", want)
	env = withEnv(env, "DEVPOD_LOGIN_CMD", loginCommand(args))
	bash := "/opt/devpod/bin/bash"
	err := syscall.Exec(bash, []string{"bash", "-c", loginScript("/opt/devpod/bin", "/host", "/proc"), "devpod-login"}, env)
	fmt.Fprintf(os.Stderr, "devpod-login: %s: %v\n", bash, err)
	return 1
}

// withEnv sets k=v in env, replacing any k there.
func withEnv(env []string, k, v string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, k+"=") {
			out = append(out, kv)
		}
	}
	return append(out, k+"="+v)
}

// pid1Env reads one variable of the container's PID 1 (the supervisor),
// for a session whose environment lacks it.
func pid1Env(k string) string {
	b, err := os.ReadFile("/proc/1/environ")
	if err != nil {
		return ""
	}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if v, ok := strings.CutPrefix(string(kv), k+"="); ok {
			return v
		}
	}
	return ""
}
