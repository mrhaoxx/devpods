# DevPodSnapshot Design

**Status:** Draft
**Date:** 2026-05-22

Snapshot a running DevPod container into an OCI image using `docker commit`,
triggered by a `DevPodSnapshot` custom resource.

---

## 1. Goals and non-goals

### Goals

- A `DevPodSnapshot` CRD that captures the user container's filesystem delta
  (overlay upper layer) as an OCI image and pushes it to a registry.
- Incremental: only the changes the user made on top of the base image are
  stored, via Docker's native commit semantics.
- Clean snapshots: DevPod infrastructure files (supervisor binary, sshd
  config, host keys, `/etc/passwd` patches) are injected via volume mounts
  and therefore excluded from the committed layer automatically.
- No user interruption: snapshot runs against a live container. Docker's
  commit does a brief pause→snapshot→unpause internally (~1 s).
- Status tracking: each `DevPodSnapshot` reports phase progression
  (Pending → Running → Succeeded / Failed) and the pushed image digest.
- Multiple snapshots per DevPod; each is an independent resource with its
  own lifecycle.

### Non-goals

- Full VM snapshot (KubeVirt `VirtualMachineSnapshot` is a separate
  mechanism).
- Process-state checkpoint (CRIU). This captures filesystem only.
- Restoring a DevPod from a snapshot image (future work — user can already
  set their DevPod's container image to the snapshot tag manually).
- Garbage-collecting old snapshot images from the registry.
- Streaming build logs to the user in real time.

---

## 2. Prerequisites — volume-mount injection

Today the supervisor patches `/etc/passwd` (and potentially other system
files) by writing directly to the container filesystem at runtime. These
writes would pollute the committed image.

**Required change before snapshot ships:** convert all DevPod infrastructure
file writes to volume-mount injection so they live outside the overlay upper
layer. Specifically:

1. `/etc/passwd` (and `/etc/shadow`, `/etc/group` if modified) — the
   initContainer reads the base image's original file, applies the home-dir
   patch, writes the result to a shared emptyDir, which is then bind-mounted
   over the original path in the user container.
2. Any sshd runtime config that the supervisor writes into the user
   container's filesystem — same emptyDir pattern.
3. Audit `cmd/supervisor/` for other filesystem writes and convert them.

After this change, `docker commit` on the user container captures only
user-installed packages, config changes, and data.

---

## 3. CRD — `DevPodSnapshot`

```yaml
apiVersion: devpod.io/v1alpha1
kind: DevPodSnapshot
metadata:
  name: my-snapshot-1
  namespace: devpods          # same namespace as DevPods
spec:
  # Required. Name of the DevPod to snapshot.
  devPodName: my-devpod

  # Required. Target image reference including tag.
  image: registry.example.com/snapshots/my-devpod:v1

  # Optional. Secret of type kubernetes.io/dockerconfigjson for push auth.
  # If omitted, the Job runs without explicit auth (works for registries
  # that trust the node's configured credentials).
  pushSecretRef:
    name: my-registry-creds

status:
  # Phase: Pending | Running | Succeeded | Failed
  phase: Succeeded

  # OCI digest of the pushed image (set on success).
  digest: "sha256:abc123..."

  # Human-readable message (set on failure).
  message: ""

  # Reference to the snapshot Job.
  jobRef:
    name: snapshot-my-snapshot-1
    namespace: devpods

  # Standard conditions.
  conditions:
    - type: Complete
      status: "True"
      lastTransitionTime: "2026-05-22T10:00:00Z"
```

### Validation rules

- `spec` is immutable after creation (CEL `x == oldSelf` on update).
- `spec.devPodName` must reference an existing, running DevPod in the same
  namespace (webhook validates at admission; controller also checks).
- `spec.image` must be a valid OCI reference (regex validation).
- `spec.pushSecretRef.name`, if set, must be non-empty.

### Printer columns

```
NAME          DEVPOD       IMAGE                          PHASE       AGE
my-snapshot   my-devpod    reg.example.com/snap:v1        Succeeded   5m
```

---

## 4. Controller — `DevPodSnapshotReconciler`

Lives in `internal/controllers/snapshot_controller.go`. Registered in the
existing `cmd/controller` manager.

### Reconcile flow

```
Get DevPodSnapshot
  ↓ NotFound → return
  ↓
Phase terminal (Succeeded|Failed)?
  → return (no-op)
  ↓
Get target DevPod
  ↓ NotFound or not Running → set Failed, patch status, return
  ↓
Get DevPod's Pod
  ↓ not Running → set Failed, patch status, return
  ↓
Find docker container ID for user container (containers[0])
  from pod.status.containerStatuses[].containerID
  ↓ not found → set Failed, return
  ↓
Job already exists? (.status.jobRef)
  ↓ yes → check Job status:
  │   Succeeded → extract digest from Job logs/annotation,
  │               set phase=Succeeded, patch status
  │   Failed    → set phase=Failed, copy message, patch status
  │   Active    → requeue (wait)
  ↓ no →
Set phase=Running, patch status
  ↓
Render snapshot Job (see §5)
  ↓
Apply Job via applyOwned (SetControllerReference)
  ↓
Requeue
```

### Watches

```go
ctrl.NewControllerManagedBy(mgr).
    For(&DevPodSnapshot{}).
    Owns(&batchv1.Job{}).
    Complete(r)
```

The controller requeues when its owned Job changes status, so it learns
about completion/failure via the normal informer path.

### Extracting the digest

The Job's container writes the pushed digest to a well-known annotation on
itself before exiting:

```
devpod.io/snapshot-digest: sha256:abc123...
```

The controller reads this annotation from the completed Job. This avoids
parsing logs.

Alternative: the Job writes the digest to a shared emptyDir file and a
second container (or the controller) reads it. The annotation approach is
simpler — the docker CLI script can `kubectl annotate` the Job, but that
requires a ServiceAccount. Simpler: the script writes the digest to stdout,
and the controller reads it from the Pod's terminated container status
message (the container's last 2048 bytes of log are available via
`pod.status.containerStatuses[].state.terminated.message` if
`terminationMessagePolicy: FallbackToLogsOnError`). Use
`terminationMessagePath` — the Job container writes the digest to
`/dev/termination-log`, and the controller reads it from the Pod status.
Zero extra permissions needed.

---

## 5. Job rendering

Pure function in `internal/render/snapshot.go`.

```go
func SnapshotJob(
    snap *v1alpha1.DevPodSnapshot,
    containerID string,    // "docker://abc123" → "abc123"
    nodeName string,
    pushSecret *string,    // nil if no auth
) *batchv1.Job
```

### Job spec

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: snapshot-<devpodsnapshot-name>
  namespace: <same as snapshot>
  labels:
    devpod.io/snapshot: <snapshot-name>
    devpod.io/devpod: <devpod-name>
spec:
  ttlSecondsAfterFinished: 300   # auto-cleanup after 5 min
  backoffLimit: 0                # no retry — fail fast
  template:
    spec:
      restartPolicy: Never
      nodeName: <target pod's node>     # must run on same node
      containers:
        - name: snapshot
          image: docker:cli
          command: ["sh", "-c"]
          args:
            - |
              docker commit "$CONTAINER_ID" "$TARGET_IMAGE" &&
              docker push "$TARGET_IMAGE" 2>&1 | tee /tmp/push.log &&
              grep -oP 'digest: \K\S+' /tmp/push.log \
                > /dev/termination-log
          env:
            - name: CONTAINER_ID
              value: "<stripped container ID>"
            - name: TARGET_IMAGE
              value: "<spec.image>"
            - name: DOCKER_CONFIG  # only if pushSecretRef set
              value: /root/.docker
          volumeMounts:
            - name: docker-sock
              mountPath: /var/run/docker.sock
            - name: push-secret        # only if pushSecretRef set
              mountPath: /root/.docker
              readOnly: true
          terminationMessagePath: /dev/termination-log
          terminationMessagePolicy: File
      volumes:
        - name: docker-sock
          hostPath:
            path: /var/run/docker.sock
            type: Socket
        - name: push-secret            # only if pushSecretRef set
          secret:
            secretName: <pushSecretRef.name>
            items:
              - key: .dockerconfigjson
                path: config.json
```

### Key decisions

- **`nodeName`** (not nodeAffinity): the Job must land on the exact node
  where the container lives. `nodeName` is a hard assignment, no scheduling
  ambiguity.
- **`ttlSecondsAfterFinished: 300`**: Job and its Pod are garbage-collected
  5 minutes after completion. Enough time for the controller to read the
  termination message.
- **`backoffLimit: 0`**: snapshot is a point-in-time operation; retrying
  makes no semantic sense (the container state will have changed).
- **No ServiceAccount / RBAC needed**: the Job doesn't talk to the K8s API.
  It only needs the Docker socket (hostPath) and optionally the push secret.

---

## 6. Security

### Docker socket access

The snapshot Job gets read-write access to the Docker socket on its node.
This is a privileged operation — a compromised Job could affect any
container on the node.

Mitigations:
- Job is created by the controller, not by the user. Users create
  `DevPodSnapshot` CRs; RBAC controls who can create them.
- Job has `backoffLimit: 0` and TTL cleanup — short-lived.
- The Job image is `docker:cli` (hardcoded by the controller, not
  user-configurable). No user-controlled code runs in the Job.
- The Job runs only `docker commit` + `docker push` with controller-derived
  arguments. No shell injection vector — container ID and image ref are
  validated by the webhook before the Job is rendered.

### RBAC

New ClusterRole rules for the controller:

```yaml
- apiGroups: [devpod.io]
  resources: [devpodsnapshots]
  verbs: [get, list, watch, patch]
- apiGroups: [devpod.io]
  resources: [devpodsnapshots/status]
  verbs: [patch]
- apiGroups: [batch]
  resources: [jobs]
  verbs: [get, list, watch, create, patch, delete]
```

User-facing RBAC (example):

```yaml
- apiGroups: [devpod.io]
  resources: [devpodsnapshots]
  verbs: [get, list, create, delete]
```

### Image reference validation

The webhook must reject `spec.image` values containing shell
metacharacters. A strict OCI reference regex
(`[a-zA-Z0-9][a-zA-Z0-9._-]*(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?`)
is applied at admission.

### Concurrency

Only one active (non-terminal) `DevPodSnapshot` per DevPod at a time.
The webhook rejects creation if another snapshot for the same `devPodName`
is in Pending or Running phase. This prevents overlapping commits that
could produce inconsistent images.

---

## 7. Lifecycle and status

```
User creates DevPodSnapshot CR
  ↓
Controller sets phase=Pending
  ↓ validates DevPod exists and is running
Controller renders Job, applies it
  ↓ sets phase=Running, records jobRef
Job runs docker commit + push
  ↓ writes digest to /dev/termination-log
Job completes
  ↓
Controller reads termination message, extracts digest
  ↓ sets phase=Succeeded, records digest
  ↓ OR if Job failed: sets phase=Failed, records message

DevPodSnapshot CR persists as a record.
Job is auto-deleted after TTL (300s).
User can delete the DevPodSnapshot CR at any time.
```

---

## 8. Helm chart changes

- New CRD: `devpod.io_devpodsnapshots.yaml`
- Controller Deployment: no changes (same binary, new reconciler auto-
  registers)
- Controller ClusterRole: add rules for `devpodsnapshots`, `jobs`
- Optional: user-facing ClusterRole for creating snapshots

---

## 9. Testing

### Unit tests

- `internal/render/snapshot_test.go`: Job rendering with/without push
  secret, label correctness, volume mounts, termination message config.
- `internal/controllers/snapshot_controller_test.go` (envtest): reconcile
  with mock DevPod+Pod, Job creation, status transitions on Job
  completion/failure, rejection of snapshot for non-running DevPod,
  concurrency guard.

### E2E tests

- Create a DevPod, wait for Running.
- Create a DevPodSnapshot targeting a local registry (kind + registry
  sidecar).
- Wait for Succeeded phase.
- Verify the image exists in the registry with the reported digest.
- Verify the image does NOT contain `/opt/devpod/` (volume-mount
  injection prerequisite).

---

## 10. Future work

- **Restore from snapshot**: auto-set the DevPod's container image to a
  snapshot tag. Today the user does this manually.
- **Scheduled snapshots**: CronJob-style periodic snapshotting.
- **Registry garbage collection**: expire old snapshot images based on
  retention policy.
- **Progress reporting**: stream `docker push` progress to a status field
  or event.
- **containerd support**: if clusters migrate off Docker, implement the
  equivalent via `ctr` / containerd Go SDK.
