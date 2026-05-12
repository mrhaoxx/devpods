# DevPod Design

**Status:** Approved (2026-05-12)
**Date:** 2026-05-12
**Audience:** project author and future implementers

DevPod is a Kubernetes-native multi-tenant remote development platform. A
developer creates a `DevPod` custom resource and gets an SSH-reachable
development environment running in the cluster. The environment can be a
regular Pod or a KubeVirt virtual machine; the SSH access path is the same
either way. Persistence, hibernation, and idle suspension are first-class
parts of the resource model.

---

## 1. Goals and non-goals

### Goals

- A single CRD (`DevPod`) that lets a developer self-serve a personal dev
  environment with SSH access, by writing standard Kubernetes-shaped spec.
- Full passthrough of `PodSpec` (or KubeVirt `VirtualMachineSpec`) so users
  can use any field they already know: resources, volumes, securityContext,
  nodeSelector, tolerations, etc. The controller only adds the SSH entry
  point; it does not otherwise modify the user's spec.
- Multi-tenant: many users on one cluster, separated by ownership labels and
  NetworkPolicy. Per-pod resource limits enforced natively by Kubernetes.
- A single SSH gateway as the only externally exposed surface, so the
  user-facing endpoint is one stable hostname/port.
- Workload-agnostic gateway: the gateway speaks SSH and dials a generic
  `status.endpoint` on the DevPod. New workload types can be added without
  changing the gateway.
- Manual hibernation (`spec.running: false`) and optional idle-based
  auto-hibernation, both preserving persistent storage.
- Two authentication paths into the gateway: direct client SSH using
  per-user public keys, and connections from a trusted upstream
  SSH-terminating proxy.

### Non-goals (v1)

- Cluster-level per-user resource quota aggregation.
- OIDC / SSO actual integration (schema reserves a field for it).
- Web UI / dashboard.
- Multi-cluster federation.
- User-image building (Dockerfile-style templates, image presets).
- Built-in TLS / WebSocket ingress (rely on external nginx / Ingress when
  needed; gateway supports being behind PROXY-protocol-aware LBs).
- Session recording / asciinema-style replay.
- Server-side SSH session migration across gateway restarts.

---

## 2. Architecture

### 2.1 Components

```
┌────────────────────────────────────────────────────────────────────┐
│ cluster                                                            │
│                                                                    │
│   devpod-controller (Deployment, 1 replica)                        │
│     - watches User, DevPod, GatewayConfig                          │
│     - reconciles Pod / KubeVirt VM / PVC / Service / NetworkPolicy │
│     - writes status.endpoint, manages finalizers and hibernation   │
│                                                                    │
│   devpod-gateway (Deployment, N replicas, stateless)               │
│     - SSH listener (port 22, optionally fronted by PROXY proto v2) │
│     - terminates client SSH, authenticates, dials backend          │
│     - patches DevPod.status.lastActivityTime                       │
│                                                                    │
│   devpod-webhook (Deployment, served by controller binary)         │
│     - validating + mutating admission for DevPod                   │
│                                                                    │
│   namespace: devpods                                               │
│     - all DevPod-managed Pods, VMs, PVCs, Services live here       │
│     - per-DevPod Secret holds the sidecar host key                 │
│     - NetworkPolicy denies cross-owner pod-to-pod traffic          │
│                                                                    │
│   namespace: devpod-system                                         │
│     - controller, gateway, webhook                                 │
│     - gateway host-key Secret and internal-key Secret              │
└────────────────────────────────────────────────────────────────────┘
```

### 2.2 Why a single gateway component

A central SSH gateway is the only way to provide one stable external
endpoint, unified authentication, and per-user/per-pod routing without
exploding the number of LoadBalancers or NodePorts. Per-pod NodePorts do
not scale to hundreds of users; `kubectl exec`-as-SSH is incompatible with
standard SSH clients (and therefore with VS Code Remote-SSH, which is a
hard requirement).

### 2.3 Why the gateway dials a generic endpoint

The DevPod's workload may be a regular Pod (sshd injected by the
controller as a sidecar) or a KubeVirt VirtualMachine (sshd inside the
guest OS). Treating the workload as opaque and dialing
`status.endpoint:<port>` keeps the gateway from caring which kind it is.
Future workload types (bare-metal nodes, Firecracker VMs, externally
hosted machines registered as DevPods) can be added by writing a new
controller that fills in `status.endpoint`. The gateway needs no change.

### 2.4 Why sshd as a sidecar (Pod case), not in the user's container

The user controls `spec.pod` entirely. Modifying the user's container's
command to also launch sshd would violate the "only the entry point
changes" constraint and require the user's image to ship sshd, a user
account, and PAM. A sidecar with `shareProcessNamespace: true` and an
`nsenter` wrapper as `ForceCommand` and SFTP subsystem lets every SSH
session, every exec, and every SFTP operation land in the user
container's mount and PID namespaces, with no requirement on the user's
image other than having a shell available.

### 2.5 Tenancy model

All DevPods live in a single namespace (`devpods`, configurable). A
`devpod.io/owner=<user>` label on every DevPod-owned object selects per-owner
NetworkPolicy. Cross-tenant access is prevented by the gateway code
checking `DevPod.spec.owner == authenticated user`; there is no Kubernetes
RBAC backstop for this boundary, which is the principal trade-off of this
choice and is reflected in the test plan.

This trade-off was made deliberately to keep namespace count constant and
operations simple. Should the cluster need per-user `ResourceQuota`, image
pull secrets, or RBAC, the model would have to move to namespace-per-user.
The CRD schema does not preclude that change.

---

## 3. Custom Resource Definitions

All under `apiVersion: devpod.io/v1alpha1`.

### 3.1 `User` (cluster-scoped)

```yaml
apiVersion: devpod.io/v1alpha1
kind: User
metadata:
  name: alice                 # SSH login user; pattern [a-z0-9-]{1,32}, no '+'
spec:
  pubkeys:                    # at least one; OpenSSH authorized_keys line
    - "ssh-ed25519 AAAA... alice@laptop"
  oidcSubject: ""             # reserved; v1alpha1 ignores
  displayName: "Alice Liu"    # optional, cosmetic
status:
  conditions: []              # Ready, KeysValidated
  devPodCount: 0
```

Validation:

- `metadata.name` matches `^[a-z0-9-]{1,32}$` and does not contain `+`
  (the `+` is reserved as the separator in `alice+podname` SSH login
  strings).
- Every `pubkeys` entry parses as a valid OpenSSH public key.
- Deletion of a `User` is gated by a finalizer until the controller has
  removed every `DevPod` with `spec.owner == this user`. A startup flag
  `--orphan-user-devpods=true` changes this to "leave DevPods, just stop
  authenticating new sessions", for operators who prefer that policy.

### 3.2 `DevPod` (namespaced, lives in `devpods` namespace)

```yaml
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: frontend-dev
  namespace: devpods
  labels:
    devpod.io/owner: alice    # mirror of spec.owner, set by controller
spec:
  owner: alice                # must reference an existing User; immutable
  running: true               # manual hibernate switch
  idleTimeoutSeconds: 0       # 0 = disabled; positive = auto-hibernate after idle
  persistence:                # optional block; absence => ephemeral
    size: 50Gi
    storageClassName: ""      # empty => cluster default
  pod:                        # exactly one of pod / vm
    metadata: {}              # PodTemplateSpec metadata (labels/annotations)
    spec:                     # full PodSpec passthrough
      containers:
        - name: dev
          image: ghcr.io/example/devbox:latest
          resources:
            requests: { cpu: "2",  memory: "4Gi" }
            limits:   { cpu: "4",  memory: "8Gi" }
  # vm:                       # alternative; KubeVirt VirtualMachineSpec
  #   ...
status:
  phase: Running              # Pending | Running | Stopped | Failed
  endpoint: "10.244.3.17:22"  # written by controller
  workloadRef:                # actual Pod or VirtualMachine created
    apiVersion: v1
    kind: Pod
    name: frontend-dev-pod
  persistentVolumeClaimRef:
    name: frontend-dev-home
  lastActivityTime: null      # written by gateway
  hibernatedAt: null          # written by controller when running -> false
  conditions: []              # Ready, WorkloadReady, EndpointReady, SidecarHealthy
```

Validation (admission webhook):

- `has(spec.pod) != has(spec.vm)` (CEL).
- `spec.owner` is immutable after creation.
- `spec.owner` refers to an existing `User`.
- `spec.pod.spec.shareProcessNamespace`, when set by the user to `false`,
  is rejected with a message explaining that DevPod requires it for the
  sidecar-nsenter mechanism.
- The name `devpod-sshd` is reserved for the controller-injected sidecar
  container; users cannot use it for their own containers.
- `spec.persistence.size` must parse as a Kubernetes quantity.

Ownership / GC:

- The DevPod owns its rendered `Pod` (or `VirtualMachine`), its `PVC`
  (when persistence is enabled), its `Service`, and its per-DevPod
  `Secret` (sidecar host key). All carry `ownerReferences` to the DevPod,
  so Kubernetes garbage collection cleans them up when the DevPod is
  deleted. `PVC` is no exception: deleting the DevPod deletes its home
  volume. Users wanting to preserve data across delete should hibernate
  instead.
- `User` does not own `DevPod` via `ownerReferences` (cluster-scoped
  resources cannot own namespaced ones in Kubernetes). The User finalizer
  enforces the equivalent on deletion.

### 3.3 `GatewayConfig` (cluster-scoped, singleton named `default`)

```yaml
apiVersion: devpod.io/v1alpha1
kind: GatewayConfig
metadata:
  name: default
spec:
  devPodNamespace: devpods

  hostKeyRef:                       # gateway's externally presented host key
    name: devpod-gateway-host-key
    namespace: devpod-system

  internalKeyRef:                   # gateway's outbound key when dialing backend sshd
    name: devpod-gateway-internal-key
    namespace: devpod-system

  listen:
    port: 22
    proxyProtocol:
      enabled: false
      trustedCIDRs:                 # required when enabled; refuse PROXY header
        - "10.0.0.0/8"              # from any other source

  trustedProxyKeys:                 # upstream SSH-terminating proxies
    - alias: corp-bastion
      pubkey: "ssh-ed25519 AAAA..."

  defaultIdleTimeoutSeconds: 0      # used when a DevPod does not set its own

  sidecarImage: "ghcr.io/<org>/devpod-sshd:v0.1.0"

status:
  readyReplicas: 0
  conditions: []
```

### 3.4 Boundary between user-provided and controller-injected fields

The "only the entry point changes" constraint is enforced concretely:

| Field                                                         | Source                | Notes                                                                                  |
|---------------------------------------------------------------|-----------------------|----------------------------------------------------------------------------------------|
| `spec.pod.spec.containers[*]` (user's)                        | user                  | unchanged                                                                              |
| `spec.pod.spec.initContainers`                                | user + controller     | controller may append (not replace) for future use; v1 appends nothing                 |
| `spec.pod.spec.shareProcessNamespace`                         | controller overrides  | forced to `true`; user `false` rejected by webhook                                     |
| `spec.pod.spec.containers[*]` (sidecar `devpod-sshd`)         | controller append     | name reserved; uses `GatewayConfig.spec.sidecarImage`                                  |
| `spec.pod.spec.volumes` (sidecar host key, optional home PVC) | controller append     | volume names `devpod-sshd-host-keys`, `devpod-home`; reserved                          |
| `spec.vm.template.spec.domain.devices.disks` (cloud-init)     | controller append     | adds a cloudInitNoCloud (or configDrive) disk with gateway pubkey                      |
| all other fields                                              | user                  | unchanged                                                                              |

---

## 4. Data flow

### 4.1 Creating a Pod-backed DevPod

1. User `kubectl apply -f devpod.yaml`.
2. controller observes the new object, validates `spec.owner` resolves to
   an existing `User`.
3. controller renders a Pod whose `spec` is the user's `spec.pod.spec`
   plus the four overlays in §3.4. It creates the Pod, a per-DevPod host-key
   Secret, a headless Service exposing port 22, and (if persistence is
   enabled) a PVC. NetworkPolicies enforcing tenant isolation are
   namespace-wide and per-owner, not per-DevPod (see §5.2); the
   controller ensures they exist when the owner's first DevPod is
   created.
4. When the Pod gets an IP, controller writes
   `status.endpoint = "<podIP>:22"` and `status.phase = Running`.

### 4.2 Creating a VM-backed DevPod

1. controller observes a DevPod with `spec.vm` populated.
2. controller renders a KubeVirt `VirtualMachine`. The user's spec is
   copied unchanged except for the addition of a cloud-init disk whose
   userdata writes the gateway's public key into
   `~/<defaultUser>/.ssh/authorized_keys`.
3. KubeVirt creates the virt-launcher Pod. With default masquerade
   networking, the VM's port 22 is reachable at the virt-launcher Pod's
   IP. controller writes `status.endpoint` accordingly. With bridge or
   multus networking, controller reads `VMI.status.interfaces[0].ipAddress`.

### 4.3 sidecar internals (Pod case)

The sidecar runs sshd with:

```
HostKey /etc/devpod/host/ssh_host_ed25519_key
Port 22
AuthorizedKeysFile /etc/devpod/authorized_keys
PermitRootLogin no
ForceCommand /usr/local/bin/devpod-nsenter "$SSH_ORIGINAL_COMMAND"
Subsystem sftp /usr/local/bin/devpod-nsenter /usr/lib/openssh/sftp-server
AllowAgentForwarding yes
AllowTcpForwarding yes
```

`devpod-nsenter` resolves the user container's main PID (using the
`DEVPOD_TARGET_CONTAINER` env, which the controller sets to
`spec.pod.spec.containers[0].name`) and invokes:

```
nsenter -t <main_pid> -m -u -i -n -p -- <command or login shell>
```

`/etc/devpod/authorized_keys` contains exactly one key: the gateway's
internal-key public half. End-user public keys are never written into
the pod.

### 4.4 SSH connect (direct client)

1. Client `ssh alice+frontend-dev@gateway`.
2. gateway accepts TCP. If `listen.proxyProtocol.enabled` and the peer
   is within `trustedCIDRs`, gateway reads a PROXY v2 header to recover
   the real client IP.
3. gateway speaks SSH. On `userauth-request`, it parses the login user
   into `(alice, frontend-dev)`. If only `alice` is given, the request
   is rejected (no implicit default; the SSH login name must include
   the DevPod name).
4. gateway looks up `User/alice` (informer cache) and tries each of its
   `pubkeys` against the client. On match, `alice` is authenticated.
5. gateway looks up `DevPod devpods/frontend-dev`, verifies
   `spec.owner == "alice"`, verifies `status.phase == Running` and
   `status.endpoint != ""`.
6. gateway opens an outbound SSH connection to `status.endpoint`,
   authenticating with the internal key under the login name `devpod`.
7. gateway pipes SSH channels between the two connections. All SSH
   features (PTY, sftp Subsystem, `direct-tcpip` port-forward,
   `forwarded-tcpip` reverse forward, agent forwarding) ride this
   multiplex transparently.
8. gateway patches `DevPod.status.lastActivityTime` on session open,
   close, and every 60 s while any session is active.

### 4.5 SSH connect (via SSH-terminating outer proxy)

1. The upstream proxy authenticates the end user by whatever means it
   chooses (corporate SSO, OIDC, mTLS, magic links). It is opaque to us.
2. The upstream proxy opens an SSH connection to gateway, presenting one
   of its keys from `GatewayConfig.spec.trustedProxyKeys`, with login
   user `alice+frontend-dev`.
3. gateway recognises the trusted proxy key, skips the
   `User.spec.pubkeys` check, and accepts the login name as the
   authenticated identity.
4. The rest of the flow (DevPod lookup, owner check, dial backend) is
   identical to §4.4.
5. Audit log records `auth_path=trusted_proxy=corp-bastion`.

The schema deliberately does not let `User` resources scope which proxy
may impersonate them; this is a v1 simplification. If finer control is
needed later, it can be added as `GatewayConfig.trustedProxyKeys[].allowedUsers`.

### 4.6 SFTP, VS Code Remote-SSH, port forwarding

These are not special-cased. Because the gateway proxies SSH at the
application layer (not as raw TCP), every SSH channel type works:

- SFTP: client requests `Subsystem sftp`. The channel reaches the
  backend sshd, which launches `devpod-nsenter /usr/lib/openssh/sftp-server`.
  The sftp-server process runs inside the user container's mount
  namespace, so paths are the user's view.
- VS Code Remote-SSH: client opens an `exec` channel running an install
  script. The backend sshd `ForceCommand`s it through `devpod-nsenter`,
  so the install runs in the user container. The VS Code server then
  binds to a localhost port in the user container and asks the SSH
  client for a `direct-tcpip` forward, which the gateway transparently
  proxies.
- VM-backed DevPods: the VM guest's own sshd handles all SSH semantics
  directly. gateway remains a transparent proxy.

### 4.7 Hibernate and wake

- User sets `spec.running: false`.
- controller deletes the workload (Pod or KubeVirt VM). It preserves
  the PVC and Service. The per-owner NetworkPolicy is not per-DevPod
  and is unaffected. It writes `status.phase=Stopped`,
  `status.hibernatedAt=<now>`, `status.endpoint=""`.
- A client connecting to a hibernated DevPod receives an SSH banner
  ("DevPod hibernated; set spec.running=true to wake") and the
  connection is closed cleanly.
- User sets `spec.running: true`. controller recreates the workload,
  the new pod IP becomes the new `status.endpoint`, `phase` returns to
  `Running`. The client just reconnects.

### 4.8 Idle auto-hibernate

- gateway patches `DevPod.status.lastActivityTime` on session open and
  close, and on a 60-second heartbeat while any session is open.
- controller reconciles: if `spec.running == true` and
  `effective_idleTimeout > 0` and
  `now - status.lastActivityTime > effective_idleTimeout`, then
  controller sets `spec.running = false` and emits an `IdleHibernate`
  Event. `effective_idleTimeout` is `spec.idleTimeoutSeconds` if set, else
  `GatewayConfig.spec.defaultIdleTimeoutSeconds`.
- This is a "controller writes spec" pattern (the controller modifies
  the user's intent). It is also how KubeVirt's `VirtualMachine`
  controller works. Users opt out by leaving `idleTimeoutSeconds` at 0.

---

## 5. Security model

### 5.1 Trust zones

```
admin           : full cluster control
 └─ controller  : DevPod / User / GatewayConfig CRUD, all devpods-ns resources
     └─ gateway : holds internalKeyRef; can SSH into any DevPod backend;
                  can patch any DevPod.status (not spec)
         └─ outer proxy : if its pubkey is in trustedProxyKeys,
                          may assert any user's identity
             └─ end user (alice, bob, ...) : untrusted
```

### 5.2 Cross-tenant isolation

Because all DevPods share one namespace, "alice cannot SSH into bob's
pod" is enforced entirely in gateway code. This is the principal
correctness risk of the design and is given dedicated test coverage
(§7.3).

Pod-to-pod network traffic across owners is denied by two layers of
NetworkPolicy in the `devpods` namespace, both managed by the controller:

1. A namespace-wide default-deny policy (ingress: none, egress:
   none-by-default), created once and never removed while the namespace
   exists.
2. One per-owner policy (`devpod-allow-<owner>`), created lazily on the
   owner's first DevPod and deleted with the owner's last DevPod. It
   permits ingress from same-owner DevPods, ingress from the gateway
   on port 22, and egress to DNS and (configurably) the public internet.

This makes the number of NetworkPolicies scale with users, not with
DevPods. Per-DevPod policies are deliberately avoided.

### 5.3 RBAC

- `controller` ServiceAccount: `User/*`, `DevPod/*`, `GatewayConfig/*`;
  inside `devpods`: `Pod/Service/PVC/NetworkPolicy/Secret` full CRUD;
  inside `devpods`: `kubevirt.io/VirtualMachine/*` (controlled by a build
  flag or capability check so kubevirt is an optional dep).
- `gateway` ServiceAccount: `User get/list/watch`,
  `DevPod get/list/watch + status patch`, `GatewayConfig get/watch`,
  named `Secret get` for the two key Secrets only. Notably, gateway
  cannot write `DevPod.spec`. Idle hibernation is the controller's job.

### 5.4 Secrets

- Gateway host key, gateway internal key: cluster-wide; in
  `devpod-system` Secrets named by `GatewayConfig`.
- Per-DevPod sidecar host key: a Secret owned by the DevPod, generated
  by the controller, mounted only into that DevPod's sidecar.
- Sidecar's authorized_keys contains exactly one key: the gateway
  internal-key public half. No end-user public keys reach the pod.
- Key rotation is not in v1. Adding it requires (a) running two
  internal keys in parallel during a rotation window, and (b) sidecars
  trusting both. Out of scope for v1; flagged as a v2 concern.

### 5.5 Auditing

Each session emits a structured log line at open and close:

```
{ts, session_id, user, devpod, auth_path, pubkey_fingerprint,
 client_ip, bytes_in, bytes_out, duration_seconds, close_reason}
```

`auth_path` is one of `direct` or `trusted_proxy=<alias>`.
`pubkey_fingerprint` is recorded (not the key itself) to make
"which device of alice connected" answerable.

Failed authentications are also logged with the same structure (minus
duration / bytes). Rate limiting beyond logging is the outer LB's
concern; v1 only records.

---

## 6. Error handling

| Condition                                            | DevPod state                                             | Client experience                                            |
|------------------------------------------------------|----------------------------------------------------------|--------------------------------------------------------------|
| `spec.owner` references missing `User`               | `phase=Failed`, `condition UserMissing`                  | gateway returns banner and disconnects                       |
| User deleted while DevPod exists                     | finalizer blocks User delete until DevPod removed        | (administrative)                                             |
| Pod image cannot be pulled                           | `phase=Pending`, `condition WorkloadReady=False`         | gateway returns "endpoint not ready" banner                  |
| Sidecar crashes                                      | Pod restarts the sidecar; condition `SidecarHealthy`     | new connections fail; existing sessions break                |
| Backend dial fails (sshd not up, key mismatch)       | Event `DialFailed{reason}`, metric increment             | gateway retries 3× (1 s apart), then disconnects             |
| Pod evicted or rescheduled (new IP)                  | controller updates `status.endpoint`                     | in-flight sessions break; client reconnects normally         |
| gateway replica crashes mid-session                  | -                                                         | client connection drops; reconnect routes to a live replica  |
| apiserver briefly unreachable                        | informer cache serves reads; status patches retried      | already-authenticated sessions unaffected                    |
| Webhook rejected (xor violation, reserved name, etc.)| object never admitted                                    | `kubectl apply` returns a clear validation error             |

---

## 7. Testing

### 7.1 Unit

Controller logic uses `envtest` (controller plane only, no kubelet).
Assertions cover:

- Pod-backed render: sidecar injected, `shareProcessNamespace` forced,
  PVC volume appears iff `spec.persistence` is set, NetworkPolicy and
  Service created.
- VM-backed render: cloud-init disk added with the expected userdata;
  other VirtualMachine fields untouched.
- Hibernate / wake: pod deleted on `running=false`, PVC preserved, pod
  recreated on `running=true`, `status.endpoint` cleared and rewritten.
- Finalizers: User deletion blocked until owned DevPods are gone.
- Status condition projection from Pod conditions to DevPod conditions.

Gateway logic uses an in-process pair (mock SSH client and mock backend
sshd). Assertions cover:

- Login-name parsing: `alice+frontend-dev` accepted; `alice`,
  `alice+`, `+pod`, `alice+pod+extra`, `alice+../pod` rejected.
- Pubkey auth match / mismatch.
- Trusted-proxy path: gateway accepts identity claim only when peer key
  is in `trustedProxyKeys`; spoofed claims with an unknown key fail.
- Owner enforcement: client authenticated as `alice` cannot reach
  `bob`-owned DevPods (gateway returns disconnect; no proxy attempt).
- `status.lastActivityTime` patched on open/close and every 60 s while
  active.
- Dial retries, dial failure cleanup, fd accounting.

### 7.2 Webhook

- mutating: sidecar injection, `shareProcessNamespace` enforcement,
  cloud-init userdata addition.
- validating: `pod xor vm`, immutable `owner`, reserved container name,
  shareProcessNamespace=false rejection, `User` existence.

### 7.3 End-to-end (kind cluster, per PR)

A kind cluster is bootstrapped, the chart is installed, and a real `ssh`
client is used to exercise:

1. `ssh alice+frontend-dev@gateway`: shell lands in the user container;
   `whoami` returns the user-container default user.
2. `scp` round-trip.
3. `ssh -L 8080:localhost:3000` port forward; pod-side `nc -lk 3000` is
   reachable from the local side.
4. VS Code-style exec: `ssh alice+frontend-dev@gateway -- bash -lc 'cat /etc/os-release'`
   returns the user-container OS, not the sidecar's.
5. Cross-tenant attempt: `ssh bob+alice-pod@gateway` (bob's key is
   valid, but the DevPod belongs to alice) is rejected. Metric
   `devpod_gateway_auth_failures_total{reason="owner_mismatch"}` increments;
   DevPod has an Event recording the rejection.
6. Hibernate: `kubectl patch ... spec.running=false`; next connect gets
   a banner. `spec.running=true`; connect succeeds.
7. Idle suspend: `spec.idleTimeoutSeconds=30`; no connections for 35 s;
   `spec.running` becomes false; `Event IdleHibernate` present.
8. PROXY protocol v2: haproxy fronted; gateway logs the real client IP.
9. Trusted proxy: a mock proxy with a known pubkey connects on behalf
   of alice with no client-side User key; succeeds.
10. KubeVirt VM-backed (skipped if cluster lacks KubeVirt): an
    Ubuntu cloud image VM, gateway pubkey injected via cloud-init;
    `ssh alice+vmpod@gateway` lands in the VM.

### 7.4 Negative / fuzz

- Malformed SSH login names (boundary cases above).
- High concurrent auth-failure rate: confirm bounded fd and memory.
- gateway `kill -9` mid-session: no leaked sshd connections to the
  backend.

---

## 8. Observability

Metrics (Prometheus, both processes):

```
# gateway
devpod_gateway_active_sessions{user, devpod}
devpod_gateway_sessions_total{user, devpod, auth_path, result}
devpod_gateway_dial_failures_total{devpod, reason}
devpod_gateway_auth_failures_total{reason, auth_path}
devpod_gateway_session_duration_seconds_bucket
devpod_gateway_bytes_total{direction, devpod}      # optional, may be costly

# controller
devpod_controller_reconcile_total{kind, result}
devpod_controller_reconcile_duration_seconds
devpod_devpods_phase{phase}                        # gauge
devpod_devpods_running                             # gauge
devpod_idle_hibernates_total
```

Events on the DevPod object: `PodCreated`, `PVCCreated`, `VMCreated`,
`EndpointReady`, `IdleHibernate`, `Hibernated`, `Woken`, `UserMissing`,
`OwnerMismatch`, `DialFailed`, `SidecarUnhealthy`.

DevPod status conditions: `Ready`, `WorkloadReady`, `EndpointReady`,
`SidecarHealthy` (Pod only).

---

## 9. Implementation milestones

Each milestone leaves the previous one's tests green and delivers a
user-visible capability.

- **M0 (cross-cutting):** kubebuilder scaffold, CRD generation,
  controller-gen, Helm chart skeleton (controller, gateway, webhook,
  CRDs, RBAC), CI running unit + envtest + kind e2e.

- **M1 — Minimal Pod-backed SSH:** `User`, `DevPod` (pod only),
  `GatewayConfig`; controller producing Pod + sidecar + Service +
  per-DevPod host-key Secret; single-replica gateway with direct
  User-pubkey auth. Demonstration: `ssh alice+pod@gateway` reaches a
  shell, `scp` works.

- **M2 — Persistence and manual hibernate:** `spec.persistence`,
  `spec.running`; PVC owned by DevPod and preserved across hibernate
  cycles. Demonstration: write file, hibernate, wake, file is still
  there.

- **M3 — Multi-replica gateway, PROXY protocol, trusted proxies:**
  gateway becomes a stateless multi-replica Deployment sharing host
  and internal keys via Secrets; PROXY protocol v2 listener with
  `trustedCIDRs`; `trustedProxyKeys` enforced; audit `auth_path`
  field populated.

- **M4 — KubeVirt VM-backed:** `spec.vm` accepted (xor webhook
  activated); controller renders a VirtualMachine with cloud-init
  injection; `status.endpoint` derives from virt-launcher pod IP or
  VMI interface.

- **M5 — Idle auto-hibernate:** gateway patches `lastActivityTime`;
  controller reconciles `idleTimeoutSeconds`; cluster default via
  `GatewayConfig`.

---

## 10. Open questions and explicit deferrals

- Key rotation for the gateway internal key. Out of scope for v1.
  Sketch: dual-key window, sidecars trust two pubkeys during rotation;
  documentation only.
- Per-user resource quota. Deliberately omitted (per-pod resources via
  Kubernetes are sufficient for now).
- Multi-cluster / federation. Out of scope.
- Whether `User` should be able to allow-list specific
  `trustedProxyKeys` aliases. Not in v1; can be added without breaking
  changes by extending `GatewayConfig.trustedProxyKeys[]` or `User`.
- A "wake on connect" mode (gateway transparently un-hibernates a
  DevPod when a connection arrives) would improve UX significantly
  but adds non-trivial latency surprise and was not chosen for v1.
