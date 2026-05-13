# DevPod F1 — Hardening + Lifecycle (M1/M2 follow-ups)

**Status:** Approved
**Date:** 2026-05-12
**Depends on:** M0+M1+M2+M3 implementations already merged.

---

## 1. Goal

Address five high-value items from `FOLLOWUPS.md`. None of these are
new features; each closes a real defect the controller / webhook /
chart left behind during the fast-march milestones.

The five items:

1. **NetworkPolicy lifecycle** — `devpod-allow-<owner>` is created
   lazily on first DevPod and never deleted, even when the owner
   has zero DevPods left. Resource leak; cluster-pollution over time.
2. **PodName collision webhook** — `(owner=alice, name=frontend-dev)`
   and `(owner=alice-frontend, name=dev)` both render to the same
   Pod/Service/PVC names. Today the second create fails opaquely with
   `AlreadyExists` against an object the user doesn't own → name
   squatting / existence-leak.
3. **Sidecar SYS_PTRACE drop** — M1 retained `CAP_SYS_PTRACE` for
   debugging. nsenter (M1+) needs only `CAP_SYS_ADMIN`. Drop it.
4. **CRD YAML sync** — `config/crd/bases/devpod.io_*.yaml` and
   `deploy/chart/templates/crds/devpod.io_*.yaml` are duplicated
   files; every regen so far has needed a manual `cp`. A
   `go generate`-driven sync script makes drift impossible.
5. **`PersistenceSpec.Size` type swap** — currently `string` with a
   webhook validator. M2's webhook catches malformed values at
   admission today, but only when the webhook is reachable. Switching
   the API type to `resource.Quantity` makes the API server itself
   reject bad input via OpenAPI validation — even if the webhook is
   down or skipped.

Non-goals (deferred to a future F2 or beyond):
- SSA-based drift reconciliation.
- User CR field indexer for `spec.owner`.
- TrustedProxyKey pubkey format regex.

---

## 2. NetworkPolicy lifecycle

### 2.1 Current state

`OwnerAllowNetworkPolicy(namespace, owner, gwNS)` is rendered into
`devpod-allow-<owner>` and applied on every DevPod reconcile via
`applyUnowned`. It has no ownerRef to any DevPod (the policy is shared
across all DevPods owned by `<owner>`).

When the last DevPod for an owner is deleted, nothing cleans up the
policy.

### 2.2 Fix

In `DevPodReconciler.Reconcile`'s deletion branch (already runs inside
the finalizer for `devpod.io/devpod-cleanup`), after `detachPVC`
succeeds, count the remaining same-owner DevPods. If this is the last
one, delete `devpod-allow-<owner>`.

Sketch:

```go
if !dp.DeletionTimestamp.IsZero() {
    if err := r.detachPVC(ctx, &dp); err != nil { ... }

    // Count same-owner DevPods that are NOT this one being deleted.
    var sib devpodv1alpha1.DevPodList
    if err := r.List(ctx, &sib, client.InNamespace(r.GwConfig.Spec.DevPodNamespace)); err != nil {
        return ctrl.Result{}, fmt.Errorf("list devpods: %w", err)
    }
    last := true
    for i := range sib.Items {
        s := &sib.Items[i]
        if s.UID == dp.UID { continue }
        if s.Spec.Owner == dp.Spec.Owner && s.DeletionTimestamp.IsZero() {
            last = false
            break
        }
    }
    if last {
        policy := &networkingv1.NetworkPolicy{
            ObjectMeta: metav1.ObjectMeta{
                Name:      render.OwnerNetPolName(dp.Spec.Owner),
                Namespace: r.GwConfig.Spec.DevPodNamespace,
            },
        }
        if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
            return ctrl.Result{}, fmt.Errorf("delete owner allow netpol: %w", err)
        }
    }

    controllerutil.RemoveFinalizer(&dp, devpodFinalizer)
    if err := r.Update(ctx, &dp); err != nil { ... }
    return ctrl.Result{}, nil
}
```

### 2.3 Race window

Two DevPods of the same owner deleted concurrently: each finalizer
goroutine sees one remaining sibling at the start, both decide it's
not "last". Then both finalizers remove themselves and the policy is
left orphan.

Mitigation: List inside the finalizer *after* the DeletionTimestamp on
the current DevPod is observed. The k8s API guarantees List reflects
all current DeletionTimestamps. The remaining sibling — also being
deleted concurrently — already has `DeletionTimestamp != nil`, and we
skip those (`s.DeletionTimestamp.IsZero()` filter). So both finalizers
correctly see "no live siblings" and both attempt the delete; the
second one gets `NotFound` which we ignore. Cleanup is idempotent.

### 2.4 Test

`internal/controllers/devpod_controller_test.go`:

- Create User + two DevPods owned by alice. Wait for both Pods to
  appear + the `devpod-allow-alice` policy.
- Delete one DevPod. Verify the policy STILL exists.
- Delete the other. Verify the policy is gone.

---

## 3. PodName collision webhook rule

### 3.1 Current state

`render.PodName(dp)` = `<owner>-<name>`. The same string can come from
two different `(owner, name)` tuples when either side contains `-`.

Today the second `kubectl apply` fails with an opaque AlreadyExists.
Worse, the error reveals the conflicting name even if the caller has
no read permission on the existing object — a small information leak.

### 3.2 Fix

`internal/webhook/devpod_webhook.go`'s `validate(dp)` runs a List on
DevPods in the same namespace and rejects if any *other* DevPod has
the same `render.PodName(dp)`.

```go
// In validate(), after the existing structural checks:
if v.Client != nil {
    var existing devpodv1alpha1.DevPodList
    if err := v.Client.List(ctx, &existing, client.InNamespace(dp.Namespace)); err != nil {
        return fmt.Errorf("list devpods: %w", err)
    }
    target := render.PodName(dp)
    for i := range existing.Items {
        other := &existing.Items[i]
        if other.UID == dp.UID { continue } // updates
        if render.PodName(other) == target {
            return fmt.Errorf("derived name %q collides with existing DevPod %q (owner=%q)",
                target, other.Name, other.Spec.Owner)
        }
    }
}
```

The `v.Client != nil` guard keeps existing unit tests (which don't pass
a client) green; only the wired-up production webhook hits the List.

Validator now takes `ctx` — bubble it through from
`ValidateCreate`/`ValidateUpdate` (they already accept it).

### 3.3 Updates

`ValidateUpdate` runs the same check. Excluding `dp.UID == other.UID`
on the loop handles the normal "update is fine" case; a webhook
manifest with `matchPolicy: Equivalent` already excludes DELETE.

### 3.4 Test

`internal/webhook/devpod_webhook_test.go` cases:

- `TestRejectsPodNameCollision`: pre-create `(alice-frontend, dev)` →
  validate `(alice, frontend-dev)` create → expect rejection with the
  collision message.
- `TestAllowsSameDevPodUpdate`: existing DevPod's update doesn't
  collide with itself.
- `TestAllowsNonCollidingCreates`: two DevPods with distinct rendered
  names admit fine.

These need a `client.Client` in the validator; pass a
`fake.NewClientBuilder` with the pre-existing DevPods loaded.

---

## 4. Sidecar SYS_PTRACE drop

### 4.1 Fix

In `internal/render/pod.go`'s `sidecarContainer`:

```go
SecurityContext: &corev1.SecurityContext{
    Capabilities: &corev1.Capabilities{
        Add: []corev1.Capability{"SYS_ADMIN"},  // SYS_PTRACE removed
    },
},
```

Comment update: remove the "retained for cross-process inspection
during M1 debugging" note since the rationale no longer applies after
the nsenter wrapper landed.

### 4.2 Test

`internal/render/pod_test.go` already snapshots the sidecar
container's caps (or should — verify the existing test asserts the
set). Update assertion to expect only `SYS_ADMIN`.

---

## 5. CRD YAML sync script

### 5.1 Fix

Add `hack/sync-crd-chart.sh`:

```bash
#!/usr/bin/env bash
# Copy controller-gen-generated CRD YAMLs into the Helm chart so the two
# locations don't drift. Run by go:generate in api/v1alpha1.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
cp config/crd/bases/devpod.io_*.yaml deploy/chart/templates/crds/
```

Wire it from `api/v1alpha1/groupversion_info.go` (or wherever the
existing `//go:generate controller-gen ...` directive lives):

```go
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen ...
//go:generate bash ../../hack/sync-crd-chart.sh
```

Now `go generate ./...` will both regenerate and mirror, single command.

### 5.2 Test

Manual: delete one of the chart CRD files, run `go generate ./...`,
confirm it comes back identical.

---

## 6. `PersistenceSpec.Size` → `resource.Quantity`

### 6.1 Current state

```go
type PersistenceSpec struct {
    Size string `json:"size"`
    ...
}
```

`render.HomePVC` calls `resource.ParseQuantity(p.Size)` and surfaces
the error; M2's webhook does the same at admission.

### 6.2 Fix

Change the type:

```go
import "k8s.io/apimachinery/pkg/api/resource"

type PersistenceSpec struct {
    Size resource.Quantity `json:"size"`
    ...
}
```

`resource.Quantity` is the same type used in `PVCSpec.Resources.Requests`
itself. controller-gen knows how to emit OpenAPI for it (an IntOrString
that's a Kubernetes quantity); kubectl validates against the schema
without needing the webhook online.

`internal/render/pvc.go`: `p.Size` is now a `resource.Quantity` directly;
drop the `resource.ParseQuantity` call.

`internal/webhook/devpod_webhook.go`'s `validatePersistence`: drop the
Size-parse check (the API server now enforces this).

Tests using `Size: "1Gi"` literal strings flip to
`Size: resource.MustParse("1Gi")`. Includes:
- `internal/render/pvc_test.go`
- `internal/render/pod_test.go`
- `internal/controllers/devpod_controller_test.go`
- `internal/webhook/devpod_webhook_test.go`

### 6.3 Backward compatibility

The wire format is unchanged: `spec.persistence.size: "1Gi"` still
parses. Existing DevPod CRs on disk roundtrip cleanly. No migration.

---

## 7. Files touched (sketch)

```
internal/controllers/devpod_controller.go     (NetworkPolicy cleanup)
internal/controllers/devpod_controller_test.go (test cases)
internal/webhook/devpod_webhook.go            (PodName collision + drop Size-parse)
internal/webhook/devpod_webhook_test.go       (test cases)
internal/render/pod.go                        (drop SYS_PTRACE)
internal/render/pod_test.go                   (caps assertion)
internal/render/pvc.go                        (typed Size)
internal/render/pvc_test.go                   (resource.MustParse)
api/v1alpha1/devpod_types.go                  (Size resource.Quantity)
api/v1alpha1/zz_generated.deepcopy.go         (regen)
config/crd/bases/devpod.io_devpods.yaml       (regen)
deploy/chart/templates/crds/devpod.io_devpods.yaml (regen via script)
hack/sync-crd-chart.sh                        NEW
```

---

## 8. Test plan

- Existing unit + envtest + webhook suites keep passing.
- New cases:
  - `internal/controllers`: NetworkPolicy cleanup roundtrip
    (two DevPods → delete one → policy stays → delete other → policy gone).
  - `internal/webhook`: PodName collision rejection;
    same-DevPod update allowed; non-collision allowed.
- e2e (`hack/e2e-up.sh` baseline): verify chart still renders after the
  CRD sync script change.

---

## 9. Open questions resolved

- Concurrent deletion of two same-owner DevPods → safe via
  `DeletionTimestamp != nil` filter + idempotent `NotFound` on Delete.
- Tests that don't pass a client to the validator → guarded by
  `v.Client != nil` in the new collision check.
- Resource.Quantity wire compatibility → JSON-string form unchanged.
