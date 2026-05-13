# DevPod M2 — Persistence and Manual Hibernate

**Status:** Approved
**Date:** 2026-05-12
**Depends on:** `2026-05-12-devpod-design.md` (the umbrella spec); M0 + M1 implementation already merged.

---

## 1. Goal

Deliver the M2 milestone from the umbrella spec:

> Persistence and manual hibernate: `spec.persistence`, `spec.running`; PVC
> owned by DevPod and preserved across hibernate cycles.
> Demonstration: write file, hibernate, wake, file is still there.

This document is the implementation-level spec. It also pulls in two
M1 follow-ups that are blocking or natural to ship together:

- Wire the validating webhook into `cmd/controller` and the Helm chart
  (FOLLOWUPS.md §"Final-review gaps").
- Fill the remaining `DevPodStatus` fields
  (`PersistentVolumeClaimRef`, `HibernatedAt`).

Non-goals (deferred to later milestones):

- Idle auto-hibernate (M5).
- Wake-on-connect at the gateway (umbrella spec §10, out of scope for v1).
- Snapshot / backup of the home PVC.
- PV reclaim policies beyond "leave the PVC alone".

---

## 2. API additions

### 2.1 `PersistenceSpec` (extended)

```go
// PersistenceSpec opts a DevPod into a persistent home volume.
//
// When set, the controller creates a PVC named <owner>-<name>-home,
// injects a volume "devpod-home" referencing that PVC into spec.pod,
// and mounts it on TargetContainer (or the first container if unset)
// at MountPath.
type PersistenceSpec struct {
    // Size of the home PVC, parsed as a Kubernetes resource quantity.
    Size string `json:"size"`

    // StorageClassName for the home PVC. Empty string uses the cluster default.
    //
    // +optional
    StorageClassName string `json:"storageClassName,omitempty"`

    // AccessModes for the home PVC. Defaults to [ReadWriteOnce]. The
    // chosen StorageClass must support every mode listed here; otherwise
    // PVC binding fails and the DevPod stays in Pending.
    //
    // +optional
    // +kubebuilder:default={ReadWriteOnce}
    // +listType=set
    AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

    // MountPath is where the PVC is mounted inside the target container.
    // Required. Must be an absolute path and must not collide with any
    // user-supplied volumeMount.mountPath on the target container — the
    // validating webhook enforces this.
    //
    // +kubebuilder:validation:Pattern=`^/[^\s]*$`
    MountPath string `json:"mountPath"`

    // TargetContainer names which container in spec.pod.spec.containers
    // receives the home mount. Defaults to spec.pod.spec.containers[0].name.
    // Must reference an existing container; webhook enforces this.
    //
    // +optional
    TargetContainer string `json:"targetContainer,omitempty"`
}
```

Existing fields (`Running`, `IdleTimeoutSeconds`) are unchanged.

### 2.2 `DevPod` metadata

Add `+kubebuilder:validation:MaxLength=22` to `DevPod.metadata.name`
to lock in the length budget cited in §3.1. This is a FOLLOWUPS
item we're picking up because §3.1 depends on it.

### 2.3 `DevPodStatus` (no schema change, fill remaining fields)

The schema for `PersistentVolumeClaimRef` and `HibernatedAt` is already
present (M0). M2 makes the controller actually write them; see §5.

---

## 3. PVC lifecycle

### 3.1 Names and length budget

- PVC name = `render.HomePVCName(dp)` = `<owner>-<dp.Name>-home`.
  Length budget: 32 (User regex max) + 1 + 22 (DevPod MaxLength,
  added in this milestone — see §2.3) + 5 = 60 ≤ 63
  (DNS-1123 subdomain). Fits.
- Volume name in the injected Pod spec = `devpod-home`. Reserved
  for the controller; user-supplied volumes / volumeMounts named with
  the `devpod-` prefix are rejected at admission.

### 3.2 Creation

`internal/render` grows `HomePVC(dp *DevPod) *corev1.PersistentVolumeClaim`.
The controller calls `applyOwned` so the PVC carries an ownerRef back
to the DevPod (used by the GC fallback in §3.3) and is reconciled via
`Owns(&corev1.PersistentVolumeClaim{})`.

When `spec.persistence == nil`, no PVC is created and no home volume
is injected.

### 3.3 Deletion — finalizer detaches before GC

Default behavior is **retain** (data survives `kubectl delete devpod`).
Implementation:

1. The PVC is created with `metav1.OwnerReferences[Controller=true]`
   pointing at the DevPod, *and* the existing
   `devpod.io/devpod-cleanup` finalizer is on the DevPod.
2. When the DevPod is being deleted (`DeletionTimestamp != nil`), the
   finalizer handler:
   a. Lists/Gets the home PVC, if present.
   b. Strips the DevPod ownerRef (and any controller marker) so K8s
      garbage collection will not delete the PVC.
   c. Removes the finalizer from the DevPod, allowing it to vanish.
3. The detached PVC becomes a tenant-owned, "release" artifact. The
   operator / user is responsible for `kubectl delete pvc <name>` when
   they're done with the data.

Failure mode: if the PVC ownerRef strip fails (transient API error),
the finalizer reconcile returns the error and the DevPod stays
visible. Observable; retryable.

### 3.4 Mounting into spec.pod (injection)

`render.Pod` is extended:

- If `dp.Spec.Persistence != nil`:
  - Append `corev1.Volume{Name: "devpod-home", VolumeSource:
    {PersistentVolumeClaim: {ClaimName: render.HomePVCName(dp)}}}` to
    the rendered pod's `spec.volumes`.
  - Locate the target container (`spec.persistence.targetContainer` or
    `containers[0]`). Append
    `corev1.VolumeMount{Name: "devpod-home", MountPath:
    spec.persistence.mountPath}` to that container's `volumeMounts`.
- Otherwise: no injection.

Injection follows the existing sidecar-injection pattern in
`render.Pod`. The render layer assumes the webhook has already
rejected reserved-name collisions and missing target containers;
defensive checks remain (returning an error) so unit-tested misuse
fails closed.

---

## 4. Hibernate state machine

### 4.1 Desired state derived from `spec.running`

- `running == true` (default): apply Pod + Service + Secret + NetworkPolicy
  + (optional) PVC. This is the M1 behavior plus PVC.
- `running == false`: ensure Pod + Service Endpoints absent; Pod object
  deleted via `client.Delete`. Service, host-key Secret, NetworkPolicies,
  and PVC are **not** touched.

### 4.2 Edge cases

- **Active SSH sessions during a `running=false` flip.** The Pod
  deletion drops them. No drain in M2.
  Documented behavior; gateway sees `clientConn.Wait()` return with a
  network error and tears down the session cleanly (the M1
  proxy-channel fix handles this).
- **Pod still terminating when we re-flip to `running=true`.** Apply
  is idempotent: `Create` returns AlreadyExists or the Pod is finally
  gone and Create succeeds. Reconcile retries normally.
- **PVC missing on resume.** If the user deleted the PVC while
  hibernated, the controller will recreate it (empty) on the next
  reconcile. Documented; not surfaced as an error.

### 4.3 Idempotence

Two consecutive reconciles must produce the same result. The Pod-delete
path uses GET-then-DELETE; an AlreadyDeleted Pod is a no-op.

---

## 5. Status reporting

`DevPodReconciler.updateStatus` is rewritten to compute the full status
in one place:

```text
desired := DevPodStatus{
    Phase:                    DevPodPending,    // or as computed below
    Endpoint:                 "",
    WorkloadRef:              nil,
    PersistentVolumeClaimRef: nil,
    HibernatedAt:             dp.Status.HibernatedAt,  // sticky unless we transition out
    Conditions:               dp.Status.Conditions,    // untouched here
}

if spec.persistence != nil {
    desired.PersistentVolumeClaimRef = &LocalObjectRef{Name: HomePVCName(dp)}
}

if spec.running == false {
    desired.Phase = DevPodStopped
    if dp.Status.HibernatedAt == nil || dp.Status.Phase != DevPodStopped {
        now := metav1.Now()
        desired.HibernatedAt = &now
    }
} else {
    desired.HibernatedAt = nil  // cleared on resume
    if Pod fetched OK {
        desired.WorkloadRef = &WorkloadRef{Kind: "Pod", Name: pod.Name, APIVersion: "v1"}
        switch {
        case pod.Status.Phase == Running && pod.Status.PodIP != "":
            desired.Phase = DevPodRunning
            desired.Endpoint = pod.Status.PodIP + ":22"
        case pod.Status.Phase == Failed:
            desired.Phase = DevPodFailed
        default:
            desired.Phase = DevPodPending
        }
    }
}

if equal(dp.Status, desired) { return nil }
patch & set & Status().Patch(...)
```

The Pod fetch is conditional: only when `running == true`. When
hibernated, we skip the Get to avoid spurious "not found" noise in
logs.

---

## 6. Webhook deployment + new rules

### 6.1 Migration to `CustomValidator`

Existing `DevPodValidator` uses the legacy `Handle` + `InjectDecoder`
shape, which is dead code in controller-runtime v0.20 (FOLLOWUPS
§"Validating webhook"). M2 migrates to `admission.CustomValidator`:

```go
type DevPodValidator struct { /* … */ }
var _ admission.CustomValidator = (*DevPodValidator)(nil)

func (v *DevPodValidator) ValidateCreate(ctx, obj) (admission.Warnings, error)
func (v *DevPodValidator) ValidateUpdate(ctx, oldObj, newObj) (admission.Warnings, error)
func (v *DevPodValidator) ValidateDelete(ctx, obj) (admission.Warnings, error)
```

Registration in `cmd/controller`:

```go
err := ctrl.NewWebhookManagedBy(mgr).
    For(&devpodv1alpha1.DevPod{}).
    WithValidator(&webhook.DevPodValidator{Client: mgr.GetClient()}).
    Complete()
```

The existing test suite migrates to the typed validator interface.

### 6.2 New validation rules (M2 additions)

Rules applied in `ValidateCreate` and `ValidateUpdate`:

1. **Reserved volume names.** Reject if any
   `spec.pod.spec.volumes[].name` has the `devpod-` prefix.
2. **Reserved volumeMount names.** Reject if any
   `spec.pod.spec.containers[].volumeMounts[].name` has the
   `devpod-` prefix.
3. **Persistence: target container exists.** If
   `spec.persistence != nil` and `spec.persistence.targetContainer`
   is set, reject if no container in `spec.pod.spec.containers`
   matches.
4. **Persistence: mount path collision.** If
   `spec.persistence != nil`, reject if its `mountPath` equals or
   is a prefix-match of any user-supplied `volumeMount.mountPath`
   on the target container.
5. **Persistence: size parses as resource.Quantity.** Reject
   unparseable strings at admission rather than at PVC create.
   (This subsumes FOLLOWUPS §"PersistenceSpec.Size ... resource.Quantity".)

Existing M1 rules (sidecar name reservation, shareProcessNamespace,
owner immutability, empty containers) survive the migration unchanged.

### 6.3 Chart wiring

`deploy/chart/templates/` grows:

- `Certificate.yaml` — cert-manager-issued, `devpod-webhook-tls`.
- `Issuer.yaml` — namespace-local self-signed issuer for the webhook cert.
- `ValidatingWebhookConfiguration.yaml` — failurePolicy `Fail`,
  matchPolicy `Equivalent`, scope `Namespaced`, selector
  `matchExpressions: [...]` so the webhook itself isn't intercepted.

`cmd/controller` learns `--webhook-port` and `--webhook-cert-dir`
flags; defaults match cert-manager's mount conventions
(`/tmp/k8s-webhook-server/serving-certs`). The Deployment is updated
to mount the cert.

cert-manager dependency is documented as a soft prereq for v1alpha1.
For local dev (`hack/e2e-up.sh`), cert-manager is installed by the
script if not present (or, simpler in M2: install once, document in
README; revisit if e2e flakes).

---

## 7. Test plan

### 7.1 Unit tests

- `internal/render/pod_test.go`:
  - With `spec.persistence == nil`: no `devpod-home` volume, no
    home mount.
  - With `spec.persistence != nil`: `devpod-home` volume present,
    mount on `targetContainer || containers[0]` at `mountPath`.
  - With `spec.persistence != nil` and `spec.pod` containing a
    `devpod-foo` volume / mount: render returns an error (the
    webhook should have caught this; defense-in-depth).

- `internal/render/pvc_test.go` (new):
  - Default access modes when omitted = `[ReadWriteOnce]`.
  - `storageClassName == ""` produces no StorageClassName field
    (cluster default), not the literal empty string.
  - Size parsed via `resource.ParseQuantity`; invalid input is a
    render-time error.

- `internal/webhook/devpod_webhook_test.go` (extended):
  - Reserved-prefix rule (volumes and volumeMounts).
  - Mount path collision rule.
  - Unknown target container rule.
  - Unparseable size rule.
  - Migration to typed validator: existing rules still pass.

### 7.2 envtest (integration)

`internal/controllers/devpod_controller_test.go`:

- **Persistence on**: Create DevPod with `spec.persistence`; assert
  PVC exists, `status.persistentVolumeClaimRef` populated, Pod
  spec contains `devpod-home` volume + mount.

- **Hibernate roundtrip**: Create DevPod (running=true). After Pod
  visible, patch `spec.running=false`. Assert Pod deleted within
  10s; assert `status.phase=Stopped`, `status.endpoint==""`,
  `status.workloadRef==nil`, `status.hibernatedAt != nil`,
  PVC still present. Patch back to `spec.running=true`; assert
  Pod recreated, `status.hibernatedAt == nil`, PVC still the
  same (UID match).

- **Delete-detaches-PVC**: Create DevPod (persistence on). Delete
  the DevPod. Assert DevPod gone; assert PVC still present and
  no longer has the DevPod in its ownerRefs.

- **Reserved-name collision via render (webhook-bypass)**: feed
  the controller a DevPod whose `spec.pod` has a `devpod-` volume
  name. Render returns an error; reconcile surfaces it (the
  webhook should catch this in production, but the integration
  test guarantees the render layer is the final defense).

### 7.3 End-to-end (OrbStack)

Run via `hack/e2e-up.sh` and a new `hack/e2e-m2.sh` that drives:

1. `kubectl apply` a DevPod with `spec.persistence: {size: 1Gi,
   mountPath: /workspace}` and a debian image.
2. `ssh alice+m2demo@gw -- 'echo hello > /workspace/marker'`
3. `kubectl patch devpod m2demo -p '{"spec":{"running":false}}' --type=merge`
4. Wait for `status.phase=Stopped` and Pod absent.
5. `kubectl patch devpod m2demo -p '{"spec":{"running":true}}' --type=merge`
6. Wait for `status.phase=Running` and `status.endpoint` non-empty.
7. `ssh alice+m2demo@gw -- 'cat /workspace/marker'` → `hello`.

---

## 8. Risks and mitigations

- **PVC ownerRef strip race**. If the finalizer detaches the
  ownerRef but K8s GC already enqueued a delete, the PVC could
  still vanish. controller-runtime's
  `apierrors.IsNotFound` on the post-strip Get is a benign no-op
  in the controller's eyes. Mitigation: strip-then-verify (Get
  again, confirm no controller ownerRef) inside the finalizer
  before removing the DevPod finalizer marker.

- **Hibernate during Pod ContainerCreating**. Deleting a Pod
  mid-create works (kubelet aborts). No mitigation needed.

- **Webhook bootstrap order**. cert-manager must be installed
  before the chart; otherwise the webhook server has no TLS cert
  and the apiserver's mutating/validating call fails with TLS
  errors. Document in README and the chart `NOTES.txt`.

- **AccessModes ↔ StorageClass mismatch**. If the user requests
  `[ReadWriteMany]` on a storageClass that only does RWO, PVC stays
  Pending. Surface this as `status.phase=Pending` with a clear
  condition once we add Conditions plumbing (deferred — M2 stays
  with Phase only).

---

## 9. Files touched (sketch)

```
api/v1alpha1/devpod_types.go             (PersistenceSpec extensions)
internal/render/render.go                (HomeVolumeName const, helpers)
internal/render/pod.go                   (injection logic)
internal/render/pvc.go                   (NEW)
internal/render/pvc_test.go              (NEW)
internal/render/pod_test.go              (new cases)
internal/controllers/devpod_controller.go(running=false branch, status writes, PVC apply, finalizer detach)
internal/controllers/devpod_controller_test.go (new cases)
internal/webhook/devpod_webhook.go       (CustomValidator migration + new rules)
internal/webhook/devpod_webhook_test.go  (new cases)
cmd/controller/main.go                   (webhook registration + flags)
deploy/chart/templates/certificate.yaml  (NEW)
deploy/chart/templates/issuer.yaml       (NEW)
deploy/chart/templates/validatingwebhookconfiguration.yaml (NEW)
deploy/chart/templates/controller.yaml   (webhook port + cert volumeMount)
deploy/chart/values.yaml                 (webhook image / cert config)
hack/e2e-m2.sh                           (NEW)
config/crd/bases/devpod.io_devpods.yaml  (regenerated)
deploy/chart/templates/crds/devpod.io_devpods.yaml (regenerated, in sync)
```

---

## 10. Open questions answered during brainstorming

- Q: PVC mount path? **A:** `spec.persistence.mountPath` (required).
- Q: Reclaim on DevPod delete? **A:** Finalizer detaches PVC ownerRef;
  default retain; manual cleanup by user.
- Q: What stays when `running=false`? **A:** Only Pod is deleted.
  Service, host-key Secret, NetworkPolicy, PVC all stay.
- Q: AccessModes? **A:** `spec.persistence.accessModes`, default
  `[ReadWriteOnce]`.
- Q: Per the user, pod must be fully configurable — persistence is
  *injected into* the user's spec.pod, not a parallel API surface.
