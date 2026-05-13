# DevPod F1 — Hardening + Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. **Each Agent dispatch (implementer + reviewers) MUST pass `model=opus`.**

**Goal:** Materialize the F1 spec —
`docs/superpowers/specs/2026-05-12-devpod-f1-hardening.md`.
Five focused followups; ~7 task units.

**Architecture:** Sequential, in dependency order. T1/T2 are
no-ops on the rest of the codebase (cap drop + script). T3 (Size type
swap) touches every test file with a `"1Gi"` literal — best done as
one batch so the reviewers see the whole sweep. T4 (PodName collision)
and T5 (NetworkPolicy cleanup) are independent of each other and of
T1-T3.

**Tech Stack:** unchanged.

---

## File structure

| File | Action | Task |
|------|--------|------|
| `internal/render/pod.go` | modify | T1 |
| `internal/render/pod_test.go` | modify | T1 |
| `hack/sync-crd-chart.sh` | create | T2 |
| `api/v1alpha1/groupversion_info.go` (or wherever the `//go:generate` is) | modify | T2 |
| `api/v1alpha1/devpod_types.go` | modify | T3 |
| `api/v1alpha1/zz_generated.deepcopy.go` | regen | T3 |
| `config/crd/bases/devpod.io_devpods.yaml` | regen | T3 |
| `deploy/chart/templates/crds/devpod.io_devpods.yaml` | regen | T3 |
| `internal/render/pvc.go` | modify | T3 |
| `internal/render/pvc_test.go` | modify | T3 |
| `internal/render/pod_test.go` | modify | T3 (Size literals) |
| `internal/controllers/devpod_controller_test.go` | modify | T3 (Size literals) |
| `internal/webhook/devpod_webhook.go` | modify | T3 (drop Size-parse) + T4 (collision check) |
| `internal/webhook/devpod_webhook_test.go` | modify | T3 + T4 |
| `internal/controllers/devpod_controller.go` | modify | T5 |
| `internal/controllers/devpod_controller_test.go` | modify | T5 |

---

### Task 1: Drop sidecar SYS_PTRACE

**Files:**
- Modify: `internal/render/pod.go`
- Modify: `internal/render/pod_test.go` (if it pins capability set)

- [ ] **Step 1: Write/update the failing test**

Search `internal/render/pod_test.go` for a capability assertion. If
there's a test that asserts SYS_PTRACE is present, update its expected
set to `["SYS_ADMIN"]`. If no such assertion exists, add a small one:

```go
func TestRenderPod_SidecarCaps_OnlySysAdmin(t *testing.T) {
    pod, err := render.Pod(minimalDevPod(), cfg())
    if err != nil {
        t.Fatalf("Pod: %v", err)
    }
    sc := pod.Spec.Containers[1].SecurityContext
    if sc == nil || sc.Capabilities == nil {
        t.Fatal("sidecar SecurityContext/Capabilities missing")
    }
    got := sc.Capabilities.Add
    if len(got) != 1 || got[0] != corev1.Capability("SYS_ADMIN") {
        t.Errorf("sidecar caps = %v, want [SYS_ADMIN]", got)
    }
}
```

- [ ] **Step 2: Run, see fail**

```bash
bash hack/test.sh ./internal/render/...
```

Expected: test fails because current caps are `["SYS_PTRACE", "SYS_ADMIN"]`.

- [ ] **Step 3: Implement**

In `internal/render/pod.go`'s `sidecarContainer`, replace:

```go
Capabilities: &corev1.Capabilities{
    Add: []corev1.Capability{"SYS_PTRACE", "SYS_ADMIN"},
},
```

with:

```go
Capabilities: &corev1.Capabilities{
    Add: []corev1.Capability{"SYS_ADMIN"},
},
```

Update the surrounding comment (which currently explains the SYS_PTRACE
"M1 debugging" rationale) to reflect that SYS_ADMIN is the sole
capability needed for `setns(2)` via the nsenter wrapper.

- [ ] **Step 4: Run, see pass**

```bash
bash hack/test.sh ./internal/render/...
```

- [ ] **Step 5: Commit**

```bash
git add internal/render/pod.go internal/render/pod_test.go
git commit -m "$(cat <<'EOF'
internal/render: drop sidecar SYS_PTRACE

CAP_SYS_PTRACE was retained from M1 for cross-process inspection
during early debugging. With the nsenter wrapper landed (M1+), setns(2)
only needs CAP_SYS_ADMIN. Remove SYS_PTRACE from the sidecar's
capability set to shrink the attack surface available to a compromised
sidecar.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: CRD YAML sync script

**Files:**
- Create: `hack/sync-crd-chart.sh`
- Modify: `api/v1alpha1/groupversion_info.go` (or whichever file has the existing `//go:generate` directive)

- [ ] **Step 1: Write the script**

`hack/sync-crd-chart.sh`:

```bash
#!/usr/bin/env bash
# Mirror controller-gen-generated CRD YAMLs from config/crd/bases/
# into the Helm chart so the two locations cannot drift. Run by
# go:generate in api/v1alpha1.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
SRC="$ROOT/config/crd/bases"
DST="$ROOT/deploy/chart/templates/crds"

mkdir -p "$DST"
cp "$SRC"/devpod.io_*.yaml "$DST"/
```

`chmod +x hack/sync-crd-chart.sh`.

- [ ] **Step 2: Locate the existing `//go:generate` directive**

```bash
grep -rn "go:generate" api/
```

You should find a `controller-gen` line. Add a second directive
immediately after:

```go
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen ...
//go:generate bash ../../hack/sync-crd-chart.sh
```

The relative path `../../hack/...` works because `go generate` runs
from the directory of the file containing the directive.

- [ ] **Step 3: Verify by deleting and regenerating**

```bash
rm deploy/chart/templates/crds/devpod.io_devpods.yaml
go generate ./...
ls deploy/chart/templates/crds/devpod.io_devpods.yaml  # should exist again
diff config/crd/bases/devpod.io_devpods.yaml deploy/chart/templates/crds/devpod.io_devpods.yaml  # empty
```

- [ ] **Step 4: Build**

```bash
go build ./...
```

- [ ] **Step 5: Commit**

```bash
git add hack/sync-crd-chart.sh api/v1alpha1/groupversion_info.go
git commit -m "$(cat <<'EOF'
hack/sync-crd-chart: mirror CRDs into the chart on go generate

Previously every regen needed a manual `cp config/crd/bases/*.yaml
deploy/chart/templates/crds/`. Now a single `go generate ./...` runs
both controller-gen and the mirror, so the two locations cannot
drift.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: PersistenceSpec.Size → resource.Quantity

**Files:**
- Modify: `api/v1alpha1/devpod_types.go`
- Regen: `api/v1alpha1/zz_generated.deepcopy.go`
- Regen: `config/crd/bases/devpod.io_devpods.yaml`
- Regen: `deploy/chart/templates/crds/devpod.io_devpods.yaml` (via T2 script)
- Modify: `internal/render/pvc.go`
- Modify: `internal/render/pvc_test.go`
- Modify: `internal/render/pod_test.go` (Size literals in fixtures)
- Modify: `internal/controllers/devpod_controller_test.go` (Size literals)
- Modify: `internal/webhook/devpod_webhook.go` (drop ParseQuantity in validatePersistence)
- Modify: `internal/webhook/devpod_webhook_test.go` (Size literals; drop or update size-parse test)

- [ ] **Step 1: Change the API type**

In `api/v1alpha1/devpod_types.go`'s `PersistenceSpec`:

```go
import "k8s.io/apimachinery/pkg/api/resource"

type PersistenceSpec struct {
    // Size of the home PVC.
    Size resource.Quantity `json:"size"`
    // ... other fields unchanged
}
```

The kubebuilder marker stays implicit — controller-gen knows about
`resource.Quantity`.

- [ ] **Step 2: Regenerate**

```bash
go generate ./...
```

Both `zz_generated.deepcopy.go` and `config/crd/bases/...` update, and
the chart copy mirrors via T2's script.

- [ ] **Step 3: Update render layer**

In `internal/render/pvc.go`:

```go
func HomePVC(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) (*corev1.PersistentVolumeClaim, error) {
    if dp.Spec.Persistence == nil {
        return nil, nil
    }
    p := dp.Spec.Persistence

    // Size is now a typed resource.Quantity — no Parse needed.
    modes := p.AccessModes
    if len(modes) == 0 {
        modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
    } else {
        modes = append([]corev1.PersistentVolumeAccessMode(nil), p.AccessModes...)
    }

    pvc := &corev1.PersistentVolumeClaim{
        ObjectMeta: ObjectMeta(HomePVCName(dp), cfg.Spec.DevPodNamespace, dp),
        Spec: corev1.PersistentVolumeClaimSpec{
            AccessModes: modes,
            Resources: corev1.VolumeResourceRequirements{
                Requests: corev1.ResourceList{corev1.ResourceStorage: p.Size},
            },
        },
    }
    if p.StorageClassName != "" {
        pvc.Spec.StorageClassName = ptr.To(p.StorageClassName)
    }
    return pvc, nil
}
```

The signature still returns `error`, but the body no longer produces
one. Either keep the signature (for callers that handle err) or
simplify to `(*PersistentVolumeClaim)` — keep it as `(..., error)` since
it's used in tests/controllers that check err already.

- [ ] **Step 4: Update tests — flip every `Size: "..."` literal to `Size: resource.MustParse("...")`**

Files needing the sweep:
- `internal/render/pvc_test.go`
- `internal/render/pod_test.go`
- `internal/controllers/devpod_controller_test.go`
- `internal/webhook/devpod_webhook_test.go`

Also drop the `TestHomePVC_InvalidSizeIsError` case in
`pvc_test.go` — it's no longer reachable. Replace with a comment:

```go
// Note: invalid Size strings can no longer reach render.HomePVC because
// the API type is now resource.Quantity; the JSON unmarshal at the API
// server rejects them.
```

Drop `TestRejectsUnparseableSize` from `webhook_test.go` for the same
reason.

In `webhook/devpod_webhook.go`'s `validatePersistence`, remove:

```go
if _, err := resource.ParseQuantity(p.Size); err != nil {
    return fmt.Sprintf("spec.persistence.size %q is not a valid resource quantity: %v", p.Size, err)
}
```

The remaining checks (target container, mountPath collision) stay.

- [ ] **Step 5: Run all tests**

```bash
bash hack/test.sh ./...
```

Expected: all green.

- [ ] **Step 6: Smoke-deploy**

```bash
bash hack/e2e-up.sh 2>&1 | tail -5
bash hack/e2e-m2.sh
```

The M2 e2e exercises the new typed Size end-to-end.

- [ ] **Step 7: Commit**

```bash
git add api/v1alpha1/devpod_types.go \
  api/v1alpha1/zz_generated.deepcopy.go \
  config/crd/bases/devpod.io_devpods.yaml \
  deploy/chart/templates/crds/devpod.io_devpods.yaml \
  internal/render/pvc.go internal/render/pvc_test.go \
  internal/render/pod_test.go \
  internal/controllers/devpod_controller_test.go \
  internal/webhook/devpod_webhook.go internal/webhook/devpod_webhook_test.go
git commit -m "$(cat <<'EOF'
api/v1alpha1: PersistenceSpec.Size → resource.Quantity

The API server itself now validates the quantity (via OpenAPI schema)
on every CR create / update, independent of whether the validating
webhook is reachable. Render and webhook layers drop their ad-hoc
ParseQuantity calls; tests flip "1Gi" literals to
resource.MustParse("1Gi").

Wire format is unchanged ("1Gi" still parses), so existing DevPod CRs
on disk roundtrip cleanly.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: PodName collision webhook rule

**Files:**
- Modify: `internal/webhook/devpod_webhook.go`
- Modify: `internal/webhook/devpod_webhook_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/webhook/devpod_webhook_test.go`:

```go
func TestValidateCreate_RejectsPodNameCollision(t *testing.T) {
    existing := &devpodv1alpha1.DevPod{
        ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "devpods", UID: "u-existing"},
        Spec: devpodv1alpha1.DevPodSpec{
            Owner:   "alice-frontend",
            Running: true,
            Pod: &devpodv1alpha1.PodWorkloadSpec{
                Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
            },
        },
    }
    scheme := testScheme()
    c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
    v := &webhook.DevPodValidator{Client: c}

    incoming := validDP()
    incoming.Namespace = "devpods"
    incoming.Name = "frontend-dev"
    incoming.Spec.Owner = "alice"
    _, err := v.ValidateCreate(context.Background(), incoming)
    if err == nil {
        t.Fatal("expected rejection of name-collision create")
    }
    if !strings.Contains(err.Error(), "alice-frontend-dev") {
        t.Errorf("error should mention the collided rendered name; got %v", err)
    }
}

func TestValidateUpdate_AllowsSameDevPod(t *testing.T) {
    existing := &devpodv1alpha1.DevPod{
        ObjectMeta: metav1.ObjectMeta{Name: "thing", Namespace: "devpods", UID: "u-x"},
        Spec: devpodv1alpha1.DevPodSpec{
            Owner:   "alice",
            Running: true,
            Pod: &devpodv1alpha1.PodWorkloadSpec{
                Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
            },
        },
    }
    c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build()
    v := &webhook.DevPodValidator{Client: c}

    // Same UID = same DevPod (label edit, say) → no collision with itself.
    updated := existing.DeepCopy()
    updated.Labels = map[string]string{"changed": "yes"}
    _, err := v.ValidateUpdate(context.Background(), existing, updated)
    if err != nil {
        t.Errorf("unexpected error on same-DevPod update: %v", err)
    }
}

func TestValidateCreate_AllowsNonColliding(t *testing.T) {
    existing := &devpodv1alpha1.DevPod{
        ObjectMeta: metav1.ObjectMeta{Name: "thing", Namespace: "devpods", UID: "u-x"},
        Spec: devpodv1alpha1.DevPodSpec{
            Owner:   "alice",
            Running: true,
            Pod: &devpodv1alpha1.PodWorkloadSpec{
                Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
            },
        },
    }
    c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build()
    v := &webhook.DevPodValidator{Client: c}

    incoming := validDP()
    incoming.Namespace = "devpods"
    incoming.Name = "different"
    incoming.Spec.Owner = "alice"
    _, err := v.ValidateCreate(context.Background(), incoming)
    if err != nil {
        t.Errorf("non-colliding create rejected: %v", err)
    }
}
```

Import additions in the test file: `strings`, `sigs.k8s.io/controller-runtime/pkg/client/fake`. `testScheme` already exists from M3.T9. `validDP` already exists from M3.T9. `corev1` / `metav1` already imported.

- [ ] **Step 2: Run, see fail**

```bash
bash hack/test.sh ./internal/webhook/...
```

The three new tests fail (no collision check yet).

- [ ] **Step 3: Implement**

In `internal/webhook/devpod_webhook.go`, add a helper:

```go
import (
    "context"
    // ...
    "sigs.k8s.io/controller-runtime/pkg/client"
    "github.com/mrhaoxx/devpod/internal/render"
)

// validateNoPodNameCollision rejects creates/updates whose derived
// rendered name (render.PodName) collides with any OTHER DevPod in
// the same namespace. The render layer maps (owner, name) → a single
// string, so two distinct tuples can collide when either side has
// dashes. Without this rule, the second create surfaces a
// cross-tenant AlreadyExists from kube-apiserver — opaque to the
// user and a small existence leak.
//
// Guarded on v.Client != nil so unit tests that don't pass a client
// continue to work.
func (v *DevPodValidator) validateNoPodNameCollision(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
    if v.Client == nil {
        return nil
    }
    var existing devpodv1alpha1.DevPodList
    if err := v.Client.List(ctx, &existing, client.InNamespace(dp.Namespace)); err != nil {
        return fmt.Errorf("list devpods: %w", err)
    }
    target := render.PodName(dp)
    for i := range existing.Items {
        other := &existing.Items[i]
        if other.UID == dp.UID {
            continue
        }
        if render.PodName(other) == target {
            return fmt.Errorf("derived name %q collides with existing DevPod %q (owner=%q); rename one of them",
                target, other.Name, other.Spec.Owner)
        }
    }
    return nil
}
```

Wire it into `validate`:

```go
func (v *DevPodValidator) validate(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
    if msg := validateXor(dp); msg != "" {
        return errors.New(msg)
    }
    if msg := validatePodConstraints(dp); msg != "" {
        return errors.New(msg)
    }
    if msg := validateReservedNames(dp); msg != "" {
        return errors.New(msg)
    }
    if msg := validatePersistence(dp); msg != "" {
        return errors.New(msg)
    }
    return v.validateNoPodNameCollision(ctx, dp)
}
```

`validate` now takes a `context.Context`. Plumb it through
`ValidateCreate` and `ValidateUpdate` (both already have a `ctx`
parameter — just pass it down).

- [ ] **Step 4: Run, see pass**

```bash
bash hack/test.sh ./internal/webhook/...
```

- [ ] **Step 5: Smoke-deploy**

```bash
bash hack/e2e-up.sh 2>&1 | tail -3

# Apply a DevPod, then try to apply one that would collide.
cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: dev
  namespace: devpods
spec:
  owner: alice-frontend
  running: true
  pod: {spec: {containers: [{name: dev, image: debian:stable, command: ["sleep","infinity"]}]}}
EOF

cat <<EOF | kubectl apply -f - 2>&1
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: frontend-dev
  namespace: devpods
spec:
  owner: alice
  running: true
  pod: {spec: {containers: [{name: dev, image: debian:stable, command: ["sleep","infinity"]}]}}
EOF
```

Second apply must be rejected by the webhook with the collision message.

Clean up:
```bash
kubectl -n devpods delete devpod dev --ignore-not-found
```

- [ ] **Step 6: Commit**

```bash
git add internal/webhook/devpod_webhook.go internal/webhook/devpod_webhook_test.go
git commit -m "$(cat <<'EOF'
internal/webhook: reject PodName collisions across (owner, name) tuples

render.PodName(dp) = "<owner>-<name>" is ambiguous when either half
contains a dash: (alice-frontend, dev) collides with (alice,
frontend-dev). Without this check, the second create returns an
opaque AlreadyExists from the apiserver against an object the caller
may have no read permission on — an existence leak / name squatting
vector.

The webhook now Lists DevPods in the target namespace and rejects
creates / updates whose render.PodName collides with any OTHER
DevPod. Guarded on v.Client != nil so unit tests that don't pass a
client stay green.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: NetworkPolicy lifecycle — clean owner-allow on last DevPod

**Files:**
- Modify: `internal/controllers/devpod_controller.go`
- Modify: `internal/controllers/devpod_controller_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestReconcile_OwnerAllowNetPolCleanedOnLastDevPod(t *testing.T) {
    setupSuite(t)
    env := newTestEnv(t)

    user := &devpodv1alpha1.User{
        ObjectMeta: metav1.ObjectMeta{Name: "jules"},
        Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA test"}},
    }
    if err := env.Client.Create(env.Ctx, user); err != nil {
        t.Fatalf("create user: %v", err)
    }
    dp1 := &devpodv1alpha1.DevPod{
        ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "devpods"},
        Spec: devpodv1alpha1.DevPodSpec{
            Owner: "jules", Running: true,
            Pod: &devpodv1alpha1.PodWorkloadSpec{
                Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
            },
        },
    }
    dp2 := dp1.DeepCopy()
    dp2.Name = "two"
    if err := env.Client.Create(env.Ctx, dp1); err != nil {
        t.Fatal(err)
    }
    if err := env.Client.Create(env.Ctx, dp2); err != nil {
        t.Fatal(err)
    }

    policyKey := types.NamespacedName{Name: "devpod-allow-jules", Namespace: "devpods"}

    // Wait for the policy to exist.
    deadline := time.Now().Add(10 * time.Second)
    for time.Now().Before(deadline) {
        var p networkingv1.NetworkPolicy
        if env.Client.Get(env.Ctx, policyKey, &p) == nil {
            break
        }
        time.Sleep(200 * time.Millisecond)
    }
    var p networkingv1.NetworkPolicy
    if err := env.Client.Get(env.Ctx, policyKey, &p); err != nil {
        t.Fatalf("policy never created: %v", err)
    }

    // Delete dp1; policy must remain.
    if err := env.Client.Delete(env.Ctx, dp1); err != nil {
        t.Fatal(err)
    }
    deadline = time.Now().Add(10 * time.Second)
    for time.Now().Before(deadline) {
        var got devpodv1alpha1.DevPod
        if apierrors.IsNotFound(env.Client.Get(env.Ctx, types.NamespacedName{Name: "one", Namespace: "devpods"}, &got)) {
            break
        }
        time.Sleep(200 * time.Millisecond)
    }
    if err := env.Client.Get(env.Ctx, policyKey, &p); err != nil {
        t.Fatalf("policy gone after deleting only one of two DevPods: %v", err)
    }

    // Delete dp2; policy must vanish.
    if err := env.Client.Delete(env.Ctx, dp2); err != nil {
        t.Fatal(err)
    }
    deadline = time.Now().Add(15 * time.Second)
    for time.Now().Before(deadline) {
        var got networkingv1.NetworkPolicy
        if apierrors.IsNotFound(env.Client.Get(env.Ctx, policyKey, &got)) {
            return // pass
        }
        time.Sleep(200 * time.Millisecond)
    }
    t.Fatalf("owner-allow policy never deleted after the last DevPod went away")
}
```

- [ ] **Step 2: Run, see fail**

```bash
bash hack/test.sh ./internal/controllers/...
```

- [ ] **Step 3: Implement in the finalizer**

In `internal/controllers/devpod_controller.go`'s `Reconcile`, replace
the deletion-handling block:

```go
if !dp.DeletionTimestamp.IsZero() {
    if err := r.detachPVC(ctx, &dp); err != nil {
        return ctrl.Result{}, err
    }
    if err := r.cleanupOwnerNetPolIfLast(ctx, &dp); err != nil {
        return ctrl.Result{}, err
    }
    controllerutil.RemoveFinalizer(&dp, devpodFinalizer)
    if err := r.Update(ctx, &dp); err != nil {
        return ctrl.Result{}, fmt.Errorf("clear finalizer: %w", err)
    }
    return ctrl.Result{}, nil
}
```

Add the new helper after `detachPVC`:

```go
// cleanupOwnerNetPolIfLast deletes the per-owner allow NetworkPolicy
// when the DevPod being deleted is the owner's last one. Idempotent;
// a concurrent finalizer running for a sibling will see this DevPod's
// DeletionTimestamp (so skip it) and the second Delete returns
// NotFound which we ignore.
func (r *DevPodReconciler) cleanupOwnerNetPolIfLast(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
    var siblings devpodv1alpha1.DevPodList
    if err := r.List(ctx, &siblings, client.InNamespace(r.GwConfig.Spec.DevPodNamespace)); err != nil {
        return fmt.Errorf("list devpods: %w", err)
    }
    for i := range siblings.Items {
        s := &siblings.Items[i]
        if s.UID == dp.UID {
            continue
        }
        if s.Spec.Owner != dp.Spec.Owner {
            continue
        }
        if s.DeletionTimestamp.IsZero() {
            // A live sibling exists — keep the policy.
            return nil
        }
    }
    // No live sibling for this owner — remove the policy.
    policy := &networkingv1.NetworkPolicy{
        ObjectMeta: metav1.ObjectMeta{
            Name:      render.OwnerNetPolName(dp.Spec.Owner),
            Namespace: r.GwConfig.Spec.DevPodNamespace,
        },
    }
    if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
        return fmt.Errorf("delete owner allow netpol: %w", err)
    }
    return nil
}
```

- [ ] **Step 4: Run, see pass**

```bash
bash hack/test.sh ./internal/controllers/...
```

- [ ] **Step 5: Commit**

```bash
git add internal/controllers/devpod_controller.go internal/controllers/devpod_controller_test.go
git commit -m "$(cat <<'EOF'
controllers/devpod: clean owner-allow NetworkPolicy on last DevPod

devpod-allow-<owner> was created lazily on first DevPod and never
removed, leaving an orphan policy for every owner that ever had a
DevPod. The DevPod finalizer now lists same-namespace siblings and
deletes the policy when no live sibling shares the owner.

Idempotent under concurrent deletion: the DeletionTimestamp != nil
filter on sibling lookup plus apierrors.IsNotFound tolerance means
both finalizers can race to the Delete and exactly one succeeds.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Done criteria

When all five tasks pass tests and the M2 e2e script (`hack/e2e-m2.sh`)
remains green, F1 is complete.

---

## Self-review notes

Spec coverage check:
- §2 NetworkPolicy lifecycle → T5
- §3 PodName collision → T4
- §4 SYS_PTRACE drop → T1
- §5 CRD YAML sync script → T2
- §6 Size type swap → T3

No placeholders. Types and helper names match what M0-M3 already
defined (`render.OwnerNetPolName`, `render.PodName`, sentinel error
list, `DevPodValidator.Client`). The `validate` ctx plumbing in T4
threads through methods that already had a ctx parameter.
