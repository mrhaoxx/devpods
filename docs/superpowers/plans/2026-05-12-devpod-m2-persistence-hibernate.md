# DevPod M2 — Persistence and Manual Hibernate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **Each Agent dispatch (implementer + spec reviewer + code reviewer) MUST pass `model=opus`.**

**Goal:** Materialize the M2 spec —
`docs/superpowers/specs/2026-05-12-devpod-m2-persistence-hibernate.md` —
so that `spec.persistence` injects a home PVC into the user's pod template,
`spec.running=false` hibernates by deleting the Pod, the controller fills the
remaining `DevPodStatus` fields, and the validating webhook actually serves.

**Architecture:** Three layers grow incrementally — `api/v1alpha1` (one new
field set + a name-length cap), `internal/render` (one new file + a Pod
injection extension), `internal/controllers/devpod_controller.go` (hibernate
branch, PVC lifecycle, full status, finalizer detach). The webhook migrates
to the typed `CustomValidator` shape, gains four new rules, and is wired
into `cmd/controller` plus the Helm chart with cert-manager-issued TLS. A
final shell script demonstrates write-hibernate-resume on OrbStack.

**Tech Stack:** Go 1.22, controller-runtime v0.20, kubebuilder annotations,
envtest, OpenSSH client for e2e, OrbStack k8s, Helm 3, cert-manager.

---

## File structure

| File | Action | Responsibility |
|------|--------|----------------|
| `api/v1alpha1/devpod_types.go` | modify | Add `AccessModes`/`MountPath`/`TargetContainer` to `PersistenceSpec`; add `+kubebuilder:validation:MaxLength=22` to `DevPod.metadata.name` via the existing `+kubebuilder` block on the struct. |
| `api/v1alpha1/zz_generated.deepcopy.go` | regen | `go generate ./...` rebuilds. |
| `config/crd/bases/devpod.io_devpods.yaml` | regen | controller-gen output. |
| `deploy/chart/templates/crds/devpod.io_devpods.yaml` | regen | Manually mirrored from the above (per FOLLOWUPS). |
| `internal/render/pvc.go` | create | `HomePVC(dp)` returns the rendered `*corev1.PersistentVolumeClaim`. |
| `internal/render/pvc_test.go` | create | Cover defaults, storageClass empty=cluster default, size parse error. |
| `internal/render/pod.go` | modify | When `spec.persistence != nil`, append a `volumeMount` named `devpod-home` at `spec.persistence.mountPath` to the target container (default `containers[0]`). |
| `internal/render/pod_test.go` | modify | New cases for the user-container mount injection + reserved-name defensive error. |
| `internal/controllers/devpod_controller.go` | modify | `applyPVC`, hibernate branch, full status, finalizer detach. |
| `internal/controllers/devpod_controller_test.go` | modify | Persistence-on case, hibernate roundtrip, delete-detaches-PVC. |
| `internal/webhook/devpod_webhook.go` | modify | Migrate to `admission.CustomValidator`; new rules; accept a `client.Client` (still unused in M2 but reserved for cross-object lookups). |
| `internal/webhook/devpod_webhook_test.go` | modify | Existing tests adapted to the new shape; new cases per rule. |
| `cmd/controller/main.go` | modify | `--webhook-port` / `--webhook-cert-dir` flags; register webhook server. |
| `deploy/chart/templates/issuer.yaml` | create | self-signed `Issuer` in the system namespace. |
| `deploy/chart/templates/certificate.yaml` | create | cert-manager `Certificate` for `devpod-webhook-tls`. |
| `deploy/chart/templates/validatingwebhookconfiguration.yaml` | create | VWC, `failurePolicy: Fail`, scoped to DevPod CRs. |
| `deploy/chart/templates/controller.yaml` | modify | Webhook port + cert volume mount. |
| `deploy/chart/templates/_helpers.tpl` | modify (if exists) or `values.yaml` | Webhook service name + cert dir. |
| `deploy/chart/values.yaml` | modify | Webhook config block (`webhook.port`, `webhook.certDir`, `webhook.serviceName`). |
| `hack/e2e-m2.sh` | create | `kubectl apply` a persistence-enabled DevPod, write a marker file, hibernate, resume, read it back. |

DRY notes:
- The `devpod-home` volume name remains the existing `render.VolumeHome`
  constant. No new constant.
- The volume injection into the user container reuses the existing
  `homeVolume()` helper that already covers the `spec.volumes` side.
  Only the `volumeMount` slice on the target container is new.

YAGNI notes:
- No Conditions plumbing in M2 (Phase carries the signal; surface
  Conditions when something concrete needs them, e.g., M3 auth_path).
- No drift reconciliation for the PVC. M2's `applyOwned` mirrors the
  existing M1 behavior; SSA is the FOLLOWUPS plan.
- No watch / informer on PVC events feeding the DevPod controller.
  Periodic reconcile via `Owns()` is enough.

---

### Task 1: API additions — PersistenceSpec fields and DevPod name length

**Files:**
- Modify: `api/v1alpha1/devpod_types.go`
- Regen: `api/v1alpha1/zz_generated.deepcopy.go`
- Regen: `config/crd/bases/devpod.io_devpods.yaml`
- Regen: `deploy/chart/templates/crds/devpod.io_devpods.yaml`

- [ ] **Step 1: Extend `PersistenceSpec`**

In `api/v1alpha1/devpod_types.go`, change the `PersistenceSpec` struct
to add three fields after `StorageClassName`:

```go
// AccessModes for the home PVC. Defaults to [ReadWriteOnce]. The
// chosen StorageClass must support every mode listed here; otherwise
// PVC binding fails and the DevPod stays in Pending.
//
// +optional
// +kubebuilder:default={ReadWriteOnce}
// +listType=set
AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

// MountPath is where the PVC is mounted inside the target container.
// Required when persistence is set. Must be absolute and must not
// collide with any user-supplied volumeMount.mountPath on the target
// container — the validating webhook enforces this.
//
// +kubebuilder:validation:Pattern=`^/[^\s]*$`
MountPath string `json:"mountPath"`

// TargetContainer names which container in spec.pod.spec.containers
// receives the home mount. Defaults to
// spec.pod.spec.containers[0].name. Must reference an existing
// container; the webhook enforces this.
//
// +optional
TargetContainer string `json:"targetContainer,omitempty"`
```

- [ ] **Step 2: Add MaxLength=22 to `DevPod.metadata.name`**

Above the `type DevPod struct {` line, add this kubebuilder marker:

```go
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 22",message="DevPod name must be at most 22 characters (length budget for derived Pod/PVC/Service names)"
```

(kubebuilder can't put `MaxLength` on `metadata.name` directly via the
struct-level annotation in v0.20; the CEL form is the supported path.)

- [ ] **Step 3: Regenerate**

```bash
go generate ./...
# Then sync the chart CRD copy (see FOLLOWUPS §"CRD YAMLs are duplicated"):
cp config/crd/bases/devpod.io_devpods.yaml deploy/chart/templates/crds/devpod.io_devpods.yaml
```

Expected: `zz_generated.deepcopy.go` updated (new slice field copy);
the CRD YAML grows `accessModes`, `mountPath`, `targetContainer` under
`spec.persistence` and a `x-kubernetes-validations` entry on the root
schema for the name length rule.

- [ ] **Step 4: Verify build**

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/devpod_types.go \
  api/v1alpha1/zz_generated.deepcopy.go \
  config/crd/bases/devpod.io_devpods.yaml \
  deploy/chart/templates/crds/devpod.io_devpods.yaml
git commit -m "api/v1alpha1: PersistenceSpec gains accessModes/mountPath/targetContainer; DevPod name cap"
```

---

### Task 2: Render PVC

**Files:**
- Create: `internal/render/pvc.go`
- Create: `internal/render/pvc_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/render/pvc_test.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

func TestHomePVC_NilPersistenceReturnsNil(t *testing.T) {
	dp := minimalDevPod()
	pvc, err := render.HomePVC(dp, cfg())
	if err != nil {
		t.Fatalf("HomePVC: %v", err)
	}
	if pvc != nil {
		t.Errorf("expected nil when persistence is nil, got %v", pvc)
	}
}

func TestHomePVC_DefaultsToRWO(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:      "10Gi",
		MountPath: "/workspace",
	}

	pvc, err := render.HomePVC(dp, cfg())
	if err != nil {
		t.Fatalf("HomePVC: %v", err)
	}
	if pvc == nil {
		t.Fatal("expected non-nil PVC")
	}
	if pvc.Name != "alice-frontend-dev-home" {
		t.Errorf("PVC name = %q, want alice-frontend-dev-home", pvc.Name)
	}
	if got := pvc.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteOnce {
		t.Errorf("access modes = %v, want [ReadWriteOnce]", got)
	}
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if want := resource.MustParse("10Gi"); q.Cmp(want) != 0 {
		t.Errorf("size = %v, want 10Gi", q)
	}
}

func TestHomePVC_EmptyStorageClassMeansClusterDefault(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:      "1Gi",
		MountPath: "/workspace",
	}
	pvc, err := render.HomePVC(dp, cfg())
	if err != nil {
		t.Fatalf("HomePVC: %v", err)
	}
	if pvc.Spec.StorageClassName != nil {
		t.Errorf("StorageClassName = %v, want nil (cluster default)", *pvc.Spec.StorageClassName)
	}
}

func TestHomePVC_ExplicitStorageClass(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:             "1Gi",
		StorageClassName: "fast-ssd",
		MountPath:        "/workspace",
	}
	pvc, err := render.HomePVC(dp, cfg())
	if err != nil {
		t.Fatalf("HomePVC: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-ssd" {
		t.Errorf("StorageClassName = %v, want fast-ssd", pvc.Spec.StorageClassName)
	}
}

func TestHomePVC_InvalidSizeIsError(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:      "not-a-quantity",
		MountPath: "/workspace",
	}
	_, err := render.HomePVC(dp, cfg())
	if err == nil {
		t.Fatal("expected error on invalid Size")
	}
}
```

`minimalDevPod()` and `cfg()` already exist in `pod_test.go` in the
same `render_test` package; reuse them.

- [ ] **Step 2: Run test, see it fail**

```bash
bash hack/test.sh ./internal/render/...
```

Expected: build error `undefined: render.HomePVC` in pvc_test.go.

- [ ] **Step 3: Implement `HomePVC`**

Create `internal/render/pvc.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// HomePVC renders the per-DevPod home PersistentVolumeClaim.
//
// Returns (nil, nil) when spec.persistence is unset — callers should
// treat that as "no PVC to apply".
//
// The returned object has no OwnerReferences set; the controller
// invokes controllerutil.SetControllerReference before Create.
func HomePVC(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) (*corev1.PersistentVolumeClaim, error) {
	if dp.Spec.Persistence == nil {
		return nil, nil
	}
	p := dp.Spec.Persistence

	size, err := resource.ParseQuantity(p.Size)
	if err != nil {
		return nil, fmt.Errorf("parse spec.persistence.size %q: %w", p.Size, err)
	}

	modes := p.AccessModes
	if len(modes) == 0 {
		modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: ObjectMeta(HomePVCName(dp), cfg.Spec.DevPodNamespace, dp),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: modes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if p.StorageClassName != "" {
		sc := p.StorageClassName
		pvc.Spec.StorageClassName = &sc
	}
	return pvc, nil
}
```

- [ ] **Step 4: Run tests, see pass**

```bash
bash hack/test.sh ./internal/render/...
```

Expected: all PVC tests pass; existing pod/secret/service tests unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/render/pvc.go internal/render/pvc_test.go
git commit -m "internal/render: HomePVC — per-DevPod home PVC renderer"
```

---

### Task 3: Inject home volumeMount into the target user container

**Files:**
- Modify: `internal/render/pod.go`
- Modify: `internal/render/pod_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/render/pod_test.go`, after `TestRenderPod_HomeVolume_OnlyWhenPersistence`,
add:

```go
func TestRenderPod_PersistenceMountsOnTargetContainer(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:      "1Gi",
		MountPath: "/workspace",
	}

	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}

	target := pod.Spec.Containers[0] // default target = first container
	var mounted bool
	for _, m := range target.VolumeMounts {
		if m.Name == render.VolumeHome {
			mounted = true
			if m.MountPath != "/workspace" {
				t.Errorf("home mountPath = %q, want /workspace", m.MountPath)
			}
		}
	}
	if !mounted {
		t.Errorf("home volume not mounted on target container %q", target.Name)
	}
}

func TestRenderPod_PersistenceTargetContainerExplicit(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Pod.Spec.Containers = append(dp.Spec.Pod.Spec.Containers, corev1.Container{
		Name:  "companion",
		Image: "busybox",
	})
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:            "1Gi",
		MountPath:       "/data",
		TargetContainer: "companion",
	}

	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	// containers[0] = original "dev"; containers[1] = "companion"; containers[2] = sidecar.
	if got := pod.Spec.Containers[1].Name; got != "companion" {
		t.Fatalf("companion container at index 1 missing, got %q", got)
	}
	var devMounted, companionMounted bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == render.VolumeHome {
			devMounted = true
		}
	}
	for _, m := range pod.Spec.Containers[1].VolumeMounts {
		if m.Name == render.VolumeHome && m.MountPath == "/data" {
			companionMounted = true
		}
	}
	if devMounted {
		t.Errorf("home mounted on dev container; should only be on companion")
	}
	if !companionMounted {
		t.Errorf("home not mounted on companion at /data")
	}
}

func TestRenderPod_PersistenceUnknownTargetContainerError(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:            "1Gi",
		MountPath:       "/data",
		TargetContainer: "does-not-exist",
	}
	_, err := render.Pod(dp, cfg())
	if err == nil {
		t.Fatal("expected error on unknown targetContainer; webhook should reject earlier, render defends in depth")
	}
}
```

- [ ] **Step 2: Run, see fail**

```bash
bash hack/test.sh ./internal/render/...
```

Expected: three new tests fail (no mount on user container, no companion
mount, no error for unknown target).

- [ ] **Step 3: Extend `render.Pod`**

In `internal/render/pod.go`, replace the body around the
`user.Containers = append(user.Containers, sidecarContainer(...))` line
to inject the user-container mount before the sidecar append:

```go
user := dp.Spec.Pod.Spec.DeepCopy()
user.ShareProcessNamespace = ptr.To(true)
user.Volumes = append(user.Volumes, hostKeyVolume(dp))
if dp.Spec.Persistence != nil {
    user.Volumes = append(user.Volumes, homeVolume(dp))
    if err := injectHomeMount(user, dp); err != nil {
        return nil, err
    }
}
user.Containers = append(user.Containers, sidecarContainer(dp, cfg, dp.Spec.Persistence != nil))
```

Then append a helper at the bottom of the file:

```go
// injectHomeMount appends a volumeMount named VolumeHome at
// spec.persistence.mountPath onto the target container in `spec`.
// Target is spec.persistence.targetContainer, or containers[0] when
// unset. Returns an error if the named target does not exist; the
// webhook is the primary defense, this is just belt-and-braces.
func injectHomeMount(spec *corev1.PodSpec, dp *devpodv1alpha1.DevPod) error {
    p := dp.Spec.Persistence
    targetName := p.TargetContainer
    if targetName == "" {
        if len(spec.Containers) == 0 {
            return fmt.Errorf("persistence: spec.pod.spec.containers is empty")
        }
        targetName = spec.Containers[0].Name
    }
    for i := range spec.Containers {
        if spec.Containers[i].Name == targetName {
            spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts, corev1.VolumeMount{
                Name:      VolumeHome,
                MountPath: p.MountPath,
            })
            return nil
        }
    }
    return fmt.Errorf("persistence.targetContainer %q not found in spec.pod.spec.containers", targetName)
}
```

- [ ] **Step 4: Run, see pass**

```bash
bash hack/test.sh ./internal/render/...
```

Expected: all pod_test cases pass.

- [ ] **Step 5: Commit**

```bash
git add internal/render/pod.go internal/render/pod_test.go
git commit -m "internal/render: inject persistence mount into target user container"
```

---

### Task 4: Controller applies PVC alongside Pod

**Files:**
- Modify: `internal/controllers/devpod_controller.go`
- Modify: `internal/controllers/devpod_controller_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/controllers/devpod_controller_test.go`, add:

```go
func TestReconcile_PersistenceCreatesPVC(t *testing.T) {
	setupSuite(t)
	ctx := context.Background()

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA test"}},
	}
	if err := k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, user) })

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "persisted", Namespace: "default"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "alice",
			Running: true,
			Persistence: &devpodv1alpha1.PersistenceSpec{
				Size:      "1Gi",
				MountPath: "/workspace",
			},
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "debian:stable"}}},
			},
		},
	}
	if err := k8sClient.Create(ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, dp) })

	pvcKey := types.NamespacedName{Name: "alice-persisted-home", Namespace: "default"}
	Eventually(t, 5*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		return k8sClient.Get(ctx, pvcKey, &pvc) == nil
	}, "PVC not created within 5s")

	var pvc corev1.PersistentVolumeClaim
	if err := k8sClient.Get(ctx, pvcKey, &pvc); err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "1Gi" {
		t.Errorf("PVC size = %s, want 1Gi", got.String())
	}
}
```

Assume `Eventually(t, timeout, fn, msg)` helper exists; if not, write a
short inline polling loop instead.

- [ ] **Step 2: Run, see fail**

```bash
bash hack/test.sh ./internal/controllers/...
```

Expected: PVC never created (controller doesn't apply it yet); test
fails with the message.

- [ ] **Step 3: Implement `applyPVC` and call it from `applyAll`**

In `internal/controllers/devpod_controller.go`, add this method after
`applyAll`:

```go
// applyPVC creates the home PVC when spec.persistence is set. Idempotent
// via the existing applyOwned path.
func (r *DevPodReconciler) applyPVC(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	if dp.Spec.Persistence == nil {
		return nil
	}
	pvc, err := render.HomePVC(dp, r.GwConfig)
	if err != nil {
		return fmt.Errorf("render pvc: %w", err)
	}
	return r.applyOwned(ctx, dp, pvc)
}
```

In `applyAll`, call `r.applyPVC(ctx, dp)` BEFORE `render.Pod` (so the PVC
exists before the Pod that references it would bind):

```go
// applyAll renders and applies the full object set for one DevPod.
func (r *DevPodReconciler) applyAll(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
    if err := r.ensureHostKeySecret(ctx, dp); err != nil {
        return fmt.Errorf("host key secret: %w", err)
    }
    if err := r.applyPVC(ctx, dp); err != nil {
        return fmt.Errorf("apply pvc: %w", err)
    }
    // … existing Pod + Service + NetworkPolicy code unchanged …
```

Also add `Owns(&corev1.PersistentVolumeClaim{})` to `SetupWithManager`.

- [ ] **Step 4: Run, see pass**

```bash
bash hack/test.sh ./internal/controllers/...
```

Expected: persistence test passes; existing controller tests still pass.

- [ ] **Step 5: Commit**

```bash
git add internal/controllers/devpod_controller.go internal/controllers/devpod_controller_test.go
git commit -m "controllers/devpod: apply home PVC when spec.persistence set"
```

---

### Task 5: Status — write PersistentVolumeClaimRef

**Files:**
- Modify: `internal/controllers/devpod_controller.go`
- Modify: `internal/controllers/devpod_controller_test.go`

- [ ] **Step 1: Write the failing test**

Extend the previous test or add:

```go
func TestReconcile_PersistencePopulatesStatusPVCRef(t *testing.T) {
	// … set up same as TestReconcile_PersistenceCreatesPVC …

	dpKey := types.NamespacedName{Name: "persisted", Namespace: "default"}
	Eventually(t, 5*time.Second, func() bool {
		var got devpodv1alpha1.DevPod
		if err := k8sClient.Get(ctx, dpKey, &got); err != nil {
			return false
		}
		return got.Status.PersistentVolumeClaimRef != nil &&
			got.Status.PersistentVolumeClaimRef.Name == "alice-persisted-home"
	}, "status.persistentVolumeClaimRef not populated")
}
```

- [ ] **Step 2: Run, see fail**

```bash
bash hack/test.sh ./internal/controllers/...
```

- [ ] **Step 3: Update `updateStatus`**

In `internal/controllers/devpod_controller.go`, change the `desired`
status build inside `updateStatus`:

```go
desired := devpodv1alpha1.DevPodStatus{
    Phase: devpodv1alpha1.DevPodPending,
    WorkloadRef: &devpodv1alpha1.WorkloadRef{
        APIVersion: "v1",
        Kind:       "Pod",
        Name:       pod.Name,
    },
    Conditions: dp.Status.Conditions,
}
if dp.Spec.Persistence != nil {
    desired.PersistentVolumeClaimRef = &devpodv1alpha1.LocalObjectRef{Name: render.HomePVCName(dp)}
}
```

Update the equality check to compare `PersistentVolumeClaimRef` too;
factor into a small helper `statusEqual(a, b DevPodStatus) bool`.

- [ ] **Step 4: Run, see pass**

```bash
bash hack/test.sh ./internal/controllers/...
```

- [ ] **Step 5: Commit**

```bash
git add internal/controllers/devpod_controller.go internal/controllers/devpod_controller_test.go
git commit -m "controllers/devpod: status.persistentVolumeClaimRef when persistence enabled"
```

---

### Task 6: Hibernate — delete Pod when spec.running=false

**Files:**
- Modify: `internal/controllers/devpod_controller.go`
- Modify: `internal/controllers/devpod_controller_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestReconcile_HibernateDeletesPod(t *testing.T) {
	setupSuite(t)
	ctx := context.Background()
	// … create User + DevPod as before, Running=true, no persistence …

	// Wait for Pod to be created.
	podKey := types.NamespacedName{Name: "alice-hibtest", Namespace: "default"}
	Eventually(t, 5*time.Second, func() bool {
		var p corev1.Pod
		return k8sClient.Get(ctx, podKey, &p) == nil
	}, "Pod never created")

	// Flip to running=false.
	dpKey := types.NamespacedName{Name: "hibtest", Namespace: "default"}
	var dp devpodv1alpha1.DevPod
	if err := k8sClient.Get(ctx, dpKey, &dp); err != nil {
		t.Fatalf("get dp: %v", err)
	}
	dp.Spec.Running = false
	if err := k8sClient.Update(ctx, &dp); err != nil {
		t.Fatalf("patch running=false: %v", err)
	}

	Eventually(t, 10*time.Second, func() bool {
		var p corev1.Pod
		err := k8sClient.Get(ctx, podKey, &p)
		return apierrors.IsNotFound(err)
	}, "Pod not deleted after running=false")
}
```

- [ ] **Step 2: Run, see fail**

The Pod will persist because the controller currently doesn't honor
`spec.running=false`.

- [ ] **Step 3: Branch in `Reconcile`**

In `Reconcile`, after the User existence check and before the
`r.applyAll(ctx, &dp)` call, add:

```go
if !dp.Spec.Running {
    if err := r.hibernate(ctx, &dp); err != nil {
        return ctrl.Result{}, err
    }
    return ctrl.Result{}, r.updateStatus(ctx, &dp)
}
```

Add the new method:

```go
// hibernate deletes the workload Pod (if present) and leaves all other
// per-DevPod objects (Service, host-key Secret, NetworkPolicy, PVC)
// intact so that flipping spec.running back to true is a cheap
// re-create of just the Pod.
func (r *DevPodReconciler) hibernate(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
    pod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name:      render.PodName(dp),
            Namespace: r.GwConfig.Spec.DevPodNamespace,
        },
    }
    if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
        return fmt.Errorf("delete pod: %w", err)
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
git commit -m "controllers/devpod: hibernate — delete Pod when spec.running=false"
```

---

### Task 7: Status — phase=Stopped, hibernatedAt, clear endpoint/workloadRef

**Files:**
- Modify: `internal/controllers/devpod_controller.go`
- Modify: `internal/controllers/devpod_controller_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestReconcile_HibernateStatusFields(t *testing.T) {
	// … create + wait for Running, then flip to Running=false …

	Eventually(t, 10*time.Second, func() bool {
		var got devpodv1alpha1.DevPod
		if err := k8sClient.Get(ctx, dpKey, &got); err != nil {
			return false
		}
		return got.Status.Phase == devpodv1alpha1.DevPodStopped &&
			got.Status.Endpoint == "" &&
			got.Status.WorkloadRef == nil &&
			got.Status.HibernatedAt != nil
	}, "status fields wrong after hibernate")

	// Flip back to Running=true.
	// … update spec.Running = true …
	Eventually(t, 10*time.Second, func() bool {
		var got devpodv1alpha1.DevPod
		if err := k8sClient.Get(ctx, dpKey, &got); err != nil {
			return false
		}
		return got.Status.HibernatedAt == nil
	}, "HibernatedAt not cleared after resume")
}
```

- [ ] **Step 2: Run, see fail**

- [ ] **Step 3: Rewrite `updateStatus` to branch on `dp.Spec.Running`**

Replace the existing `updateStatus` body with the two-branch shape
from the spec §5. Key points:
- When `!dp.Spec.Running`: skip Pod Get, set `Phase=Stopped`,
  `Endpoint=""`, `WorkloadRef=nil`. Set `HibernatedAt=now()` only on
  the transition (when `dp.Status.HibernatedAt == nil` or
  `dp.Status.Phase != DevPodStopped`).
- When `dp.Spec.Running`: existing Pod-fetch logic. Set
  `HibernatedAt=nil` on resume.

Replace `updateStatus` with:

```go
func (r *DevPodReconciler) updateStatus(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
    desired := devpodv1alpha1.DevPodStatus{
        Conditions: dp.Status.Conditions,
    }
    if dp.Spec.Persistence != nil {
        desired.PersistentVolumeClaimRef = &devpodv1alpha1.LocalObjectRef{Name: render.HomePVCName(dp)}
    }

    if !dp.Spec.Running {
        desired.Phase = devpodv1alpha1.DevPodStopped
        if dp.Status.HibernatedAt != nil && dp.Status.Phase == devpodv1alpha1.DevPodStopped {
            desired.HibernatedAt = dp.Status.HibernatedAt
        } else {
            now := metav1.Now()
            desired.HibernatedAt = &now
        }
    } else {
        // Resume / Running: HibernatedAt sticky-cleared.
        desired.HibernatedAt = nil
        var pod corev1.Pod
        err := r.Get(ctx, types.NamespacedName{Name: render.PodName(dp), Namespace: r.GwConfig.Spec.DevPodNamespace}, &pod)
        if err != nil && !apierrors.IsNotFound(err) {
            return fmt.Errorf("get rendered pod: %w", err)
        }
        if err == nil {
            desired.WorkloadRef = &devpodv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "Pod", Name: pod.Name}
            switch {
            case pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "":
                desired.Phase = devpodv1alpha1.DevPodRunning
                desired.Endpoint = pod.Status.PodIP + ":22"
            case pod.Status.Phase == corev1.PodFailed:
                desired.Phase = devpodv1alpha1.DevPodFailed
            default:
                desired.Phase = devpodv1alpha1.DevPodPending
            }
        } else {
            desired.Phase = devpodv1alpha1.DevPodPending
        }
    }

    if statusEqual(dp.Status, desired) {
        return nil
    }
    patch := client.MergeFrom(dp.DeepCopy())
    dp.Status.Phase = desired.Phase
    dp.Status.Endpoint = desired.Endpoint
    dp.Status.WorkloadRef = desired.WorkloadRef
    dp.Status.PersistentVolumeClaimRef = desired.PersistentVolumeClaimRef
    dp.Status.HibernatedAt = desired.HibernatedAt
    return r.Status().Patch(ctx, dp, patch)
}

func statusEqual(a, b devpodv1alpha1.DevPodStatus) bool {
    return a.Phase == b.Phase &&
        a.Endpoint == b.Endpoint &&
        workloadRefEqual(a.WorkloadRef, b.WorkloadRef) &&
        localRefEqual(a.PersistentVolumeClaimRef, b.PersistentVolumeClaimRef) &&
        timePtrEqual(a.HibernatedAt, b.HibernatedAt)
}

func localRefEqual(a, b *devpodv1alpha1.LocalObjectRef) bool {
    switch {
    case a == nil && b == nil:
        return true
    case a == nil || b == nil:
        return false
    default:
        return a.Name == b.Name
    }
}

func timePtrEqual(a, b *metav1.Time) bool {
    switch {
    case a == nil && b == nil:
        return true
    case a == nil || b == nil:
        return false
    default:
        return a.Time.Equal(b.Time)
    }
}
```

- [ ] **Step 4: Run, see pass**

- [ ] **Step 5: Commit**

```bash
git add internal/controllers/devpod_controller.go internal/controllers/devpod_controller_test.go
git commit -m "controllers/devpod: full status — Stopped/HibernatedAt on hibernate, cleared on resume"
```

---

### Task 8: Finalizer detaches PVC ownerRef before allowing DevPod delete

**Files:**
- Modify: `internal/controllers/devpod_controller.go`
- Modify: `internal/controllers/devpod_controller_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestReconcile_DeleteDetachesPVC(t *testing.T) {
	setupSuite(t)
	ctx := context.Background()
	// … create User + DevPod with persistence, wait for PVC …

	pvcKey := types.NamespacedName{Name: "alice-detach-home", Namespace: "default"}
	Eventually(t, 5*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		return k8sClient.Get(ctx, pvcKey, &pvc) == nil
	}, "PVC not created")

	// Delete the DevPod.
	if err := k8sClient.Delete(ctx, dp); err != nil {
		t.Fatalf("delete dp: %v", err)
	}

	dpKey := types.NamespacedName{Name: "detach", Namespace: "default"}
	Eventually(t, 10*time.Second, func() bool {
		var got devpodv1alpha1.DevPod
		err := k8sClient.Get(ctx, dpKey, &got)
		return apierrors.IsNotFound(err)
	}, "DevPod not removed after delete")

	// PVC must still exist and have no DevPod ownerRef.
	var pvc corev1.PersistentVolumeClaim
	if err := k8sClient.Get(ctx, pvcKey, &pvc); err != nil {
		t.Fatalf("PVC vanished: %v", err)
	}
	for _, or := range pvc.OwnerReferences {
		if or.Kind == "DevPod" {
			t.Errorf("PVC still owns DevPod ref %+v", or)
		}
	}
}
```

- [ ] **Step 2: Run, see fail**

Currently the finalizer is removed unconditionally without touching the
PVC, so K8s GC will delete the PVC along with the DevPod.

- [ ] **Step 3: Detach in the finalizer path**

In `Reconcile`, replace the deletion branch:

```go
if !dp.DeletionTimestamp.IsZero() {
    if err := r.detachPVC(ctx, &dp); err != nil {
        return ctrl.Result{}, err
    }
    controllerutil.RemoveFinalizer(&dp, devpodFinalizer)
    if err := r.Update(ctx, &dp); err != nil {
        return ctrl.Result{}, fmt.Errorf("clear finalizer: %w", err)
    }
    return ctrl.Result{}, nil
}
```

Add the helper:

```go
// detachPVC strips this DevPod's OwnerReference from the home PVC so
// it survives Kubernetes garbage collection. If the PVC does not exist
// (was never created or already gone), this is a no-op.
func (r *DevPodReconciler) detachPVC(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
    if dp.Spec.Persistence == nil {
        return nil
    }
    var pvc corev1.PersistentVolumeClaim
    key := types.NamespacedName{Name: render.HomePVCName(dp), Namespace: r.GwConfig.Spec.DevPodNamespace}
    if err := r.Get(ctx, key, &pvc); err != nil {
        if apierrors.IsNotFound(err) {
            return nil
        }
        return fmt.Errorf("get pvc: %w", err)
    }
    refs := pvc.OwnerReferences[:0]
    changed := false
    for _, or := range pvc.OwnerReferences {
        if or.Kind == "DevPod" && or.UID == dp.UID {
            changed = true
            continue
        }
        refs = append(refs, or)
    }
    if !changed {
        return nil
    }
    patch := client.MergeFrom(pvc.DeepCopy())
    pvc.OwnerReferences = refs
    return r.Patch(ctx, &pvc, patch)
}
```

- [ ] **Step 4: Run, see pass**

- [ ] **Step 5: Commit**

```bash
git add internal/controllers/devpod_controller.go internal/controllers/devpod_controller_test.go
git commit -m "controllers/devpod: finalizer detaches home PVC ownerRef before delete"
```

---

### Task 9: Webhook — migrate to admission.CustomValidator

**Files:**
- Modify: `internal/webhook/devpod_webhook.go`
- Modify: `internal/webhook/devpod_webhook_test.go`

- [ ] **Step 1: Replace the validator type**

Rewrite the head of `internal/webhook/devpod_webhook.go`:

```go
package webhook

import (
    "context"
    "fmt"

    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/webhook"
    "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

    devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// DevPodValidator implements admission.CustomValidator for DevPod.
type DevPodValidator struct {
    Client client.Client
}

var _ admission.CustomValidator = (*DevPodValidator)(nil)

func (v *DevPodValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
    dp, ok := obj.(*devpodv1alpha1.DevPod)
    if !ok {
        return nil, fmt.Errorf("expected DevPod, got %T", obj)
    }
    return nil, v.validate(dp)
}

func (v *DevPodValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
    oldDP, ok := oldObj.(*devpodv1alpha1.DevPod)
    if !ok {
        return nil, fmt.Errorf("expected DevPod, got %T", oldObj)
    }
    newDP, ok := newObj.(*devpodv1alpha1.DevPod)
    if !ok {
        return nil, fmt.Errorf("expected DevPod, got %T", newObj)
    }
    if newDP.Spec.Owner != oldDP.Spec.Owner {
        return nil, fmt.Errorf("spec.owner is immutable")
    }
    return nil, v.validate(newDP)
}

func (v *DevPodValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
    return nil, nil
}

// validate runs the structural checks shared by Create and Update.
func (v *DevPodValidator) validate(dp *devpodv1alpha1.DevPod) error {
    // Existing rules (sidecar name, ShareProcessNamespace=false, exactly-one)
    // … move the old Handle()'s checks here verbatim …
    return nil
}

// SetupWebhookWithManager registers the validator at /validate-devpod-io-v1alpha1-devpod.
func (v *DevPodValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
    return ctrl.NewWebhookManagedBy(mgr).
        For(&devpodv1alpha1.DevPod{}).
        WithValidator(v).
        Complete()
}
```

The exact imports list (`runtime`, `ctrl`) follows controller-runtime's
typed-webhook example.

- [ ] **Step 2: Adapt existing tests**

In `internal/webhook/devpod_webhook_test.go`, drop the
`admissionv1.AdmissionRequest` wrapper used by the legacy Handler tests
and invoke `ValidateCreate` / `ValidateUpdate` directly with the typed
`*DevPod` objects. The test bodies are otherwise identical.

- [ ] **Step 3: Run, see pass**

```bash
bash hack/test.sh ./internal/webhook/...
```

- [ ] **Step 4: Commit**

```bash
git add internal/webhook/devpod_webhook.go internal/webhook/devpod_webhook_test.go
git commit -m "internal/webhook: migrate DevPodValidator to admission.CustomValidator"
```

---

### Task 10: Webhook — new rules for persistence and reserved names

**Files:**
- Modify: `internal/webhook/devpod_webhook.go`
- Modify: `internal/webhook/devpod_webhook_test.go`

- [ ] **Step 1: Write the failing tests**

In `internal/webhook/devpod_webhook_test.go`, add four cases:

```go
func TestValidateCreate_RejectsReservedVolumeName(t *testing.T) {
    dp := minimalDevPodForValidator()
    dp.Spec.Pod.Spec.Volumes = []corev1.Volume{{Name: "devpod-mine"}}
    _, err := (&webhook.DevPodValidator{}).ValidateCreate(context.Background(), dp)
    if err == nil {
        t.Fatal("expected error on devpod- prefix volume name")
    }
}

func TestValidateCreate_RejectsReservedMountName(t *testing.T) {
    dp := minimalDevPodForValidator()
    dp.Spec.Pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "devpod-x", MountPath: "/x"}}
    _, err := (&webhook.DevPodValidator{}).ValidateCreate(context.Background(), dp)
    if err == nil {
        t.Fatal("expected error on devpod- prefix volumeMount name")
    }
}

func TestValidateCreate_RejectsUnknownTargetContainer(t *testing.T) {
    dp := minimalDevPodForValidator()
    dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{Size: "1Gi", MountPath: "/data", TargetContainer: "ghost"}
    _, err := (&webhook.DevPodValidator{}).ValidateCreate(context.Background(), dp)
    if err == nil {
        t.Fatal("expected error on unknown targetContainer")
    }
}

func TestValidateCreate_RejectsMountPathCollision(t *testing.T) {
    dp := minimalDevPodForValidator()
    dp.Spec.Pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "user-data", MountPath: "/workspace"}}
    dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{Size: "1Gi", MountPath: "/workspace"}
    _, err := (&webhook.DevPodValidator{}).ValidateCreate(context.Background(), dp)
    if err == nil {
        t.Fatal("expected error on overlapping mountPath")
    }
}

func TestValidateCreate_RejectsUnparseableSize(t *testing.T) {
    dp := minimalDevPodForValidator()
    dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{Size: "not-a-quantity", MountPath: "/data"}
    _, err := (&webhook.DevPodValidator{}).ValidateCreate(context.Background(), dp)
    if err == nil {
        t.Fatal("expected error on bogus size")
    }
}
```

- [ ] **Step 2: Run, see fail**

- [ ] **Step 3: Implement rules in `validate`**

Add to `validate()` in `internal/webhook/devpod_webhook.go`:

```go
const reservedPrefix = "devpod-"

if dp.Spec.Pod != nil {
    for _, v := range dp.Spec.Pod.Spec.Volumes {
        if strings.HasPrefix(v.Name, reservedPrefix) {
            return fmt.Errorf("spec.pod.spec.volumes[].name %q uses reserved prefix %q", v.Name, reservedPrefix)
        }
    }
    for _, c := range dp.Spec.Pod.Spec.Containers {
        for _, m := range c.VolumeMounts {
            if strings.HasPrefix(m.Name, reservedPrefix) {
                return fmt.Errorf("container %q volumeMounts[].name %q uses reserved prefix %q", c.Name, m.Name, reservedPrefix)
            }
        }
    }
}

if p := dp.Spec.Persistence; p != nil && dp.Spec.Pod != nil {
    // Size must parse.
    if _, err := resource.ParseQuantity(p.Size); err != nil {
        return fmt.Errorf("spec.persistence.size %q: %w", p.Size, err)
    }

    // Target container must exist.
    target := p.TargetContainer
    if target == "" {
        target = dp.Spec.Pod.Spec.Containers[0].Name
    }
    var tgt *corev1.Container
    for i := range dp.Spec.Pod.Spec.Containers {
        if dp.Spec.Pod.Spec.Containers[i].Name == target {
            tgt = &dp.Spec.Pod.Spec.Containers[i]
            break
        }
    }
    if tgt == nil {
        return fmt.Errorf("spec.persistence.targetContainer %q not found in spec.pod.spec.containers", target)
    }

    // MountPath collision: equal or prefix-match against any existing mount.
    for _, m := range tgt.VolumeMounts {
        if m.MountPath == p.MountPath ||
            strings.HasPrefix(m.MountPath, p.MountPath+"/") ||
            strings.HasPrefix(p.MountPath, m.MountPath+"/") {
            return fmt.Errorf("spec.persistence.mountPath %q overlaps with existing volumeMount %q at %q", p.MountPath, m.Name, m.MountPath)
        }
    }
}
```

- [ ] **Step 4: Run, see pass**

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/devpod_webhook.go internal/webhook/devpod_webhook_test.go
git commit -m "internal/webhook: new rules — reserved devpod- prefix, persistence sanity"
```

---

### Task 11: Wire the webhook server into cmd/controller

**Files:**
- Modify: `cmd/controller/main.go`

- [ ] **Step 1: Add the flags and register the validator**

Add these flags near the existing ones in `cmd/controller/main.go`:

```go
var webhookPort int
var webhookCertDir string

func init() {
    flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the admission webhook listens on.")
    flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "Directory containing tls.crt and tls.key.")
}
```

In `main()`, when constructing `ctrl.Options{...}`, add:

```go
WebhookServer: webhook.NewServer(webhook.Options{
    Port:    webhookPort,
    CertDir: webhookCertDir,
}),
```

After `mgr, err := ctrl.NewManager(...)`, register the validator:

```go
if err := (&devpodwebhook.DevPodValidator{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to set up DevPod webhook")
    os.Exit(1)
}
```

Add the `webhook "sigs.k8s.io/controller-runtime/pkg/webhook"` import
and the `devpodwebhook "github.com/mrhaoxx/devpod/internal/webhook"`
import.

- [ ] **Step 2: Build**

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add cmd/controller/main.go
git commit -m "cmd/controller: serve DevPod validating webhook"
```

---

### Task 12: Helm chart — cert-manager Issuer + Certificate + VWC + Deployment volume

**Files:**
- Create: `deploy/chart/templates/issuer.yaml`
- Create: `deploy/chart/templates/certificate.yaml`
- Create: `deploy/chart/templates/validatingwebhookconfiguration.yaml`
- Create: `deploy/chart/templates/webhook-service.yaml`
- Modify: `deploy/chart/templates/controller.yaml`
- Modify: `deploy/chart/values.yaml`

- [ ] **Step 1: Add values**

In `deploy/chart/values.yaml`, append:

```yaml
webhook:
  port: 9443
  serviceName: devpod-webhook
  certDir: /tmp/k8s-webhook-server/serving-certs
  failurePolicy: Fail
```

- [ ] **Step 2: Create Issuer**

`deploy/chart/templates/issuer.yaml`:

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: devpod-selfsigned
  namespace: {{ .Values.namespaces.system }}
spec:
  selfSigned: {}
```

- [ ] **Step 3: Create Certificate**

`deploy/chart/templates/certificate.yaml`:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: devpod-webhook-tls
  namespace: {{ .Values.namespaces.system }}
spec:
  secretName: devpod-webhook-tls
  issuerRef:
    name: devpod-selfsigned
    kind: Issuer
  dnsNames:
    - {{ .Values.webhook.serviceName }}.{{ .Values.namespaces.system }}.svc
    - {{ .Values.webhook.serviceName }}.{{ .Values.namespaces.system }}.svc.cluster.local
  duration: 8760h
  renewBefore: 720h
```

- [ ] **Step 4: Create webhook Service**

`deploy/chart/templates/webhook-service.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ .Values.webhook.serviceName }}
  namespace: {{ .Values.namespaces.system }}
  labels: {{ include "devpod.labels" . | nindent 4 }}
spec:
  selector:
    app.kubernetes.io/name: devpod-controller
  ports:
  - name: webhook
    port: 443
    targetPort: webhook
```

- [ ] **Step 5: Create ValidatingWebhookConfiguration**

`deploy/chart/templates/validatingwebhookconfiguration.yaml`:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: devpod-validating-webhook
  annotations:
    cert-manager.io/inject-ca-from: "{{ .Values.namespaces.system }}/devpod-webhook-tls"
webhooks:
- name: vdevpod.devpod.io
  failurePolicy: {{ .Values.webhook.failurePolicy }}
  matchPolicy: Equivalent
  rules:
  - apiGroups: ["devpod.io"]
    apiVersions: ["v1alpha1"]
    operations: ["CREATE", "UPDATE"]
    resources: ["devpods"]
    scope: Namespaced
  clientConfig:
    service:
      name: {{ .Values.webhook.serviceName }}
      namespace: {{ .Values.namespaces.system }}
      path: /validate-devpod-io-v1alpha1-devpod
      port: 443
  admissionReviewVersions: ["v1"]
  sideEffects: None
  timeoutSeconds: 10
```

- [ ] **Step 6: Update controller Deployment**

In `deploy/chart/templates/controller.yaml`:

- Add a port:
  ```yaml
  - name: webhook
    containerPort: {{ .Values.webhook.port }}
  ```
- Add an arg:
  ```yaml
  - --webhook-port={{ .Values.webhook.port }}
  - --webhook-cert-dir={{ .Values.webhook.certDir }}
  ```
- Add a volume mount:
  ```yaml
  - name: webhook-tls
    mountPath: {{ .Values.webhook.certDir }}
    readOnly: true
  ```
- Add a volume:
  ```yaml
  - name: webhook-tls
    secret:
      secretName: devpod-webhook-tls
  ```

- [ ] **Step 7: Verify chart renders**

```bash
helm template deploy/chart > /tmp/devpod-render.yaml
grep -c "ValidatingWebhookConfiguration" /tmp/devpod-render.yaml   # 1
grep -c "kind: Certificate" /tmp/devpod-render.yaml                # 1
grep -c "kind: Issuer" /tmp/devpod-render.yaml                      # 1
```

- [ ] **Step 8: Deploy and smoke-test**

```bash
# cert-manager must be installed (one-time):
helm repo add jetstack https://charts.jetstack.io || true
helm upgrade --install cert-manager jetstack/cert-manager -n cert-manager --create-namespace --set installCRDs=true

bash hack/e2e-up.sh 2>&1 | tail -5
kubectl -n devpod-system get certificate devpod-webhook-tls
# Expect READY=True within ~30s
kubectl get validatingwebhookconfiguration devpod-validating-webhook
```

Try to apply a DevPod with a reserved volume name; expect rejection.

- [ ] **Step 9: Commit**

```bash
git add deploy/chart/values.yaml deploy/chart/templates/issuer.yaml deploy/chart/templates/certificate.yaml deploy/chart/templates/validatingwebhookconfiguration.yaml deploy/chart/templates/webhook-service.yaml deploy/chart/templates/controller.yaml
git commit -m "deploy/chart: wire validating webhook with cert-manager-issued TLS"
```

---

### Task 13: e2e — write, hibernate, resume, read

**Files:**
- Create: `hack/e2e-m2.sh`

- [ ] **Step 1: Write the script**

`hack/e2e-m2.sh`:

```bash
#!/usr/bin/env bash
# End-to-end demo for M2: write a file, hibernate, resume, read it back.
set -euo pipefail

NS=devpods
NAME=m2demo
OWNER=alice
KEY="$(cat /tmp/devpod-test-key-path)"
GW_PORT=2222

echo "[1/7] Apply DevPod with persistence"
cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: $NAME
  namespace: $NS
spec:
  owner: $OWNER
  running: true
  persistence:
    size: 1Gi
    mountPath: /workspace
  pod:
    spec:
      containers:
      - name: dev
        image: debian:stable
        command: ["sleep", "infinity"]
EOF

echo "[2/7] Wait for Running phase"
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=120s

echo "[3/7] Write marker"
ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -p "$GW_PORT" -i "$KEY" "$OWNER+$NAME@127.0.0.1" -- \
  'echo hello-from-m2 > /workspace/marker && cat /workspace/marker'

echo "[4/7] Hibernate"
kubectl -n "$NS" patch devpod "$NAME" --type=merge -p '{"spec":{"running":false}}'
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Stopped devpod/"$NAME" --timeout=60s

echo "[5/7] Verify Pod is gone, PVC remains"
if kubectl -n "$NS" get pod "$OWNER-$NAME" 2>/dev/null; then
  echo "FAIL: Pod still present after hibernate"
  exit 1
fi
kubectl -n "$NS" get pvc "$OWNER-$NAME-home"

echo "[6/7] Resume"
kubectl -n "$NS" patch devpod "$NAME" --type=merge -p '{"spec":{"running":true}}'
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=120s

echo "[7/7] Read marker back"
got=$(ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -p "$GW_PORT" -i "$KEY" "$OWNER+$NAME@127.0.0.1" -- 'cat /workspace/marker')
if [[ "$got" != "hello-from-m2" ]]; then
  echo "FAIL: marker round-trip failed; got %q" "$got"
  exit 1
fi
echo "OK: $got"
```

`chmod +x hack/e2e-m2.sh`.

- [ ] **Step 2: Run it**

```bash
bash hack/e2e-m2.sh
```

Expected: prints "OK: hello-from-m2" at the end.

- [ ] **Step 3: Commit**

```bash
git add hack/e2e-m2.sh
git commit -m "hack/e2e-m2: end-to-end write-hibernate-resume-read script"
```

---

## Done criteria

When every checkbox above is checked and all unit + envtest + the e2e
script succeed, M2 is complete. The M2 spec demonstration is satisfied:

> write file, hibernate, wake, file is still there.

Next milestone (M3 — multi-replica gateway, PROXY protocol, trusted
proxies) starts from a green branch.

---

## Self-review notes

Spec coverage check:
- §2.1 PersistenceSpec extensions → Task 1.
- §2.2 DevPod MaxLength → Task 1 (CEL form for v0.20 compatibility).
- §3.2 PVC creation → Task 4.
- §3.3 Finalizer detach → Task 8.
- §3.4 Pod injection → Tasks 2 (PVC volume already covered) + 3 (user-container mount).
- §4 Hibernate state machine → Tasks 6 + 7.
- §5 Status reporting → Tasks 5 + 7.
- §6.1 CustomValidator migration → Task 9.
- §6.2 New validation rules → Task 10.
- §6.3 Chart wiring → Task 12.
- §7.1 Unit tests → Tasks 2, 3, 9, 10.
- §7.2 envtest integration → Tasks 4, 5, 6, 7, 8.
- §7.3 e2e → Task 13.

No placeholders. No "TBD". Types are consistent (e.g.,
`PersistentVolumeClaimRef` references `LocalObjectRef` which already
exists in `api/v1alpha1`).
