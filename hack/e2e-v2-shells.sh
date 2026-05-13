#!/usr/bin/env bash
# Prove the shell bundle: a distroless user image with no shell of its
# own still yields working ssh sessions, and spec.shell selects the
# bundle shell. Verifies bash / zsh / fish + fallback path.
#
# Prereqs: hack/e2e-up.sh already run (cluster up, supervisor image
# loaded, gateway service exposed).

set -euo pipefail

NS=devpods
OWNER=alice
GW_PORT=2222
KEY="$(cat /tmp/devpod-test-key-path)"

cleanup() {
    pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
    for n in s-bash s-zsh s-fish s-fallback; do
        kubectl -n "$NS" delete devpod "$n" --ignore-not-found --wait=false 2>/dev/null || true
    done
}
trap cleanup EXIT

start_port_forward() {
    pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
    sleep 1
    kubectl -n devpod-system port-forward svc/devpod-gateway "$GW_PORT:22" >/dev/null 2>&1 &
    local deadline=$((SECONDS + 30))
    until nc -z 127.0.0.1 "$GW_PORT" 2>/dev/null; do
        [[ $SECONDS -lt $deadline ]] || { echo "FAIL: port-forward never came up"; exit 1; }
        sleep 1
    done
}

ssh_run() {
    local name="$1"; shift
    ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -p "$GW_PORT" -i "$KEY" "$OWNER+$name@127.0.0.1" -- "$@"
}

apply_devpod() {
    local name="$1"; local shell="$2"
    local shell_field=""
    [[ -n "$shell" ]] && shell_field="  shell: $shell"
    cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: $name, namespace: $NS}
spec:
  owner: $OWNER
  running: true
${shell_field}
  pod:
    spec:
      containers:
      - name: dev
        image: gcr.io/distroless/static-debian12
        # No command — supervisor runs only sshd; the user container
        # has no /bin/sh of its own (that's the point).
EOF
    kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$name" --timeout=180s
}

verify_shell() {
    local name="$1"; local expected="$2"
    local out
    out=$(ssh_run "$name" 'env')
    if ! grep -q "^DEVPOD_ACTIVE_SHELL=$expected$" <<<"$out"; then
        echo "FAIL: $name expected DEVPOD_ACTIVE_SHELL=$expected, got:"
        grep DEVPOD_ACTIVE_SHELL <<<"$out" || echo "  (no DEVPOD_ACTIVE_SHELL in env)"
        return 1
    fi
    # Coreutils + PATH wiring: ls of bundle dir must succeed and show
    # several known binaries.
    out=$(ssh_run "$name" 'ls /opt/devpod/bin')
    for binary in bash zsh fish busybox; do
        if ! grep -q "^$binary$" <<<"$out"; then
            echo "FAIL: $name missing /opt/devpod/bin/$binary"
            return 1
        fi
    done
    # Terminfo: assert the xterm-256color entry is reachable via the
    # canonical TERMINFO path. We don't shell out to tput because
    # busybox doesn't ship it and the bundle deliberately omits
    # ncurses-utils — the terminfo data is what we ship, and any
    # ncurses-linked binary in the user's image finds it via the
    # TERMINFO / TERMINFO_DIRS env we set in sshd_config.
    out=$(ssh_run "$name" 'ls /opt/devpod/share/terminfo/x/xterm-256color')
    if [[ "$out" != "/opt/devpod/share/terminfo/x/xterm-256color" ]]; then
        echo "FAIL: $name terminfo db missing for xterm-256color, got: $out"
        return 1
    fi
    echo "OK: $name DEVPOD_ACTIVE_SHELL=$expected, /opt/devpod/bin populated, terminfo reachable"
}

start_port_forward

echo "[1/4] DevPod with spec.shell=bash"
apply_devpod s-bash bash
verify_shell s-bash bash

echo "[2/4] DevPod with spec.shell=zsh"
apply_devpod s-zsh zsh
verify_shell s-zsh zsh

echo "[3/4] DevPod with spec.shell=fish"
apply_devpod s-fish fish
verify_shell s-fish fish

echo "[4/4] DevPod with spec.shell unset — auto-fallback to bash"
apply_devpod s-fallback ""
verify_shell s-fallback bash

echo
echo "OK: shell bundle demo passed — distroless + bash/zsh/fish/fallback."
