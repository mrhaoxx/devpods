# DevPod v2 — In-container sshd (drop the sidecar)

**Status:** Approved
**Date:** 2026-05-13
**Depends on:** M0+M1+M2+M3+F1 already merged.
**Replaces:** the per-DevPod sidecar container path.

---

## 1. Goal

Eliminate the dedicated sidecar container. Replace it with:

1. An **init container** that copies static OpenSSH + a supervisor binary
   into a shared `emptyDir`.
2. A **command wrapper** on the user's target container: the supervisor
   becomes PID 1 of that container, spawning `sshd` and (if the user
   provided a command) the user's original command as siblings.

Result: sshd runs directly inside the user container's namespaces, so:

- VS Code Remote / SFTP land files where the SSH shell lands.
- Port forwarding (`direct-tcpip` / `forwarded-tcpip`) and agent
  forwarding work natively without nsenter.
- No `CAP_SYS_ADMIN`, no `shareProcessNamespace`, no per-session setns.
- One fewer container per DevPod. Pod is the user's containers + a
  short-lived init container.

The DevPod CRD schema is **unchanged**. This is a backend
re-implementation; everything user-facing in the API stays compatible.

Non-goals:

- KubeVirt VM path (M4 was deferred).
- Auto-hibernate (M5).
- Backward-compat support for the old sidecar layout. Existing DevPods
  upgrade in-place when the controller re-renders them; their Pods
  restart with the new shape. No migration shim.

---

## 2. Architecture

### 2.1 Pod overlay

For a DevPod with `spec.pod.spec.containers = [dev]` and no
`spec.persistence.targetContainer`, the rendered Pod becomes:

```yaml
spec:
  # shareProcessNamespace REMOVED. Containers are isolated again.
  initContainers:
  - name: devpod-bootstrap
    image: <supervisor image>
    command: ["/bin/cp", "-a", "/opt/devpod/.", "/devpod-bin/"]
    volumeMounts:
    - {name: devpod-bin, mountPath: /devpod-bin}
  containers:
  - name: dev                        # user's container, untouched apart from:
    image: <user image>
    command: ["/opt/devpod/devpod-supervisor"]
    args: [<user's original command + args>]   # may be empty
    volumeMounts:
    - {name: devpod-bin,  mountPath: /opt/devpod, readOnly: true}
    - {name: devpod-host, mountPath: /etc/devpod, readOnly: true}
    # plus any user volumeMounts and the persistence mount, unchanged
  volumes:
  - {name: devpod-bin,  emptyDir: {}}
  - name: devpod-host
    secret:
      secretName: <owner>-<dpName>-hostkey
      defaultMode: 0o400
  # plus persistence volume etc.
```

Other user containers (when `containers[]` has > 1 entry) are passed
through verbatim. Only the target container gets the wrap.

### 2.2 Target container selection

Same selector as `spec.persistence.targetContainer`:

- If `spec.persistence != nil && spec.persistence.targetContainer != ""`: that container.
- Else: `containers[0]`.

The webhook ensures the named container exists (existing rule from M2).

If user did not provide a `command` (relying on image ENTRYPOINT),
supervisor runs with an empty user-cmd: only `sshd` runs in the
container. The DevPod becomes a pure dev shell. This is the answer to
"what if the user image is opaque": don't try to inspect the registry;
make the behavior explicit. Documented in CRD field godoc.

### 2.3 Backend listen port

sshd listens on **2222** (not 22). The user container needs no
privileged capability to bind it, regardless of UID. The
controller writes `status.endpoint = "<podIP>:2222"`. The gateway dials
that endpoint the same way it does today (per M2/M3 the gateway reads
`status.endpoint`).

`render.Service` already maps a named port; targetPort becomes 2222.

Configurable via `GatewayConfig.spec.backendPort` (defaults to 2222).
Cluster operators with strict pod-security policies can shift it.

### 2.4 supervisor responsibilities

`/opt/devpod/devpod-supervisor` is a small static Go binary (~3 MB).
PID 1 of the user's target container.

1. **Spawn `sshd`**:
   ```
   exec /opt/devpod/sbin/sshd -D -e -f /etc/devpod/sshd_config \
       -h /etc/devpod/ssh_host_ed25519_key
   ```
   Runs in background. Logs to stderr (so kubelet picks them up).

2. **Spawn user command** (only if `os.Args[1:]` is non-empty):
   ```
   exec <args[1]> <args[2:]...>
   ```

3. **Reap zombies** in a wait3 loop. PID-1 responsibility.

4. **Signal forwarding**: SIGTERM / SIGINT / SIGHUP / SIGQUIT delivered
   to supervisor are forwarded to all direct children.

5. **Exit policy**:
   - If sshd dies: exit. Kubelet restarts the container.
   - If user command (when present) dies: exit. Same.
   - Either way: bail out, let kubelet recover. No partial states.

6. **No log fan-out / no service discovery**. PID-1 minimalism.

### 2.5 sshd_config (baked into supervisor image)

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

No `ForceCommand`. sshd's default login flow does the right thing.

The config is baked into the image at `/opt/devpod/etc/sshd_config`
(or similar) — no per-DevPod customization needed, no ConfigMap.

### 2.6 Host-key Secret content

Existing `render.HostKeySecret` produces per-DevPod Secrets with:

- `ssh_host_ed25519_key` (private)
- `ssh_host_ed25519_key.pub`
- `authorized_keys` (the gateway internal-key pubkey)

v2 mounts the same Secret at `/etc/devpod/` in the user container.
sshd reads them from there. Identical to today, just a different
container.

---

## 3. Image build pipeline

`images/supervisor/Dockerfile` (replaces `images/sidecar/Dockerfile`):

```dockerfile
# syntax=docker/dockerfile:1.7

# --- Stage 1: build static OpenSSH (musl) ---
FROM alpine:3.20 AS sshd-build
ARG OPENSSH_VERSION=10.0p2
RUN apk add --no-cache build-base linux-headers \
    openssl-dev openssl-libs-static zlib-dev zlib-static \
    autoconf
WORKDIR /src
RUN wget -qO- https://cdn.openbsd.org/pub/OpenBSD/OpenSSH/portable/openssh-${OPENSSH_VERSION}.tar.gz | tar xz
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
        --disable-strip && \
    make -j sshd sftp-server && \
    mkdir -p /out/sbin /out/libexec && \
    cp sshd /out/sbin/sshd && \
    cp sftp-server /out/libexec/sftp-server && \
    strip /out/sbin/sshd /out/libexec/sftp-server

# --- Stage 2: build supervisor ---
FROM golang:1.22-alpine AS supervisor-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/devpod-supervisor ./cmd/supervisor

# --- Stage 3: assembly into scratch (used as initContainer) ---
FROM scratch
COPY --from=sshd-build      /out/sbin/sshd                  /opt/devpod/sbin/sshd
COPY --from=sshd-build      /out/libexec/sftp-server        /opt/devpod/libexec/sftp-server
COPY --from=supervisor-build /out/devpod-supervisor          /opt/devpod/devpod-supervisor
COPY images/supervisor/sshd_config /opt/devpod/etc/sshd_config
```

Built with buildx for `linux/amd64,linux/arm64`. Image size target:
< 15 MB (3 binaries plus the config file).

OpenSSH 10.0p2 is pinned via ARG; bump on each security release. The
bol-van/bins repo confirms this exact recipe produces working
static-pie binaries on x86_64; we replicate it under our own
reproducible CI rather than trusting third-party blobs.

---

## 4. Code changes

### 4.1 New `cmd/supervisor/main.go`

~150 lines:

- Parse `os.Args[1:]` as the user's original command.
- `os.StartProcess("/opt/devpod/sbin/sshd", ...)` with stderr inherited.
- If user-cmd present: `os.StartProcess(args[0], args[1:]...)`.
- Signal handler installed for `SIGTERM, SIGINT, SIGHUP, SIGQUIT`,
  forwards to both children's PGIDs.
- Main loop: `syscall.Wait4(-1, ...)` for any child:
  - If it's sshd or the user-cmd: signal the other, exit with the
    reaper's status.
  - Otherwise it's a session orphan reparented to us — silently reap.
- On exit: if either tracked child still running, send SIGTERM, wait
  up to 10 s, then SIGKILL, then exit.

No external dependencies beyond stdlib + `golang.org/x/sys/unix` for
PGID signaling.

### 4.2 Delete `cmd/sidecar/`

The directory and its package go away entirely. The nsenter wrapper,
the cgroup-based PID discovery, the SFTP-via-sidecar-namespace logic
— all gone.

### 4.3 Render rewrite

`internal/render/pod.go`:

- Drop `sidecarContainer()`, `VolumeHostKey`, `EnvTargetContainer`.
- Drop `shareProcessNamespace = true`.
- Add `injectSupervisor(spec *corev1.PodSpec, dp *DevPod)` which:
  1. Finds the target container by name (same logic as
     `injectHomeMount`).
  2. Wraps its `command`: original goes into `args`, `command` becomes
     `["/opt/devpod/devpod-supervisor"]`.
  3. Adds two volumeMounts to the target container.
- Add `initContainerBootstrap()` returning the
  `corev1.Container{InitContainer, Image: supervisorImage, ...}` value.
- Append both `devpod-bin` (emptyDir) and `devpod-host` (Secret)
  volumes to `pod.spec.volumes`.

`render.Service`:

- targetPort changes from `ssh` (22) to a configurable port (default
  2222).

`render.HostKeySecret`:

- No change. Same contents; mounted at a different path in v2.

`render.Pod`: the high-level orchestrator change is just substituting
the sidecar-append call with init-container-prepend + supervisor-wrap.

### 4.4 Webhook rule deletions

- Drop "container name `devpod-sshd` is reserved" check. No more
  reserved container name (we mutate the user's container, we don't
  add ours).

- Drop "spec.pod.spec.shareProcessNamespace must be true" check.
  shareProcessNamespace is no longer required; it's irrelevant.

Webhook rules retained:
- xor pod / vm
- non-empty containers
- reserved `devpod-` volume / mount name prefix
- persistence target container exists
- persistence mountPath collision
- DevPod name length
- PodName collision (F1.T4)

### 4.5 Controller status

`status.endpoint` becomes `<podIP>:<backendPort>` instead of hardcoded
`:22`. Configured via the new `GatewayConfig.spec.backendPort` (default
2222).

`status.workloadRef` unchanged.

### 4.6 GatewayConfig schema

Add one field:

```go
type GatewayConfigSpec struct {
    // ... existing fields ...

    // BackendPort is the TCP port the in-container sshd listens on.
    // Defaults to 2222 so the user container can bind it without
    // CAP_NET_BIND_SERVICE regardless of UID. The gateway dials this
    // port via DevPod.status.endpoint.
    //
    // +optional
    // +kubebuilder:default=2222
    // +kubebuilder:validation:Minimum=1024
    // +kubebuilder:validation:Maximum=65535
    BackendPort int32 `json:"backendPort,omitempty"`
}
```

Rename `SidecarImage` → `SupervisorImage`. v1alpha1 lets us break
field names; operators re-apply their GatewayConfig YAML. No
multi-version conversion shim.

### 4.7 Helm chart

- Drop `image.sidecar`. Add `image.supervisor` (same shape).
- The controller `--sidecar-image` flag becomes `--supervisor-image`.
- `deploy/chart/templates/controller.yaml` updates accordingly.

### 4.8 Gateway

Unchanged. Authenticator, dialer, proxy, audit, metrics — all of
M1/M3 — work identically with the new backend; the only difference is
the backend listens on 2222 instead of 22 (and status.endpoint
encodes that). `internal/gateway/dialer.go` already uses the endpoint
verbatim from `status.endpoint`.

---

## 5. Test plan

### 5.1 Unit

- `internal/render/pod_test.go`:
  - Pod has 1 initContainer (bootstrap) and 1 container per user spec.
  - Target container's `command` is `["/opt/devpod/devpod-supervisor"]`
    and `args` is the user's original command+args concatenated.
  - User command empty → args is empty.
  - Both `devpod-bin` and `devpod-host` volumeMounts appear on target.
  - Multi-container pod: only target wrapped; other containers
    untouched.
  - `shareProcessNamespace` is not set.

- `cmd/supervisor/main_test.go` (new):
  - Spawn supervisor with a child cmd that exits 0 → supervisor exits 0.
  - Spawn supervisor with a child cmd that exits 7 → supervisor exits 7.
  - Signal forwarding test: send SIGTERM to supervisor pid, verify child
    receives it (use a small sleep/sentinel cmd).

- `internal/webhook/devpod_webhook_test.go`:
  - Tests for the dropped rules removed.
  - Existing rules still pass.

### 5.2 envtest

- `internal/controllers/devpod_controller_test.go`: existing
  reconciler tests update to expect the new render shape (no sidecar
  container).

### 5.3 e2e

`hack/e2e-v2.sh` (new):

1. Apply DevPod with `command: ["sleep","infinity"]` + persistence.
2. Wait for Running.
3. `ssh alice+v2demo@gw -- whoami` → root.
4. `ssh alice+v2demo@gw -- ls /` → user image's rootfs (debian).
5. `scp ./localfile alice+v2demo@gw:/workspace/copied` → file lands in
   /workspace inside user container. **The key bug v2 fixes.**
6. `ssh alice+v2demo@gw -- cat /workspace/copied` → matches.
7. Hibernate + resume → file survives (re-use M2 flow).
8. VS Code Remote SSH (manual / scripted): server install completes
   without "Connection closed" error.

`hack/e2e-m2.sh` and `hack/e2e-m3.sh` continue to pass without
modification (they exercise behavior that v2 keeps identical at the
SSH protocol level).

---

## 6. Risks

- **Building static OpenSSH cross-arch**: musl + static-pie + arm64
  build needs to be tested. If `configure` warns on missing optional
  features (X11, KRB5), suppress safely with `--without-X`.
- **Image size**: target < 15 MB. If OpenSSH ends up shipping
  sshd-session and sshd-auth separately (10.0p2 does), include both.
- **Strict pod security**: K8s 1.29+ Restricted PodSecurity policy
  rejects `runAsNonRoot: false` containers from binding privileged
  ports. v2 uses 2222 so this is fine, but document.
- **emptyDir size**: bootstrap copies a few MB; default emptyDir
  sizeLimit is unset (uses node ephemeral storage). Set
  `emptyDir.sizeLimit: 50Mi` defensively.
- **Image registry availability**: the user pod can't start if
  bootstrap image is unpullable. Same issue today's sidecar has —
  configurable via chart values + imagePullSecrets.

---

## 7. Files touched (sketch)

```
cmd/sidecar/                                    DELETE
cmd/supervisor/main.go                          NEW
cmd/supervisor/main_test.go                     NEW

images/sidecar/                                 DELETE
images/supervisor/Dockerfile                    NEW
images/supervisor/sshd_config                   NEW

internal/render/pod.go                          rewrite (sidecar → supervisor wrap + init container)
internal/render/pod_test.go                     rewrite
internal/render/render.go                       drop SidecarContainerName / VolumeHostKey / EnvTargetContainer

internal/webhook/devpod_webhook.go              drop sidecar-name + shareProcessNamespace rules
internal/webhook/devpod_webhook_test.go         drop corresponding tests

internal/controllers/devpod_controller.go       minimal (status.endpoint port plumbed via GwConfig)
internal/controllers/devpod_controller_test.go  adapt existing pod-shape assertions

api/v1alpha1/gatewayconfig_types.go             add BackendPort; rename SidecarImage → SupervisorImage
api/v1alpha1/zz_generated.deepcopy.go           regen
config/crd/bases/devpod.io_gatewayconfigs.yaml  regen + sync
deploy/chart/templates/crds/...                 regen via T2

deploy/chart/values.yaml                        sidecar → supervisor image
deploy/chart/templates/controller.yaml          flag rename
deploy/chart/templates/gatewayconfig.yaml       ditto

hack/e2e-up.sh                                  build supervisor image instead of sidecar
hack/e2e-v2.sh                                  NEW
```

---

## 8. Open questions resolved during brainstorming

- Q: Backend port (22 or 2222)? **A:** 2222 by default, configurable.
  Eliminates the entire CAP_NET_BIND_SERVICE / root question.
- Q: User didn't set `command` — fall back to image ENTRYPOINT? **A:**
  No. Supervisor runs only sshd in that case. Documented behavior.
- Q: Multi-container pods? **A:** Only the target container (default
  `containers[0]`, override via `spec.persistence.targetContainer`)
  gets the wrap. Other containers verbatim passthrough.
- Q: sshd_config — baked or per-DevPod? **A:** Baked into the
  supervisor image. authorized_keys + host-key come per-DevPod from
  the existing Secret.
- Q: OpenSSH source? **A:** Build ourselves from OpenBSD CDN tarball
  pinned via Dockerfile ARG. Don't trust third-party static-binary
  repos.
- Q: Backward compat shim for old sidecar layout? **A:** No. Existing
  DevPods upgrade in-place when the controller re-renders; their Pods
  restart with the v2 shape.
