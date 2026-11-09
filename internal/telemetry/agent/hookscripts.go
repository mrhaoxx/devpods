// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

// Shell command hooks (spec §5.1). They use shell builtins only, so no
// process is forked per command, and append one record per command to
// <hook dir>/<shell pid>:
//
//	pid ␟ shell_start ␟ tty ␟ cwd ␟ start ␟ end ␟ dur_ms ␟ exit ␟ cmd ␀
//
// Fields a shell cannot produce with builtins (shell_start, tty, and the
// times in fish) stay empty; the agent fills them from /proc. Hooks are
// only installed when a hook directory is writable — the agent creates
// it when hooks are enabled — or DEVPOD_HOOK_DIR is set.

// HookDirs are the hook directories in preference order.
var HookDirs = []string{"/run/devpod/hooks", "/tmp/.devpod/hooks"}

// ProfileHookLoader is appended to /etc/profile.d/devpod.sh. That file
// is parsed by dash too, so it stays POSIX and sources the per-shell
// files (which /etc/profile.d's *.sh glob does not pick up itself).
const ProfileHookLoader = `
# DevPod: per-shell command hooks for the session audit.
if [ -n "${BASH_VERSION-}" ] && [ -r /etc/profile.d/devpod-hook.bash ]; then
    . /etc/profile.d/devpod-hook.bash
elif [ -n "${ZSH_VERSION-}" ] && [ -r /etc/profile.d/devpod-hook.zsh ]; then
    emulate zsh -c '. /etc/profile.d/devpod-hook.zsh'
fi
`

// BashHook: DEBUG trap captures the command, PROMPT_COMMAND writes it.
const BashHook = `# DevPod: record interactive commands for the session audit (builtins only).
if [ -n "${BASH_VERSION-}" ] && [ -z "${__devpod_hook-}" ] && [ "${-#*i}" != "$-" ]; then
    if [ -n "${DEVPOD_HOOK_DIR-}" ]; then __devpod_hdir=$DEVPOD_HOOK_DIR
    elif [ -w /run/devpod/hooks ]; then __devpod_hdir=/run/devpod/hooks
    elif [ -w /tmp/.devpod/hooks ]; then __devpod_hdir=/tmp/.devpod/hooks
    else __devpod_hdir=
    fi
    if [ -n "$__devpod_hdir" ]; then
        __devpod_hook=1
        __devpod_cmd=
        __devpod_t0=
        __devpod_prompt=1
        __devpod_pre() {
            [ -n "$__devpod_prompt" ] && return 0
            case $BASH_COMMAND in __devpod_post*|__devpod_ready*) return 0 ;; esac
            if [ -z "$__devpod_cmd" ]; then
                __devpod_t0=${EPOCHREALTIME-}
                __devpod_cmd=$BASH_COMMAND
            elif [ "${#__devpod_cmd}" -lt 4096 ]; then
                __devpod_cmd="$__devpod_cmd; $BASH_COMMAND"
            fi
            return 0
        }
        __devpod_post() {
            local __devpod_s=$?
            __devpod_prompt=1
            if [ -n "$__devpod_cmd" ]; then
                builtin printf '%s\037\037\037%s\037%s\037%s\037\037%s\037%s\000' "$$" "$PWD" "$__devpod_t0" "${EPOCHREALTIME-}" "$__devpod_s" "$__devpod_cmd" >>"$__devpod_hdir/$$" 2>/dev/null
                __devpod_cmd=
            fi
            return $__devpod_s
        }
        __devpod_ready() {
            local __devpod_s=$?
            __devpod_prompt=
            return $__devpod_s
        }
        trap '__devpod_pre' DEBUG
        PROMPT_COMMAND="__devpod_post${PROMPT_COMMAND:+;$PROMPT_COMMAND};__devpod_ready"
    fi
fi
`

// ZshHook: preexec gets the full command line, precmd writes it.
const ZshHook = `# DevPod: record interactive commands for the session audit (builtins only).
if [[ -n ${ZSH_VERSION-} && -z ${__devpod_hook-} && -o interactive ]]; then
    if [[ -n ${DEVPOD_HOOK_DIR-} ]]; then typeset -g __devpod_hdir=$DEVPOD_HOOK_DIR
    elif [[ -w /run/devpod/hooks ]]; then typeset -g __devpod_hdir=/run/devpod/hooks
    elif [[ -w /tmp/.devpod/hooks ]]; then typeset -g __devpod_hdir=/tmp/.devpod/hooks
    else typeset -g __devpod_hdir=
    fi
    if [[ -n $__devpod_hdir ]]; then
        typeset -g __devpod_hook=1 __devpod_cmd= __devpod_t0=
        zmodload zsh/datetime 2>/dev/null
        __devpod_pre() { __devpod_t0=${EPOCHREALTIME-}; __devpod_cmd=$1 }
        __devpod_post() {
            local __devpod_s=$?
            if [[ -n $__devpod_cmd ]]; then
                builtin printf '%s\037\037\037%s\037%s\037%s\037\037%s\037%s\000' "$$" "$PWD" "$__devpod_t0" "${EPOCHREALTIME-}" "$__devpod_s" "$__devpod_cmd" >>"$__devpod_hdir/$$" 2>/dev/null
                __devpod_cmd=
            fi
            return $__devpod_s
        }
        typeset -ga preexec_functions precmd_functions
        preexec_functions+=(__devpod_pre)
        precmd_functions=(__devpod_post $precmd_functions)
    fi
fi
`

// FishHook is appended to /etc/fish/conf.d/devpod.fish.
const FishHook = `
# DevPod: record interactive commands for the session audit (builtins only).
if status is-interactive; and not set -q __devpod_hook
    if set -q DEVPOD_HOOK_DIR
        set -g __devpod_hdir $DEVPOD_HOOK_DIR
    else if test -w /run/devpod/hooks
        set -g __devpod_hdir /run/devpod/hooks
    else if test -w /tmp/.devpod/hooks
        set -g __devpod_hdir /tmp/.devpod/hooks
    end
    if set -q __devpod_hdir
        set -g __devpod_hook 1
        function __devpod_post --on-event fish_postexec
            set -l s $status
            test -n "$argv[1]"; or return $s
            builtin printf '%s\x1f\x1f\x1f%s\x1f\x1f\x1f%s\x1f%s\x1f%s\x00' $fish_pid $PWD $CMD_DURATION $s "$argv[1]" >>$__devpod_hdir/$fish_pid 2>/dev/null
            return $s
        end
    end
end
`
