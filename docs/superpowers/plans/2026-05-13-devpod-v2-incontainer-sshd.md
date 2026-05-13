# DevPod v2 — In-container sshd Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. **Each Agent dispatch (implementer + reviewers) MUST pass `model=opus`.**

**Goal:** Materialize the v2 spec —
`docs/superpowers/specs/2026-05-13-devpod-v2-incontainer-sshd.md`.
Drop the sidecar container entirely; sshd runs in the user
container's namespaces via a PID-1 supervisor.

**Architecture:** 10 tasks in dependency order. T1-T2 build the new
binaries / image. T3-T6 rewire the controller / render / webhook to
emit the new Pod shape. T7-T8 wire the chart. T9 ships an e2e that
proves SFTP works (the bug that motivated v2). T10 deletes the old
sidecar code.

**Tech Stack:** Go 1.22 (supervisor), OpenSSH 10.0p2 portable
(static-pie musl, built ourselves from OpenBSD CDN tarball), Docker
buildx for arm64 + amd64, Helm 3, controller-runtime v0.20.

---

## File structure

| File | Action | Task |
|------|--------|------|
| `cmd/supervisor/main.go` | NEW | T1 |
| `cmd/supervisor/main_test.go` | NEW | T1 |
| `images/supervisor/Dockerfile` | NEW | T2 |
| `images/supervisor/sshd_config` | NEW | T2 |
| `api/v1alpha1/gatewayconfig_types.go` | modify | T3 |
| `api/v1alpha1/zz_generated.deepcopy.go` | regen | T3 |
| `config/crd/bases/devpod.io_gatewayconfigs.yaml` | regen | T3 |
| `deploy/chart/templates/crds/...` | regen via T2 sync | T3 |
| `internal/render/pod.go` | rewrite | T4 |
| `internal/render/pod_test.go` | rewrite | T4 |
| `internal/render/render.go` | drop sidecar consts | T4 |
| `internal/render/service.go` | targetPort 2222 | T4 |
| `internal/render/service_test.go` | adapt | T4 |
| `internal/webhook/devpod_webhook.go` | drop sidecar-name + SPN rules | T5 |
| `internal/webhook/devpod_webhook_test.go` | adapt | T5 |
| `internal/controllers/devpod_controller.go` | endpoint port from GwConfig | T6 |
| `internal/controllers/devpod_controller_test.go` | adapt | T6 |
| `cmd/controller/main.go` | flag rename | T7 |
| `deploy/chart/values.yaml` | image rename | T7 |
| `deploy/chart/templates/controller.yaml` | flag rename | T7 |
| `hack/e2e-up.sh` | build supervisor image | T8 |
| `hack/e2e-v2.sh` | NEW | T9 |
| `cmd/sidecar/` | DELETE | T10 |
| `images/sidecar/` | DELETE | T10 |

---

### Task 1: cmd/supervisor — PID-1 supervisor binary

**Files:**
- Create: `cmd/supervisor/main.go`
- Create: `cmd/supervisor/main_test.go`

- [ ] **Step 1: Write the test scaffolding**

`cmd/supervisor/main_test.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main_test

import (
	"os/exec"
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

func TestSupervisor_ExitsWhenChildExitsZero(t *testing.T) {
	sup := buildSupervisor(t)
	// Use /usr/bin/true as the user cmd. Supervisor must propagate its
	// exit code 0 ... once it also kills the sshd it tried to spawn.
	// Here, sshd doesn't exist; the supervisor's sshd-spawn attempt
	// MUST be invoked via SUPERVISOR_SSHD_PATH so the test can point
	// it at /usr/bin/sleep 60.
	cmd := exec.Command(sup, "/usr/bin/true")
	cmd.Env = append(cmd.Environ(), "SUPERVISOR_SSHD_PATH=/bin/sleep", "SUPERVISOR_SSHD_ARGS=60")
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("supervisor exit code = %d, want 0", ee.ExitCode())
		}
		t.Fatalf("run supervisor: %v", err)
	}
}

func TestSupervisor_PropagatesChildNonzeroExit(t *testing.T) {
	sup := buildSupervisor(t)
	cmd := exec.Command(sup, "/bin/sh", "-c", "exit 7")
	cmd.Env = append(cmd.Environ(), "SUPERVISOR_SSHD_PATH=/bin/sleep", "SUPERVISOR_SSHD_ARGS=60")
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T %v", err, err)
	}
	if ee.ExitCode() != 7 {
		t.Errorf("exit = %d, want 7", ee.ExitCode())
	}
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
	sup := buildSupervisor(t)
	// User cmd: sleep 60 in a shell that catches SIGTERM and exits 42.
	script := `trap 'exit 42' TERM; sleep 60 & wait`
	cmd := exec.Command(sup, "/bin/sh", "-c", script)
	cmd.Env = append(cmd.Environ(), "SUPERVISOR_SSHD_PATH=/bin/sleep", "SUPERVISOR_SSHD_ARGS=60")
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
```

The test uses env vars `SUPERVISOR_SSHD_PATH` and
`SUPERVISOR_SSHD_ARGS` (semicolon-separated) so tests can substitute a
stub for the sshd binary. Production code defaults to
`/opt/devpod/sbin/sshd` and a fixed argv.

- [ ] **Step 2: Run, see fail**

```bash
bash hack/test.sh ./cmd/supervisor/...
```

Expected: build error (no main.go yet).

- [ ] **Step 3: Implement**

`cmd/supervisor/main.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-supervisor is PID 1 inside a DevPod's user target
// container. It spawns the in-container sshd plus (optionally) the
// user's original command, reaps zombies, forwards signals, and exits
// when either tracked child exits so kubelet restarts the container.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defaultSshdPath = "/opt/devpod/sbin/sshd"
	defaultSshdArgs = "-D;-e;-f;/opt/devpod/etc/sshd_config"
)

// child tags a tracked process so the reap loop can identify which one died.
type child struct {
	name string
	cmd  *exec.Cmd
}

func main() {
	sshdPath := envOr("SUPERVISOR_SSHD_PATH", defaultSshdPath)
	sshdArgsRaw := envOr("SUPERVISOR_SSHD_ARGS", defaultSshdArgs)
	sshdArgs := splitSemis(sshdArgsRaw)

	// 1. Spawn sshd.
	sshdCmd := exec.Command(sshdPath, sshdArgs...)
	sshdCmd.Stdout = os.Stdout
	sshdCmd.Stderr = os.Stderr
	sshdCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sshdCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "supervisor: start sshd %q: %v\n", sshdPath, err)
		os.Exit(1)
	}
	children := []*child{{name: "sshd", cmd: sshdCmd}}

	// 2. Spawn user command, if any.
	userArgs := os.Args[1:]
	var userCmd *exec.Cmd
	if len(userArgs) > 0 {
		userCmd = exec.Command(userArgs[0], userArgs[1:]...)
		userCmd.Stdout = os.Stdout
		userCmd.Stderr = os.Stderr
		userCmd.Stdin = os.Stdin
		userCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := userCmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "supervisor: start user cmd %q: %v\n", userArgs[0], err)
			_ = killAll(children)
			os.Exit(1)
		}
		children = append(children, &child{name: "user", cmd: userCmd})
	}

	// 3. Signal-forwarding goroutine.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for sig := range sigCh {
			fwdSig := sig.(syscall.Signal)
			for _, c := range children {
				if c.cmd.Process != nil {
					_ = syscall.Kill(-c.cmd.Process.Pid, fwdSig)
				}
			}
		}
	}()

	// 4. Reap loop. Exit when a tracked child exits; reap orphans
	//    silently.
	exitCode := waitTrackedExit(children)

	// 5. Tear down whatever's left.
	_ = gracefulShutdown(children)
	os.Exit(exitCode)
}

// waitTrackedExit blocks until any tracked child exits and returns its
// exit code. Untracked reparented zombies are silently reaped.
func waitTrackedExit(children []*child) int {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, syscall.ECHILD) {
				return 0
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return 1
		}
		for _, c := range children {
			if c.cmd.Process == nil || c.cmd.Process.Pid != pid {
				continue
			}
			fmt.Fprintf(os.Stderr, "supervisor: %s (pid %d) exited\n", c.name, pid)
			if ws.Exited() {
				return ws.ExitStatus()
			}
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return 1
		}
		// untracked orphan — reaped, loop
	}
}

// gracefulShutdown sends SIGTERM to surviving tracked children, waits
// up to 10 s, then SIGKILL.
func gracefulShutdown(children []*child) error {
	deadline := time.Now().Add(10 * time.Second)
	for _, c := range children {
		if c.cmd.ProcessState != nil && c.cmd.ProcessState.Exited() {
			continue
		}
		if c.cmd.Process != nil {
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		}
	}
	for time.Now().Before(deadline) {
		alive := false
		for _, c := range children {
			if c.cmd.ProcessState == nil || !c.cmd.ProcessState.Exited() {
				alive = true
				break
			}
		}
		if !alive {
			return nil
		}
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		time.Sleep(100 * time.Millisecond)
	}
	return killAll(children)
}

func killAll(children []*child) error {
	var firstErr error
	for _, c := range children {
		if c.cmd.Process != nil {
			if err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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
```

- [ ] **Step 4: Run, see pass**

```bash
bash hack/test.sh ./cmd/supervisor/...
```

- [ ] **Step 5: Commit**

```bash
git add cmd/supervisor/
git commit -m "$(cat <<'EOF'
cmd/supervisor: PID-1 supervisor for in-container sshd

Spawns /opt/devpod/sbin/sshd plus (optionally) the user's original
command as siblings; reaps zombies in a Wait4 loop; forwards
SIGTERM/SIGINT/SIGHUP/SIGQUIT to both children's PGIDs; exits when
either tracked child exits so kubelet restarts the container.

Replaces the sidecar nsenter wrapper. The supervisor IS PID 1 of the
user's target container — sshd lives in the user container's
namespaces directly, no setns at session time.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: images/supervisor — Dockerfile builds static OpenSSH + supervisor

**Files:**
- Create: `images/supervisor/Dockerfile`
- Create: `images/supervisor/sshd_config`

- [ ] **Step 1: Write `images/supervisor/sshd_config`**

```
Port 2222
HostKey /etc/devpod/ssh_host_ed25519_key
AuthorizedKeysFile /etc/devpod/authorized_keys
PermitRootLogin prohibit-password
PasswordAuthentication no
ChallengeResponseAuthentication no
UsePAM no
PrintMotd no
AllowAgentForwarding yes
AllowTcpForwarding yes
Subsystem sftp /opt/devpod/libexec/sftp-server
LogLevel INFO
```

- [ ] **Step 2: Write `images/supervisor/Dockerfile`**

```dockerfile
# syntax=docker/dockerfile:1.7

# --- Stage 1: build static OpenSSH (musl) ---
FROM alpine:3.20 AS sshd-build
ARG OPENSSH_VERSION=10.0p2
RUN apk add --no-cache build-base linux-headers \
    openssl-dev openssl-libs-static \
    zlib-dev zlib-static \
    autoconf wget
WORKDIR /src
RUN wget -qO openssh.tar.gz \
    "https://cdn.openbsd.org/pub/OpenBSD/OpenSSH/portable/openssh-${OPENSSH_VERSION}.tar.gz" && \
    tar xzf openssh.tar.gz
WORKDIR /src/openssh-${OPENSSH_VERSION}
RUN LDFLAGS="-static -static-pie" \
    CFLAGS="-fpie -O2" \
    ./configure \
        --prefix=/opt/devpod \
        --sbindir=/opt/devpod/sbin \
        --libexecdir=/opt/devpod/libexec \
        --sysconfdir=/opt/devpod/etc \
        --without-PAM \
        --without-selinux \
        --without-ldns \
        --disable-strip
RUN make -j sshd sftp-server
RUN mkdir -p /out/sbin /out/libexec /out/etc && \
    cp sshd /out/sbin/sshd && \
    cp sftp-server /out/libexec/sftp-server && \
    (test -f sshd-session && cp sshd-session /out/sbin/sshd-session || true) && \
    (test -f sshd-auth && cp sshd-auth /out/sbin/sshd-auth || true) && \
    strip /out/sbin/* /out/libexec/* 2>/dev/null || true

# --- Stage 2: build supervisor ---
FROM golang:1.22-alpine AS supervisor-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/devpod-supervisor ./cmd/supervisor

# --- Stage 3: minimal assembly, used as the initContainer image ---
FROM scratch
COPY --from=sshd-build      /out/sbin/    /opt/devpod/sbin/
COPY --from=sshd-build      /out/libexec/ /opt/devpod/libexec/
COPY --from=supervisor-build /out/devpod-supervisor /opt/devpod/devpod-supervisor
COPY images/supervisor/sshd_config /opt/devpod/etc/sshd_config
```

- [ ] **Step 3: Build locally**

```bash
docker buildx build --platform linux/arm64 \
  -f images/supervisor/Dockerfile \
  -t devpod-supervisor:dev-local \
  --load .
docker image inspect devpod-supervisor:dev-local | grep -E '"Size"|Architecture'
```

Expect size < 20 MB.

Spot-check the binary is static-pie:

```bash
docker run --rm --entrypoint=/bin/sh alpine:3.20 -c 'apk add --no-cache file >/dev/null && file /opt/devpod/sbin/sshd' \
  --volume "$(docker create devpod-supervisor:dev-local):/" \
  || true  # If this is awkward, use the simpler check below
```

Simpler: `docker run --rm devpod-supervisor:dev-local /opt/devpod/sbin/sshd -V 2>&1 | head -3`.

If the binary doesn't run inside scratch (no /tmp, no /etc/resolv.conf etc.), that's fine — it runs in user container's filesystem at runtime, not scratch.

- [ ] **Step 4: Commit**

```bash
git add images/supervisor/
git commit -m "$(cat <<'EOF'
images/supervisor: Dockerfile that builds static OpenSSH 10.0p2 + supervisor

Multi-stage build:
- Stage 1: alpine + musl-static-pie compile OpenSSH 10.0p2 portable
  from OpenBSD CDN tarball (no PAM, no selinux, no ldns). Outputs
  sshd, sftp-server, sshd-session, sshd-auth.
- Stage 2: Go static compile of cmd/supervisor.
- Stage 3: FROM scratch + binaries under /opt/devpod/.

Used as the v2 initContainer that copies binaries into a shared
emptyDir on every DevPod Pod.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: API — GatewayConfig.backendPort + SupervisorImage rename

**Files:**
- Modify: `api/v1alpha1/gatewayconfig_types.go`
- Regen: `api/v1alpha1/zz_generated.deepcopy.go`
- Regen: `config/crd/bases/devpod.io_gatewayconfigs.yaml` + chart mirror

- [ ] **Step 1: Edit `api/v1alpha1/gatewayconfig_types.go`**

In `GatewayConfigSpec`:

1. Rename `SidecarImage` field to `SupervisorImage`. Update JSON tag from `sidecarImage` to `supervisorImage`. Update godoc:

```go
// SupervisorImage is the container image holding the init-container
// payload: static OpenSSH (sshd, sftp-server) and the
// devpod-supervisor binary. It is consumed once per DevPod Pod by a
// `cp` initContainer that copies its contents into an emptyDir
// volume.
//
// +kubebuilder:validation:MinLength=1
SupervisorImage string `json:"supervisorImage"`
```

2. Add `BackendPort` field after `SupervisorImage`:

```go
// BackendPort is the TCP port the in-container sshd listens on inside
// each DevPod Pod. Defaults to 2222 so the user container can bind it
// without CAP_NET_BIND_SERVICE regardless of UID. The gateway dials
// this port via DevPod.status.endpoint.
//
// +optional
// +kubebuilder:default=2222
// +kubebuilder:validation:Minimum=1024
// +kubebuilder:validation:Maximum=65535
BackendPort int32 `json:"backendPort,omitempty"`
```

- [ ] **Step 2: Regenerate**

```bash
go generate ./...
```

This regenerates zz_deepcopy, config/crd/bases, and (via F1's sync
script) the chart copy.

- [ ] **Step 3: Verify build**

```bash
go build ./...
```

Expect failures everywhere `gw.Spec.SidecarImage` is referenced. We
fix those in T6/T7. For now: leave them broken. We commit the API
change first; subsequent tasks adapt callers.

Actually no — Go won't build if anything references the renamed field.
So: do a single rename sweep across the codebase here. Use:

```bash
grep -rn "SidecarImage" --include="*.go" .
```

Files likely hit: `cmd/controller/main.go`, `internal/controllers/devpod_controller_test.go`, `internal/render/pod.go` (or wherever sidecar image is consumed), `internal/render/render_test.go`. Rename each
to `SupervisorImage`.

- [ ] **Step 4: Build clean**

```bash
go build ./...
bash hack/test.sh ./...
```

Tests will still fail because the render layer's pod_test expects a
sidecar container. That's fine — T4 fixes the render layer. Just
confirm the **build** is clean (`go build ./... 2>&1 | grep -v "^$"`
should be empty). Tests being red is expected at this checkpoint.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/gatewayconfig_types.go \
  api/v1alpha1/zz_generated.deepcopy.go \
  config/crd/bases/devpod.io_gatewayconfigs.yaml \
  deploy/chart/templates/crds/devpod.io_gatewayconfigs.yaml \
  cmd/ internal/
git commit -m "$(cat <<'EOF'
api/v1alpha1: GatewayConfig.backendPort; SidecarImage → SupervisorImage

Rename the image field to reflect its new content (init-container
payload, not a runtime sidecar). Add BackendPort (default 2222) so the
in-container sshd binds an unprivileged port and the controller can
write status.endpoint accordingly.

v1alpha1 lets us break field names; operators re-apply their
GatewayConfig YAML with the new key. No conversion shim.

The render layer's pod_test breaks at this commit and is fixed in the
next task; build remains clean.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: render — drop sidecar injection, add init container + supervisor wrap

**Files:**
- Modify: `internal/render/render.go` (drop sidecar consts)
- Modify: `internal/render/pod.go` (rewrite)
- Modify: `internal/render/pod_test.go` (rewrite)
- Modify: `internal/render/service.go` (targetPort 2222)
- Modify: `internal/render/service_test.go` (assertion)

- [ ] **Step 1: Drop sidecar constants in render.go**

Remove these consts (no longer used):
- `SidecarContainerName`
- `EnvTargetContainer`
- `VolumeHostKey`

Add new consts:
- `SupervisorBootstrapName` = "devpod-bootstrap"
- `SupervisorVolumeBin` = "devpod-bin"
- `SupervisorVolumeHost` = "devpod-host"

- [ ] **Step 2: Service targetPort**

In `internal/render/service.go`, change the targetPort from 22 (or
named `ssh`) to the configured `backendPort`. Read it from `cfg.Spec.BackendPort`. Default behavior when 0 (not set): use 2222.

In `service_test.go`, update the assertion to expect 2222.

- [ ] **Step 3: Rewrite `internal/render/pod.go`**

Replace `Pod()` body:

```go
func Pod(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) (*corev1.Pod, error) {
    if dp.Spec.Pod == nil {
        return nil, ErrWorkloadKindMissing
    }
    backendPort := cfg.Spec.BackendPort
    if backendPort == 0 {
        backendPort = 2222
    }

    user := dp.Spec.Pod.Spec.DeepCopy()

    // Append the binaries emptyDir and host-key Secret volumes.
    user.Volumes = append(user.Volumes,
        emptyDirVolume(SupervisorVolumeBin),
        hostKeySecretVolume(SupervisorVolumeHost, HostKeySecretName(dp)),
    )

    // Optional persistence volume injection (unchanged from M2).
    if dp.Spec.Persistence != nil {
        user.Volumes = append(user.Volumes, homeVolume(dp))
        if err := injectHomeMount(user, dp); err != nil {
            return nil, err
        }
    }

    // Wrap the target container's command with the supervisor.
    if err := wrapTargetWithSupervisor(user, dp); err != nil {
        return nil, err
    }

    // Prepend the bootstrap initContainer.
    user.InitContainers = append([]corev1.Container{
        bootstrapInitContainer(cfg.Spec.SupervisorImage),
    }, user.InitContainers...)

    pod := &corev1.Pod{
        ObjectMeta: ObjectMeta(PodName(dp), cfg.Spec.DevPodNamespace, dp),
        Spec:       *user,
    }
    mergeStrings(pod.Labels, dp.Spec.Pod.ObjectMeta.Labels)
    pod.Annotations = mergeStringsCopy(pod.Annotations, dp.Spec.Pod.ObjectMeta.Annotations)
    return pod, nil
}

// wrapTargetWithSupervisor mutates the target container so its command
// becomes the supervisor and the user's original command falls into
// args. Also adds the two volumeMounts the supervisor needs.
func wrapTargetWithSupervisor(spec *corev1.PodSpec, dp *devpodv1alpha1.DevPod) error {
    targetName := ""
    if dp.Spec.Persistence != nil && dp.Spec.Persistence.TargetContainer != "" {
        targetName = dp.Spec.Persistence.TargetContainer
    } else {
        targetName = spec.Containers[0].Name
    }
    for i := range spec.Containers {
        if spec.Containers[i].Name != targetName {
            continue
        }
        c := &spec.Containers[i]

        // user's original cmd+args → supervisor args
        var userArgs []string
        userArgs = append(userArgs, c.Command...)
        userArgs = append(userArgs, c.Args...)

        c.Command = []string{"/opt/devpod/devpod-supervisor"}
        c.Args = userArgs

        c.VolumeMounts = append(c.VolumeMounts,
            corev1.VolumeMount{Name: SupervisorVolumeBin, MountPath: "/opt/devpod", ReadOnly: true},
            corev1.VolumeMount{Name: SupervisorVolumeHost, MountPath: "/etc/devpod", ReadOnly: true},
        )
        return nil
    }
    return fmt.Errorf("supervisor: target container %q not found in spec.pod.spec.containers", targetName)
}

func bootstrapInitContainer(supervisorImage string) corev1.Container {
    return corev1.Container{
        Name:    SupervisorBootstrapName,
        Image:   supervisorImage,
        Command: []string{"/bin/cp", "-a", "/opt/devpod/.", "/devpod-bin/"},
        VolumeMounts: []corev1.VolumeMount{
            {Name: SupervisorVolumeBin, MountPath: "/devpod-bin"},
        },
    }
}

func emptyDirVolume(name string) corev1.Volume {
    return corev1.Volume{
        Name:         name,
        VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
    }
}

func hostKeySecretVolume(name, secretName string) corev1.Volume {
    return corev1.Volume{
        Name: name,
        VolumeSource: corev1.VolumeSource{
            Secret: &corev1.SecretVolumeSource{
                SecretName:  secretName,
                DefaultMode: ptr.To[int32](0o400),
            },
        },
    }
}
```

Drop the old `sidecarContainer`, `hostKeyVolume` (legacy name), `homeVolume` stays.

`/bin/cp` is invoked via PATH; FROM-scratch image has no `/bin/cp`.
Override with a static busybox? Actually the supervisor image is
FROM scratch with just /opt/devpod/* — `/bin/cp` doesn't exist.

Solutions:
- (a) Use coreutils-static in the supervisor image: `/opt/devpod/cp` shipped explicitly.
- (b) Write the copy logic into supervisor itself: invoke `devpod-supervisor bootstrap` as the init container command.

Option (b) is cleaner. Update the supervisor to handle a `bootstrap`
subcommand:

```go
if len(os.Args) >= 2 && os.Args[1] == "bootstrap" {
    runBootstrap()
    return
}
```

`runBootstrap` walks `/opt/devpod/` and copies to `/devpod-bin/`
preserving file modes and ownership. Standard `filepath.Walk` + io.Copy.

Update Task 2's Dockerfile and Task 4's `bootstrapInitContainer`
accordingly:

```go
Command: []string{"/opt/devpod/devpod-supervisor", "bootstrap"},
```

Note in T1's commit: supervisor also handles the `bootstrap`
subcommand. Add a tiny test for it.

- [ ] **Step 4: Rewrite `internal/render/pod_test.go`**

Drop assertions about a sidecar container. Add:

```go
func TestRenderPod_V2_HasBootstrapInitContainer(t *testing.T) {
    pod, err := render.Pod(minimalDevPod(), cfg())
    if err != nil { t.Fatal(err) }
    if len(pod.Spec.InitContainers) != 1 {
        t.Fatalf("init containers = %d, want 1", len(pod.Spec.InitContainers))
    }
    ic := pod.Spec.InitContainers[0]
    if ic.Name != render.SupervisorBootstrapName {
        t.Errorf("init container name = %q, want %q", ic.Name, render.SupervisorBootstrapName)
    }
    if ic.Image != cfg().Spec.SupervisorImage {
        t.Errorf("init container image mismatch")
    }
    if ic.Command[0] != "/opt/devpod/devpod-supervisor" {
        t.Errorf("init container command = %v", ic.Command)
    }
}

func TestRenderPod_V2_TargetContainerWrappedWithSupervisor(t *testing.T) {
    dp := minimalDevPod()
    dp.Spec.Pod.Spec.Containers[0].Command = []string{"sleep", "infinity"}
    pod, err := render.Pod(dp, cfg())
    if err != nil { t.Fatal(err) }

    c := pod.Spec.Containers[0]
    if got := c.Command; len(got) != 1 || got[0] != "/opt/devpod/devpod-supervisor" {
        t.Errorf("command = %v, want [/opt/devpod/devpod-supervisor]", got)
    }
    if got := c.Args; len(got) != 2 || got[0] != "sleep" || got[1] != "infinity" {
        t.Errorf("args = %v, want [sleep infinity]", got)
    }
    var hasBin, hasHost bool
    for _, m := range c.VolumeMounts {
        if m.Name == render.SupervisorVolumeBin && m.MountPath == "/opt/devpod" { hasBin = true }
        if m.Name == render.SupervisorVolumeHost && m.MountPath == "/etc/devpod" { hasHost = true }
    }
    if !hasBin || !hasHost {
        t.Errorf("missing supervisor mounts: bin=%v host=%v", hasBin, hasHost)
    }
}

func TestRenderPod_V2_EmptyUserCommandStillRunsSshd(t *testing.T) {
    dp := minimalDevPod()
    dp.Spec.Pod.Spec.Containers[0].Command = nil
    dp.Spec.Pod.Spec.Containers[0].Args = nil
    pod, err := render.Pod(dp, cfg())
    if err != nil { t.Fatal(err) }
    if got := pod.Spec.Containers[0].Args; len(got) != 0 {
        t.Errorf("expected empty args, got %v", got)
    }
}

func TestRenderPod_V2_NoShareProcessNamespace(t *testing.T) {
    pod, err := render.Pod(minimalDevPod(), cfg())
    if err != nil { t.Fatal(err) }
    if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
        t.Errorf("ShareProcessNamespace must not be true in v2")
    }
}

func TestRenderPod_V2_MultipleContainers_OnlyTargetWrapped(t *testing.T) {
    dp := minimalDevPod()
    dp.Spec.Pod.Spec.Containers = append(dp.Spec.Pod.Spec.Containers, corev1.Container{
        Name: "companion", Image: "busybox", Command: []string{"sleep", "infinity"},
    })
    pod, err := render.Pod(dp, cfg())
    if err != nil { t.Fatal(err) }
    if pod.Spec.Containers[0].Command[0] != "/opt/devpod/devpod-supervisor" {
        t.Errorf("target container should be wrapped")
    }
    if got := pod.Spec.Containers[1].Command; len(got) != 2 || got[0] != "sleep" {
        t.Errorf("companion should be untouched, got command=%v", got)
    }
}
```

Drop or rename pre-v2 tests that expect a sidecar:
`TestRenderPod_HostKeyVolumePresent`, `TestRenderPod_HomeVolume_OnlyWhenPersistence`,
`TestRenderPod_SidecarCaps_OnlySysAdmin` — gone.

Keep persistence-related tests but adapt them to assert the mount is
on the target container that's also wrapped.

- [ ] **Step 5: Run tests**

```bash
bash hack/test.sh ./internal/render/...
```

- [ ] **Step 6: Commit**

```bash
git add internal/render/
git commit -m "$(cat <<'EOF'
internal/render: v2 — init container + supervisor wrap, no sidecar

render.Pod no longer injects a sidecar container. Instead:
- Prepends a "devpod-bootstrap" initContainer that runs
  `devpod-supervisor bootstrap` from the supervisor image to copy
  binaries into a shared emptyDir.
- Wraps the target container's command/args with the supervisor; the
  user's original command becomes the supervisor's args.
- Mounts the binaries emptyDir at /opt/devpod and the host-key Secret
  at /etc/devpod onto the target container.
- shareProcessNamespace removed; no setns at runtime.

Service.targetPort flips from 22 to GatewayConfig.spec.backendPort
(default 2222). The gateway already reads status.endpoint verbatim.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: webhook — drop sidecar-name + shareProcessNamespace rules

**Files:**
- Modify: `internal/webhook/devpod_webhook.go`
- Modify: `internal/webhook/devpod_webhook_test.go`

- [ ] **Step 1: Strip the rules**

In `validatePodConstraints`:

- Remove the loop rejecting `c.Name == render.SidecarContainerName`
  (the constant is gone after T4 anyway).
- Remove the `ShareProcessNamespace == false` rejection.

`validatePodConstraints` shrinks to just the "containers non-empty"
check.

- [ ] **Step 2: Update tests**

Drop `TestRejectsReservedSidecarName` and
`TestRejectsShareProcessNamespaceFalse`.

Existing positive tests (`TestAllowsValid`,
`TestAllowsVMOnly`, owner-immutability, etc.) keep passing.

- [ ] **Step 3: Run**

```bash
bash hack/test.sh ./internal/webhook/...
```

- [ ] **Step 4: Commit**

```bash
git add internal/webhook/
git commit -m "$(cat <<'EOF'
internal/webhook: drop sidecar-reserved-name + SPN rules

v2 has no sidecar container, so the "container name devpod-sshd is
reserved" rule is moot. shareProcessNamespace is no longer required
(nsenter is gone), so the "must be true" rule is moot too.

The remaining rules — xor pod/vm, non-empty containers, devpod-
prefix volume/mount, persistence sanity, PodName collision, owner
immutability — are all retained.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: controller — status.endpoint port from GwConfig

**Files:**
- Modify: `internal/controllers/devpod_controller.go`
- Modify: `internal/controllers/devpod_controller_test.go`

- [ ] **Step 1: Use backendPort in updateStatus**

In `updateStatus`, the Running-branch line:

```go
desired.Endpoint = pod.Status.PodIP + ":22"
```

becomes:

```go
port := r.GwConfig.Spec.BackendPort
if port == 0 { port = 2222 }
desired.Endpoint = fmt.Sprintf("%s:%d", pod.Status.PodIP, port)
```

- [ ] **Step 2: Adapt existing tests**

Any envtest case that asserts `status.endpoint == "10.0.0.1:22"`
flips to `"10.0.0.1:2222"`. Tests that mock Pod.Status.PodIP need
nothing else.

- [ ] **Step 3: Run**

```bash
bash hack/test.sh ./internal/controllers/...
```

- [ ] **Step 4: Commit**

```bash
git add internal/controllers/
git commit -m "$(cat <<'EOF'
controllers/devpod: status.endpoint port from GatewayConfig.backendPort

The in-container sshd listens on backendPort (default 2222) instead
of a hardcoded 22. Controller writes status.endpoint = "<podIP>:<backendPort>"
so the gateway dials the right port.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: cmd/controller + Helm chart — image + flag rename

**Files:**
- Modify: `cmd/controller/main.go`
- Modify: `deploy/chart/values.yaml`
- Modify: `deploy/chart/templates/controller.yaml`
- Modify: `deploy/chart/templates/gatewayconfig.yaml` (if it references sidecar)

- [ ] **Step 1: cmd/controller**

In `cmd/controller/main.go`:

- Flag `--sidecar-image` becomes `--supervisor-image`.
- Var `sidecarImage` becomes `supervisorImage`.
- The `GwConfig.Spec.SidecarImage` assignment becomes `SupervisorImage`.

- [ ] **Step 2: values.yaml**

Replace the `image.sidecar` block with `image.supervisor`:

```yaml
image:
  controller: { ... }
  gateway:    { ... }
  supervisor:
    repository: ghcr.io/mrhaoxx/devpod-supervisor
    tag: dev
```

- [ ] **Step 3: chart templates**

In `deploy/chart/templates/controller.yaml`, change the arg:

```yaml
- --supervisor-image={{ .Values.image.supervisor.repository }}:{{ .Values.image.supervisor.tag }}
```

If `deploy/chart/templates/gatewayconfig.yaml` (the GatewayConfig CR
applied via Helm) sets `sidecarImage`, change it to `supervisorImage`
with the new value.

- [ ] **Step 4: Render check**

```bash
helm template deploy/chart > /tmp/render.yaml
grep -c "supervisorImage" /tmp/render.yaml
grep -c "sidecarImage" /tmp/render.yaml  # should be 0
```

- [ ] **Step 5: Commit**

```bash
git add cmd/controller/main.go deploy/chart/
git commit -m "$(cat <<'EOF'
cmd/controller, deploy/chart: rename sidecar → supervisor

Mechanical rename across the controller flag, the Helm values block,
and the rendered GatewayConfig CR. No behavior change; just keeping
the names accurate after v2's image-purpose change.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: hack/e2e-up.sh — build supervisor image

**Files:**
- Modify: `hack/e2e-up.sh`

- [ ] **Step 1: Replace the sidecar image build**

Find the section that builds the sidecar image (look for
`images/sidecar/Dockerfile`). Replace with:

```bash
docker buildx build \
    --platform linux/arm64 \
    -f images/supervisor/Dockerfile \
    -t devpod-supervisor:e2e \
    --load .
```

Update any other reference (image tag, image push) to use
`devpod-supervisor:e2e`.

Also adjust the helm values override so the deployed chart points at
the right image.

- [ ] **Step 2: Run end-to-end**

```bash
bash hack/e2e-up.sh
kubectl -n devpods get devpod
kubectl -n devpod-system rollout status deploy/devpod-gateway
```

Then create a quick DevPod and SSH in:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: v2sanity, namespace: devpods}
spec:
  owner: alice
  running: true
  pod:
    spec:
      containers:
      - {name: dev, image: debian:stable, command: ["sleep","infinity"]}
EOF
kubectl -n devpods wait --for=jsonpath='{.status.phase}'=Running devpod/v2sanity --timeout=120s

pkill -f "port-forward.*devpod-gateway" 2>/dev/null
kubectl -n devpod-system port-forward svc/devpod-gateway 2222:22 >/dev/null 2>&1 &
sleep 2

KEY=$(cat /tmp/devpod-test-key-path)
ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -p 2222 -i "$KEY" alice+v2sanity@127.0.0.1 -- 'uname -a; ps -ef | head'

# Cleanup
kubectl -n devpods delete devpod v2sanity --ignore-not-found
```

- [ ] **Step 3: Commit**

```bash
git add hack/e2e-up.sh
git commit -m "$(cat <<'EOF'
hack/e2e-up: build supervisor image instead of sidecar image

The sidecar image build is removed; the supervisor image is built
via buildx from images/supervisor/Dockerfile and loaded into the
local daemon for the cluster to pull.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: hack/e2e-v2.sh — prove SFTP works

**Files:**
- Create: `hack/e2e-v2.sh`

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# End-to-end demo for v2: the bug v2 fixes is "scp / VS Code Remote
# fails because sftp-server runs in the wrong namespace". This script
# proves that's gone.
#
# Prereqs: hack/e2e-up.sh has been run and built the supervisor image.

set -euo pipefail

NS=devpods
NAME=v2demo
OWNER=alice
GW_PORT=2222
KEY="$(cat /tmp/devpod-test-key-path)"

cleanup() {
    pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
    kubectl -n "$NS" delete devpod "$NAME" --ignore-not-found --wait=false 2>/dev/null || true
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

echo "[1/5] Apply DevPod"
cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: $NAME, namespace: $NS}
spec:
  owner: $OWNER
  running: true
  persistence: {size: 1Gi, mountPath: /workspace}
  pod:
    spec:
      containers:
      - {name: dev, image: debian:stable, command: ["sleep","infinity"]}
EOF
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=180s

echo "[2/5] Verify sshd runs in user container (debian)"
start_port_forward
out=$(ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -p "$GW_PORT" -i "$KEY" "$OWNER+$NAME@127.0.0.1" -- 'cat /etc/os-release | head -1')
if ! grep -q "Debian" <<<"$out"; then
    echo "FAIL: expected Debian rootfs, got: $out"
    exit 1
fi
echo "OK: shell lands in user container (debian)"

echo "[3/5] scp a local file to /workspace"
LOCAL=/tmp/devpod-v2-marker
echo "v2 sftp works at $(date)" > "$LOCAL"
scp -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -P "$GW_PORT" -i "$KEY" "$LOCAL" "$OWNER+$NAME@127.0.0.1:/workspace/marker"

echo "[4/5] Read it back via SSH"
got=$(ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -p "$GW_PORT" -i "$KEY" "$OWNER+$NAME@127.0.0.1" -- 'cat /workspace/marker')
if [[ "$got" != "$(cat $LOCAL)" ]]; then
    echo "FAIL: scp-uploaded file did not round-trip"
    echo "want: $(cat $LOCAL)"
    echo "got:  $got"
    exit 1
fi
echo "OK: scp wrote into user container's /workspace; SSH shell sees it"

echo "[5/5] Hibernate + resume + marker survives"
kubectl -n "$NS" patch devpod "$NAME" --type=merge -p '{"spec":{"running":false}}'
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Stopped devpod/"$NAME" --timeout=60s
kubectl -n "$NS" patch devpod "$NAME" --type=merge -p '{"spec":{"running":true}}'
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=180s
start_port_forward
got=$(ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -p "$GW_PORT" -i "$KEY" "$OWNER+$NAME@127.0.0.1" -- 'cat /workspace/marker')
if [[ "$got" != "$(cat $LOCAL)" ]]; then
    echo "FAIL: marker lost across hibernate"
    exit 1
fi
echo "OK: marker survived hibernate"
rm -f "$LOCAL"

echo
echo "OK: v2 demo passed — sshd in user ns, scp/sftp work, hibernate works."
```

`chmod +x hack/e2e-v2.sh`.

- [ ] **Step 2: Run**

```bash
bash hack/e2e-v2.sh
```

- [ ] **Step 3: Commit**

```bash
git add hack/e2e-v2.sh
git commit -m "$(cat <<'EOF'
hack/e2e-v2: prove scp/sftp works against the in-container sshd

Five-step demo:
1. Apply a DevPod with persistence
2. Verify shell lands in user container (debian rootfs, not alpine sidecar)
3. scp a local file into /workspace
4. ssh-shell reads the same content back
5. Hibernate + resume + marker still readable

The bug v2 fixes is step 3/4 — under v1, scp's SFTP subsystem ran in
the sidecar's mount namespace and the shell's view didn't see the
uploaded file. v2's supervisor + in-container sshd makes the two
agree.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: Delete `cmd/sidecar/` and `images/sidecar/`

**Files:**
- Delete: `cmd/sidecar/`
- Delete: `images/sidecar/`

- [ ] **Step 1: rm them**

```bash
git rm -rf cmd/sidecar images/sidecar
```

- [ ] **Step 2: Verify nothing references them**

```bash
grep -rn "cmd/sidecar\|images/sidecar" --include="*.go" --include="*.sh" --include="*.yaml" --include="Dockerfile*" .
```

Should return zero hits. If anything remains (chart template, README,
older e2e script), update or remove it.

- [ ] **Step 3: Final check**

```bash
go build ./...
bash hack/test.sh ./...
bash hack/e2e-up.sh 2>&1 | tail -5
bash hack/e2e-v2.sh
bash hack/e2e-m2.sh   # M2 hibernate still works
bash hack/e2e-m3.sh   # M3 multi-replica / PROXY / trusted proxy still works
```

All green.

- [ ] **Step 4: Commit**

```bash
git commit -m "$(cat <<'EOF'
cmd/sidecar, images/sidecar: delete (replaced by supervisor)

v2 has shipped: sshd runs directly in the user container's namespaces
via a PID-1 supervisor. The old sidecar binary, the nsenter wrapper,
and the alpine sidecar image are no longer reachable. Remove them
entirely — no backward-compat shim.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Done criteria

When every checkbox is checked, all unit/envtest pass, and
`hack/e2e-v2.sh`, `hack/e2e-m2.sh`, `hack/e2e-m3.sh` are all green
against an OrbStack cluster — v2 is shipped. The DevPod CRD schema is
unchanged for users; only operators see GatewayConfig field renames.

---

## Self-review notes

Spec coverage check:
- Spec §2.1 Pod overlay → T4
- Spec §2.2 target container selection → T4 (`wrapTargetWithSupervisor`)
- Spec §2.3 backendPort 2222 → T3 + T4 (service) + T6 (controller)
- Spec §2.4 supervisor responsibilities → T1
- Spec §2.5 sshd_config → T2
- Spec §2.6 host-key Secret unchanged → no task; existing M1 render covers it
- Spec §3 image build pipeline → T2
- Spec §4.1 cmd/supervisor → T1
- Spec §4.2 delete cmd/sidecar → T10
- Spec §4.3 render rewrite → T4
- Spec §4.4 webhook deletions → T5
- Spec §4.5 controller status → T6
- Spec §4.6 GatewayConfig schema → T3
- Spec §4.7 chart → T7
- Spec §4.8 gateway unchanged → no task

No placeholders. Type names (`SupervisorImage`, `BackendPort`,
`SupervisorBootstrapName`, `SupervisorVolumeBin`,
`SupervisorVolumeHost`) introduced in T3/T4 and used consistently.
The supervisor's `bootstrap` subcommand referenced in T4 is added to
T1's responsibilities (note added in T4 step 3).
