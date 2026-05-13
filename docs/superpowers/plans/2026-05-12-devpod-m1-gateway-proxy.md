# DevPod M1-completion: SSH Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) tracking.

**Goal:** Make `ssh alice+hello@gateway -- uname -a` actually work end-to-end. The gateway authenticates the client against the User CRD, looks up the target DevPod, dials its sidecar, and proxies SSH channels + global requests bidirectionally — exactly like OpenNG/ngssh's pattern, but with K8s-driven dynamic host lookup.

**Architecture:** Three components co-evolve. The **controller** starts publishing `DevPod.status.endpoint` + `phase` and embeds the gateway's internal public key into each per-DevPod host-key Secret. The **sidecar** entrypoint installs that public key as the only entry in `/etc/devpod/authorized_keys`; its `ForceCommand` wrapper execs the SSH session as a shell within the sidecar's namespace (real `nsenter` into the user container is the *next* plan — M2). The **gateway** terminates client SSH, validates the client's public key against the `User` CRD, looks up the target `DevPod`, then opens a backend SSH connection to `status.endpoint` using the gateway's internal signing key, and pumps channels + global requests + per-channel requests both directions.

**Tech stack:**
- Same Go module, controller-runtime, k8s.io/api versions as the previous plan.
- `golang.org/x/crypto/ssh` for both server and client SSH primitives.
- `sigs.k8s.io/controller-runtime/pkg/cache` for the gateway's informer-cached watches.
- Reference implementation: `/Users/star/Documents/Projects/OpenNG/modules/ngssh/{proxy.go,midware.go}`.

**Spec:** `docs/superpowers/specs/2026-05-12-devpod-design.md` — §4.3, §4.4 in particular.

**Authorization model addition:** This plan extends the DevPod CRD with `spec.collaborators []string`. Any User in `collaborators` (in addition to `spec.owner`) may SSH into the DevPod. Only the owner may mutate `spec` (collaborator mutation is a webhook concern, deferred to a follow-up plan since the webhook itself is not yet served).

**Out of scope (deferred to follow-ups):**
- Real `nsenter` into the user container — `cmd/sidecar` execs in the sidecar's own namespace for this plan. Shell lands in alpine; user container files visible via `/proc/<userPID>/root/...` but not natively.
- `status.lastActivityTime` patching + idle hibernate — separate plan.
- `trustedProxyKeys` (the SSH-terminating outer-proxy bypass).
- `ValidatingWebhookConfiguration` + K8s-side webhook serving.
- Per-user `setuid` after nsenter (lands together with real nsenter).
- Multi-container DevPod target selection.

---

## File structure deltas

```
api/v1alpha1/devpod_types.go            (modify — add Collaborators)
api/v1alpha1/zz_generated.deepcopy.go   (regen)
config/crd/bases/devpod.io_devpods.yaml (regen)
deploy/chart/templates/crds/devpod.io_devpods.yaml  (resync)

internal/render/secret.go               (modify — embed gateway internal pubkey)
internal/render/pod.go                  (no change; host-key Secret mount already exposes the new key)
internal/controllers/devpod_controller.go  (modify — status writes + GatewayInternalPub field)
internal/controllers/suite_test.go      (modify — pass GatewayInternalPub)

images/sidecar/entrypoint.sh            (modify — install authorized_keys from mounted Secret)

cmd/sidecar/main.go                     (modify — exec sh / sftp-server based on SSH_ORIGINAL_COMMAND)

internal/gateway/auth.go                (new — User/DevPod-driven pubkey verification)
internal/gateway/auth_test.go           (new)
internal/gateway/dialer.go              (new — dial backend with internal key)
internal/gateway/dialer_test.go         (new — test with an in-process backend sshd)
internal/gateway/proxy.go               (new — channel + request pump, ported from OpenNG)
internal/gateway/proxy_test.go          (new — bidirectional pipe test)
internal/gateway/server.go              (new — wires Listener + Auth + Dialer + Proxy)
internal/gateway/server_test.go         (new — end-to-end test with in-process backend)

cmd/gateway/main.go                     (rewrite — informer-cached client + new server)

test/e2e/smoke_proxy_test.go            (new — `ssh alice+hello@gateway uname -a` returns alpine info)
```

---

## Conventions

Inherited from the previous plan:
- TDD per task: failing test first, run-fails, implement, run-passes, commit.
- Sentinel errors (`var ErrX = errors.New(...)`) + `fmt.Errorf("%w", ...)`.
- `ctx context.Context` is the first parameter on every blocking call.
- Functional options where >2 optional knobs exist.
- All subagent dispatches use `model: "opus"`.
- No Makefile; use `go build`, `go test`, and `bash hack/test.sh`.

---

## Tasks

### Task 1: Add `DevPod.spec.collaborators`

**Files:**
- Modify: `api/v1alpha1/devpod_types.go`
- Regen: `api/v1alpha1/zz_generated.deepcopy.go`
- Regen: `config/crd/bases/devpod.io_devpods.yaml`
- Resync: `deploy/chart/templates/crds/devpod.io_devpods.yaml`

- [ ] **Step 1: Add the field**

In `api/v1alpha1/devpod_types.go`, find the `DevPodSpec` struct and add a `Collaborators` field immediately after `Owner`:

```go
	// Owner names the User that owns this DevPod. Immutable.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="owner is immutable"
	Owner string `json:"owner"`

	// Collaborators lists additional Users (by name) who may SSH into
	// this DevPod. The owner is always implicitly authorized; collaborators
	// only gain SSH access — they cannot mutate spec (the webhook will
	// enforce this in a follow-up plan).
	//
	// +optional
	// +listType=set
	Collaborators []string `json:"collaborators,omitempty"`
```

- [ ] **Step 2: Regenerate**

```bash
go generate ./api/...
/bin/rm -rf config/      # we'll regenerate the CRD yaml in Step 4
```

- [ ] **Step 3: Verify compile**

```bash
go build ./...
bash hack/test.sh -run TestDevPodReconciler -v 2>&1 | tail -5
```

Expected: build clean; existing controller tests still pass (Collaborators is omitempty so older fixtures keep working).

- [ ] **Step 4: Regenerate CRD yaml and sync into chart**

```bash
mkdir -p config/crd/bases
go generate ./...
cp config/crd/bases/*.yaml deploy/chart/templates/crds/
grep -A1 collaborators config/crd/bases/devpod.io_devpods.yaml | head -5
```

Expected: the grep shows `collaborators:` under `spec.properties` with `type: array`, `x-kubernetes-list-type: set`.

- [ ] **Step 5: Commit**

```bash
git add api/ config/ deploy/chart/templates/crds/
git commit -m "api/v1alpha1: add DevPodSpec.Collaborators for shared access"
```

---

### Task 2: Render — embed gateway internal pubkey into host-key Secret

**Files:**
- Modify: `internal/render/secret.go`
- Modify: `internal/render/secret_test.go`

**Background:** Per spec §4.3, the sidecar's `authorized_keys` contains exactly one key: the gateway's internal pubkey. Today the sidecar's entrypoint installs an empty file (placeholder). We need the controller to embed the gateway internal pubkey into each per-DevPod host-key Secret as a third data key. The sidecar entrypoint (Task 4) then copies that key into `/etc/devpod/authorized_keys`.

We extend `HostKeySecret` to take an extra `authorizedKey []byte` argument. Existing callers pass the gateway internal pubkey bytes.

- [ ] **Step 1: Write the failing test (extend the existing one)**

Append to `internal/render/secret_test.go`:

```go
func TestHostKeySecret_EmbedsAuthorizedKey(t *testing.T) {
	dp := minimalDevPod()
	authKey := []byte("ssh-ed25519 AAAA gateway-internal\n")

	sec, err := render.HostKeySecret(dp, cfg(), &fixedReader{b: 0x42}, authKey)
	if err != nil {
		t.Fatalf("HostKeySecret: %v", err)
	}
	if got := sec.Data["authorized_keys"]; !bytes.Equal(got, authKey) {
		t.Errorf("authorized_keys data mismatch: got %q, want %q", got, authKey)
	}
}
```

- [ ] **Step 2: Run the test, expect FAIL**

```bash
go test ./internal/render/... -run TestHostKeySecret -v
```

Expected: FAIL — `HostKeySecret` does not accept the new argument.

- [ ] **Step 3: Update `internal/render/secret.go`**

```go
// HostKeySecret renders the per-DevPod sshd host-key Secret.
//
// authorizedKey is the gateway internal public key (OpenSSH
// authorized_keys line). The sidecar entrypoint installs it as
// /etc/devpod/authorized_keys, which is sshd's sole trusted key —
// SSH clients reach the sidecar exclusively through the gateway,
// never directly.
//
// randSrc is the entropy source for the ed25519 host-key generation;
// pass nil to use DefaultRand.
//
// Note: even with a deterministic randSrc, the rendered private-key
// bytes are not byte-stable across calls; ssh.MarshalPrivateKey draws
// a random checkint from crypto/rand internally.
func HostKeySecret(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig, randSrc io.Reader, authorizedKey []byte) (*corev1.Secret, error) {
	if randSrc == nil {
		randSrc = DefaultRand
	}

	pub, priv, err := ed25519.GenerateKey(randSrc)
	if err != nil {
		return nil, fmt.Errorf("generating ed25519 key: %w", err)
	}

	privPEM, err := ssh.MarshalPrivateKey(priv, "devpod sshd host key")
	if err != nil {
		return nil, fmt.Errorf("marshaling private key: %w", err)
	}
	privBytes := pem.EncodeToMemory(privPEM)

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("ssh.NewPublicKey: %w", err)
	}
	pubBytes := ssh.MarshalAuthorizedKey(sshPub)

	sec := &corev1.Secret{
		ObjectMeta: ObjectMeta(HostKeySecretName(dp), cfg.Spec.DevPodNamespace, dp),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ssh_host_ed25519_key":     privBytes,
			"ssh_host_ed25519_key.pub": pubBytes,
			"authorized_keys":          append([]byte(nil), authorizedKey...),
		},
	}
	return sec, nil
}
```

The two existing tests need adjustment to pass `nil` (or a placeholder) for the new argument. Update both `TestHostKeySecret_ProducesEd25519PEMAndPub` and `TestHostKeySecret_KeyTypeMarker` to pass a placeholder `[]byte("ssh-ed25519 AAAA test\n")` as the fourth arg.

- [ ] **Step 4: Run all render tests**

```bash
go test ./internal/render/... -v 2>&1 | tail -5
```

Expected: PASS (existing 16 + 1 new = 17 in render).

- [ ] **Step 5: Commit**

```bash
git add internal/render/secret.go internal/render/secret_test.go
git commit -m "internal/render: HostKeySecret embeds gateway internal pubkey as authorized_keys"
```

---

### Task 3: Controller — fetch gateway internal pubkey + write status + pass through to render

**Files:**
- Modify: `internal/controllers/devpod_controller.go`
- Modify: `internal/controllers/suite_test.go`

The controller needs to:
1. Read the gateway internal pubkey (from the Secret named by `GatewayConfig.spec.internalKeyRef`) at startup, cache it on the reconciler.
2. Pass that pubkey into `render.HostKeySecret(..., gatewayPub)`.
3. Once the Pod has an IP, write `DevPod.status.endpoint = "<podIP>:22"` and `status.phase = Running`. When Pod is Pending or missing, write `phase = Pending` and clear endpoint.

- [ ] **Step 1: Add reconciler fields**

In `internal/controllers/devpod_controller.go`, extend `DevPodReconciler`:

```go
type DevPodReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	GwConfig         *devpodv1alpha1.GatewayConfig
	GatewayNamespace string

	// GatewayInternalPub is the gateway's internal public key in
	// OpenSSH authorized_keys-line form, embedded into every per-DevPod
	// host-key Secret as the sidecar's sole authorized key.
	GatewayInternalPub []byte
}
```

- [ ] **Step 2: Update `ensureHostKeySecret` to pass the pubkey**

Locate the `sec, err := render.HostKeySecret(dp, r.GwConfig, nil)` call and change to:

```go
	sec, err := render.HostKeySecret(dp, r.GwConfig, nil, r.GatewayInternalPub)
```

- [ ] **Step 3: Add status writes**

Add a new step at the end of `applyAll`:

```go
	// 5. Status: phase + endpoint, derived from the rendered Pod.
	return r.updateStatus(ctx, dp)
}

func (r *DevPodReconciler) updateStatus(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Name: render.PodName(dp), Namespace: r.GwConfig.Spec.DevPodNamespace}, &pod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Pod not yet visible to the cache; next reconcile picks up.
			return nil
		}
		return fmt.Errorf("get rendered pod: %w", err)
	}

	desired := devpodv1alpha1.DevPodStatus{
		Phase:    devpodv1alpha1.DevPodPending,
		Endpoint: "",
		WorkloadRef: &devpodv1alpha1.WorkloadRef{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       pod.Name,
		},
		Conditions: dp.Status.Conditions, // preserve
	}
	if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
		desired.Phase = devpodv1alpha1.DevPodRunning
		desired.Endpoint = pod.Status.PodIP + ":22"
	} else if pod.Status.Phase == corev1.PodFailed {
		desired.Phase = devpodv1alpha1.DevPodFailed
	}

	// Skip the round-trip if nothing changed.
	if dp.Status.Phase == desired.Phase &&
		dp.Status.Endpoint == desired.Endpoint &&
		workloadRefEqual(dp.Status.WorkloadRef, desired.WorkloadRef) {
		return nil
	}

	patch := client.MergeFrom(dp.DeepCopy())
	dp.Status.Phase = desired.Phase
	dp.Status.Endpoint = desired.Endpoint
	dp.Status.WorkloadRef = desired.WorkloadRef
	return r.Status().Patch(ctx, dp, patch)
}

func workloadRefEqual(a, b *devpodv1alpha1.WorkloadRef) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}
```

- [ ] **Step 4: Update envtest setup to inject a fake pubkey**

In `internal/controllers/suite_test.go`, find the `(&controllers.DevPodReconciler{...}).SetupWithManager(mgr)` block and add `GatewayInternalPub`:

```go
	if err := (&controllers.DevPodReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		GwConfig:           defaultGwConfig(),
		GatewayNamespace:   "devpod-system",
		GatewayInternalPub: []byte("ssh-ed25519 AAAA test gateway-internal\n"),
	}).SetupWithManager(mgr); err != nil {
```

- [ ] **Step 5: Run controller tests**

```bash
bash hack/test.sh -run TestDevPodReconciler 2>&1 | tail -5
```

Expected: existing two tests still pass.

- [ ] **Step 6: Add a status-write test**

Append to `internal/controllers/devpod_controller_test.go`:

```go
func TestDevPodReconciler_WritesStatusEndpointAndPhase(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "diana"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA diana"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "stat", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "diana",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	// Wait for the Pod to be created, then patch its status to Running.
	deadline := time.Now().Add(30 * time.Second)
	var pod corev1.Pod
	for time.Now().Before(deadline) {
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "diana-stat", Namespace: "devpods"}, &pod); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pod.Name == "" {
		t.Fatalf("pod never appeared")
	}

	patch := client.MergeFrom(pod.DeepCopy())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.1.2.3"
	if err := env.Client.Status().Patch(env.Ctx, &pod, patch); err != nil {
		t.Fatalf("patch pod status: %v", err)
	}

	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "stat", Namespace: "devpods"}, &got); err == nil {
			if got.Status.Phase == devpodv1alpha1.DevPodRunning && got.Status.Endpoint == "10.1.2.3:22" {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("DevPod status.endpoint/phase never reached Running 10.1.2.3:22")
}
```

This test relies on the existing controller's reconcile being re-triggered by Pod status changes. Because `SetupWithManager` already calls `Owns(&corev1.Pod{})`, the watch is in place — patching the Pod's status triggers a DevPod reconcile.

- [ ] **Step 7: Run all controller tests**

```bash
bash hack/test.sh -run TestDevPodReconciler 2>&1 | tail -10
```

Expected: 3 PASS (existing 2 + 1 new).

- [ ] **Step 8: Commit**

```bash
git add internal/controllers/devpod_controller.go internal/controllers/devpod_controller_test.go internal/controllers/suite_test.go
git commit -m "internal/controllers: write DevPod.status.endpoint+phase and pass gateway pubkey to render"
```

---

### Task 4: Sidecar entrypoint — install authorized_keys from mounted Secret

**Files:**
- Modify: `images/sidecar/entrypoint.sh`

The host-key Secret now contains a third data key, `authorized_keys`, mounted via the existing `devpod-sshd-host-keys` volume at `/etc/devpod/host/`. We need the entrypoint to copy it into the canonical location `/etc/devpod/authorized_keys`.

- [ ] **Step 1: Edit `images/sidecar/entrypoint.sh`**

Replace the `: > /etc/devpod/authorized_keys` line (today's stub) with:

```bash
# Wait for the host key Secret to be mounted (kubelet writes it on Pod start).
for i in $(seq 1 30); do
  if [ -s /etc/devpod/host/ssh_host_ed25519_key ] && [ -f /etc/devpod/host/authorized_keys ]; then
    break
  fi
  echo "devpod-sshd: waiting for host key + authorized_keys ($i/30)"
  sleep 1
done

if [ ! -s /etc/devpod/host/ssh_host_ed25519_key ]; then
  echo "devpod-sshd: host key never appeared at /etc/devpod/host/ssh_host_ed25519_key" >&2
  exit 1
fi

# Install the gateway's pubkey as the sole authorized_keys entry. The
# host-key Secret carries it (controller-injected); we copy rather than
# symlink so sshd's strict ownership/permission checks pass.
mkdir -p /etc/devpod
install -m 0600 /etc/devpod/host/authorized_keys /etc/devpod/authorized_keys
```

Leave the rest of `entrypoint.sh` (install sshd config, supervise stub, exec sshd) unchanged.

- [ ] **Step 2: Verify the script still runs without docker (just syntax-check)**

```bash
sh -n images/sidecar/entrypoint.sh && echo "syntax OK"
```

Expected: "syntax OK".

- [ ] **Step 3: Rebuild the sidecar image (smoke test)**

```bash
docker build --platform=linux/arm64 -t devpod-sshd:proxy-dev -f images/sidecar/Dockerfile .
docker run --rm --entrypoint /bin/sh devpod-sshd:proxy-dev -c 'cat /etc/devpod/sshd_config.tmpl | grep AuthorizedKeysFile'
```

Expected: prints `AuthorizedKeysFile /etc/devpod/authorized_keys` (no functional regression; just confirms the image still bakes).

- [ ] **Step 4: Commit**

```bash
git add images/sidecar/entrypoint.sh
git commit -m "images/sidecar: install gateway pubkey from mounted host-key Secret"
```

---

### Task 5: Sidecar binary — exec sh / sftp-server based on SSH_ORIGINAL_COMMAND

**Files:**
- Modify: `cmd/sidecar/main.go`

The wrapper's job (today: log+exit-2) becomes: replicate sshd's standard handling of `ForceCommand` and the `sftp` subsystem. Real `nsenter` into the user container is deferred — for this plan we just exec in the sidecar's own namespace, which gives clients a working alpine shell. Multi-container DevPods will land in the sidecar's view; that's acceptable for proxy verification.

- [ ] **Step 1: Replace `cmd/sidecar/main.go` body**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-sidecar is the sshd-side wrapper invoked by the sidecar
// container's ForceCommand and SFTP Subsystem. In this plan, the wrapper
// executes the requested command (or a default shell) in the sidecar's
// own namespace. The next plan implements nsenter into the user container.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// sftpServerPath is the OpenSSH sftp-server path on Alpine.
const sftpServerPath = "/usr/lib/ssh/sftp-server"

func main() {
	supervise := flag.Bool("supervise", false, "run in background until SIGTERM")
	flag.Parse()

	if *supervise {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		fmt.Fprintln(os.Stderr, "devpod-sidecar: supervise mode; waiting for SIGTERM")
		<-ctx.Done()
		fmt.Fprintln(os.Stderr, "devpod-sidecar: terminating")
		return
	}

	args := flag.Args()
	// SFTP subsystem: sshd invokes this binary as `devpod-sidecar sftp`
	// per the Subsystem directive in sshd_config.tmpl.
	if len(args) > 0 && args[0] == "sftp" {
		if err := syscall.Exec(sftpServerPath, []string{"sftp-server"}, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "devpod-sidecar: exec sftp-server: %v\n", err)
			os.Exit(126)
		}
		return // unreachable on success
	}

	// ForceCommand path: SSH_ORIGINAL_COMMAND holds the client's command
	// (empty for an interactive shell).
	cmd := os.Getenv("SSH_ORIGINAL_COMMAND")
	if cmd == "" {
		if err := syscall.Exec("/bin/sh", []string{"-sh"}, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "devpod-sidecar: exec sh: %v\n", err)
			os.Exit(126)
		}
		return
	}
	if err := syscall.Exec("/bin/sh", []string{"sh", "-c", cmd}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "devpod-sidecar: exec sh -c %q: %v\n", strings.TrimSpace(cmd), err)
		os.Exit(126)
	}
}
```

- [ ] **Step 2: Build**

```bash
go build -o bin/devpod-sidecar ./cmd/sidecar
```

Expected: clean.

- [ ] **Step 3: Sanity-check on host**

```bash
SSH_ORIGINAL_COMMAND='echo hello from wrapper' ./bin/devpod-sidecar
```

Expected: prints `hello from wrapper`. (This relies on `/bin/sh` existing on the host, which it does on macOS and Linux.)

Note: `syscall.Exec` on macOS with `/bin/sh` works fine for this verification. The actual production binary is the linux/arm64 cross-build inside the sidecar image.

- [ ] **Step 4: Commit**

```bash
git add cmd/sidecar/main.go
git commit -m "cmd/sidecar: exec sh / sftp-server based on SSH_ORIGINAL_COMMAND"
```

---

### Task 6: Gateway auth — User-pubkey verification + DevPod ACL

**Files:**
- Create: `internal/gateway/auth.go`
- Create: `internal/gateway/auth_test.go`

The authenticator takes:
- A `ssh.PublicKey` (the client's offered key)
- The SSH login user string (e.g., `alice+frontend-dev`)
- A read-only K8s `client.Reader` (informer cache backs this)

It returns the (validated) authenticated User name, the target DevPod object, and any error. The caller (the SSH server's `PublicKeyCallback`) uses this to populate `ssh.Permissions.Extensions` so downstream code can find the target.

- [ ] **Step 1: Write the failing test `internal/gateway/auth_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

// fakeClient builds a controller-runtime fake client preloaded with the
// passed objects and the DevPod scheme.
func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// ed25519Pubkey returns a fresh public key + its authorized_keys-line form.
func ed25519Pubkey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	signer, err := ssh.NewSignerFromKey(generateEd25519(t))
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey(), string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func TestAuthenticate_OwnerSucceeds(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		&devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "devpods"},
			Spec: devpodv1alpha1.DevPodSpec{
				Owner: "alice",
				Pod: &devpodv1alpha1.PodWorkloadSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
				},
			},
			Status: devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning, Endpoint: "10.0.0.1:22"},
		},
	)

	a := gateway.NewAuthenticator(c, "devpods")
	res, err := a.Authenticate(context.Background(), "alice+hello", pk)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.User != "alice" || res.DevPodName != "hello" || res.Endpoint != "10.0.0.1:22" {
		t.Errorf("got %+v", res)
	}
}

func TestAuthenticate_CollaboratorSucceeds(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "bob"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		&devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "devpods"},
			Spec: devpodv1alpha1.DevPodSpec{
				Owner:         "alice",
				Collaborators: []string{"bob"},
				Pod:           &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}},
			},
			Status: devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning, Endpoint: "10.0.0.1:22"},
		},
	)

	res, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "bob+hello", pk)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.User != "bob" || res.DevPodName != "hello" {
		t.Errorf("got %+v", res)
	}
}

func TestAuthenticate_NotOwnerNotCollab_Denied(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "carol"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		&devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "devpods"},
			Spec: devpodv1alpha1.DevPodSpec{
				Owner: "alice",
				Pod:   &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}},
			},
			Status: devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning, Endpoint: "10.0.0.1:22"},
		},
	)

	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "carol+hello", pk)
	if !errors.Is(err, gateway.ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
}

func TestAuthenticate_UnknownUser_Denied(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	c := fakeClient(t)
	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "nobody+hello", pk)
	if !errors.Is(err, gateway.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestAuthenticate_PubkeyMismatch_Denied(t *testing.T) {
	_, line := ed25519Pubkey(t) // store this one
	other, _ := ed25519Pubkey(t) // present this one
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{line}},
		},
	)
	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "alice+hello", other)
	if !errors.Is(err, gateway.ErrPubkeyMismatch) {
		t.Fatalf("err = %v, want ErrPubkeyMismatch", err)
	}
}

func TestAuthenticate_DevPodNotRunning_Denied(t *testing.T) {
	pk, pkLine := ed25519Pubkey(t)
	c := fakeClient(t,
		&devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pkLine}},
		},
		&devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "devpods"},
			Spec: devpodv1alpha1.DevPodSpec{
				Owner: "alice",
				Pod:   &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}},
			},
			Status: devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodPending},
		},
	)
	_, err := gateway.NewAuthenticator(c, "devpods").Authenticate(context.Background(), "alice+hello", pk)
	if !errors.Is(err, gateway.ErrDevPodNotReady) {
		t.Fatalf("err = %v, want ErrDevPodNotReady", err)
	}
}
```

Add the helper `generateEd25519` at the end of the test file:

```go
// generateEd25519 is a tiny helper local to gateway_test for the auth tests.
func generateEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}
```

Add the corresponding imports: `crypto/ed25519` and `crypto/rand`.

- [ ] **Step 2: Run, expect FAIL**

```bash
go test ./internal/gateway/... -run TestAuthenticate -v
```

Expected: FAIL at compile — `gateway.NewAuthenticator` undefined.

- [ ] **Step 3: Create `internal/gateway/auth.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Sentinel errors returned by Authenticate. Callers can use errors.Is.
var (
	ErrUserNotFound    = errors.New("user not found")
	ErrPubkeyMismatch  = errors.New("pubkey does not match any User entry")
	ErrDevPodNotFound  = errors.New("devpod not found")
	ErrDevPodNotReady  = errors.New("devpod not running")
	ErrAccessDenied    = errors.New("access denied: not owner or collaborator")
	ErrLoginNameFormat = errors.New("login name must be <user>+<pod>")
)

// AuthResult is what Authenticate returns on success — enough for the
// proxy layer to dial the right backend and tag the connection.
type AuthResult struct {
	User       string // authenticated User CRD name
	DevPodName string // target DevPod CRD name
	Endpoint   string // DevPod.status.endpoint, "<ip>:<port>"
}

// Authenticator validates SSH client public keys against the User CRD
// and resolves the target DevPod.
//
// It uses a client.Reader (typically the controller-runtime cache);
// callers MUST ensure the underlying cache is started and synced for
// User and DevPod kinds before calls.
type Authenticator struct {
	c      client.Reader
	dpNS   string
}

// NewAuthenticator returns an Authenticator that reads from c and
// looks up DevPods in the given namespace.
func NewAuthenticator(c client.Reader, devpodNamespace string) *Authenticator {
	return &Authenticator{c: c, dpNS: devpodNamespace}
}

// Authenticate parses the SSH login name as "<user>+<pod>", verifies
// that key matches some pubkey in User/<user>.spec.pubkeys, checks the
// target DevPod is running, and verifies <user> is the owner or in
// spec.collaborators.
func (a *Authenticator) Authenticate(ctx context.Context, loginName string, key ssh.PublicKey) (*AuthResult, error) {
	user, pod, err := ParseLoginName(loginName)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoginNameFormat, err)
	}

	// Look up the User and find a matching pubkey.
	var u devpodv1alpha1.User
	if err := a.c.Get(ctx, types.NamespacedName{Name: user}, &u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q", ErrUserNotFound, user)
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	if !matchesAny(key, u.Spec.Pubkeys) {
		return nil, fmt.Errorf("%w: user %q", ErrPubkeyMismatch, user)
	}

	// Look up the DevPod and verify access.
	var dp devpodv1alpha1.DevPod
	if err := a.c.Get(ctx, types.NamespacedName{Name: pod, Namespace: a.dpNS}, &dp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q in ns %q", ErrDevPodNotFound, pod, a.dpNS)
		}
		return nil, fmt.Errorf("get devpod: %w", err)
	}
	if !accessAllowed(&dp, user) {
		return nil, fmt.Errorf("%w: user %q on devpod %q", ErrAccessDenied, user, pod)
	}
	if dp.Status.Phase != devpodv1alpha1.DevPodRunning || dp.Status.Endpoint == "" {
		return nil, fmt.Errorf("%w: %q phase=%q endpoint=%q", ErrDevPodNotReady, pod, dp.Status.Phase, dp.Status.Endpoint)
	}

	return &AuthResult{
		User:       user,
		DevPodName: pod,
		Endpoint:   dp.Status.Endpoint,
	}, nil
}

// accessAllowed returns true if user is the owner or appears in
// spec.collaborators.
func accessAllowed(dp *devpodv1alpha1.DevPod, user string) bool {
	if dp.Spec.Owner == user {
		return true
	}
	for _, c := range dp.Spec.Collaborators {
		if c == user {
			return true
		}
	}
	return false
}

// matchesAny returns true if key's marshaled bytes equal any of the
// authorized_keys-line pubkeys in lines.
func matchesAny(key ssh.PublicKey, lines []string) bool {
	want := key.Marshal()
	for _, line := range lines {
		parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		if bytes.Equal(parsed.Marshal(), want) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Pull controller-runtime fake-client dep**

```bash
go mod tidy
```

- [ ] **Step 5: Run, expect PASS**

```bash
go test ./internal/gateway/... -v 2>&1 | tail -20
```

Expected: 6 PASS for the new auth tests + the existing login-name tests.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/auth.go internal/gateway/auth_test.go go.mod go.sum
git commit -m "internal/gateway: Authenticator (User pubkey + owner/collaborator ACL)"
```

---

### Task 7: Gateway dialer — connect to backend sshd with internal key

**Files:**
- Create: `internal/gateway/dialer.go`
- Create: `internal/gateway/dialer_test.go`

`Dialer` opens a backend SSH connection to a given address using the gateway's internal private key. The login user is hard-coded to `devpod` (the sidecar's sshd ignores the username; only the pubkey matters).

- [ ] **Step 1: Write the failing test `internal/gateway/dialer_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"crypto/ed25519"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

// fakeBackend starts an in-process ssh server that accepts only the given
// gateway pubkey. Returns the listener address and a cancel func.
func fakeBackend(t *testing.T, allowedPub ssh.PublicKey) (string, func()) {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(allowedPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _, _, _ = ssh.NewServerConn(c, cfg)
				// no-op: we only care about the handshake succeeding
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestDialer_HappyPath(t *testing.T) {
	_, gwPriv, _ := ed25519.GenerateKey(nil)
	gwSigner, _ := ssh.NewSignerFromKey(gwPriv)

	addr, stop := fakeBackend(t, gwSigner.PublicKey())
	defer stop()

	d := gateway.NewDialer(gwSigner)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, chans, reqs, err := d.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	go func() {
		for ch := range chans {
			_ = ch.Reject(ssh.Prohibited, "test")
		}
	}()
}

func TestDialer_TimeoutOnNoListener(t *testing.T) {
	_, gwPriv, _ := ed25519.GenerateKey(nil)
	gwSigner, _ := ssh.NewSignerFromKey(gwPriv)

	d := gateway.NewDialer(gwSigner)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// 127.0.0.1 with an unused port: should fail fast (TCP RST).
	_, _, _, err := d.Dial(ctx, "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error dialing dead port")
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
go test ./internal/gateway/... -run TestDialer -v
```

Expected: FAIL — `gateway.NewDialer` undefined.

- [ ] **Step 3: Create `internal/gateway/dialer.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// DialTimeout is the default backend-dial timeout.
const DialTimeout = 5 * time.Second

// Dialer opens SSH connections to backend (per-DevPod) sshd instances.
type Dialer struct {
	signer ssh.Signer
}

// NewDialer returns a Dialer that authenticates to the backend using
// the given gateway internal signing key.
func NewDialer(internalKey ssh.Signer) *Dialer {
	return &Dialer{signer: internalKey}
}

// Dial opens a TCP connection to addr and completes an SSH client
// handshake. The returned channels and request channel must be drained
// or routed by the caller (typically the proxy loop). The host key is
// not verified — the backend is reached over a cluster-internal Pod IP
// which is itself the trust anchor.
func (d *Dialer) Dial(ctx context.Context, addr string) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	dialer := &net.Dialer{Timeout: DialTimeout}
	tcp, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	cfg := &ssh.ClientConfig{
		User:            "devpod",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(d.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         DialTimeout,
		ClientVersion:   "SSH-2.0-devpod-gateway",
	}
	conn, chans, reqs, err := ssh.NewClientConn(tcp, addr, cfg)
	if err != nil {
		_ = tcp.Close()
		return nil, nil, nil, fmt.Errorf("ssh handshake to %s: %w", addr, err)
	}
	return conn, chans, reqs, nil
}
```

- [ ] **Step 4: Run, expect PASS**

```bash
go test ./internal/gateway/... -run TestDialer -v
```

Expected: 2 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/dialer.go internal/gateway/dialer_test.go
git commit -m "internal/gateway: Dialer — backend SSH client connection with internal key"
```

---

### Task 8: Gateway proxy — channels + requests pump

**Files:**
- Create: `internal/gateway/proxy.go`
- Create: `internal/gateway/proxy_test.go`

This is the heart of the proxy, ported from OpenNG `proxy.go`'s `HandleSSH` + `HandleChannel`. Two functions:

- `Proxy(ctx, serverConn, serverChans, serverReqs, clientConn, clientChans, clientReqs)` — the top-level pump.
- `proxyChannel(newCh, remote)` — per-channel pump (accept, OpenChannel on remote, pump data + requests).

- [ ] **Step 1: Write the failing test `internal/gateway/proxy_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"crypto/ed25519"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

// echoBackend runs a tiny sshd that accepts the given client pubkey,
// expects a single "session" channel, and echoes everything the client
// writes back. Returns the listen address + cleanup.
func echoBackend(t *testing.T, allowedPub ssh.PublicKey) (string, func()) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(nil)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(allowedPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			tc, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					ch, in, err := newCh.Accept()
					if err != nil {
						return
					}
					go func() {
						for r := range in {
							_ = r.Reply(true, nil)
						}
					}()
					// echo
					_, _ = io.Copy(ch, ch)
					_ = ch.Close()
				}
			}(tc)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestProxy_EchoEndToEnd(t *testing.T) {
	_, gwPriv, _ := ed25519.GenerateKey(nil)
	gwSigner, _ := ssh.NewSignerFromKey(gwPriv)

	backendAddr, stopBackend := echoBackend(t, gwSigner.PublicKey())
	defer stopBackend()

	// Bring up a "gateway" listener that accepts any client pubkey
	// (test scope) and uses Proxy to bridge to the echo backend.
	_, frontHostPriv, _ := ed25519.GenerateKey(nil)
	frontHostSigner, _ := ssh.NewSignerFromKey(frontHostPriv)

	frontCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	frontCfg.AddHostKey(frontHostSigner)

	frontLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer frontLn.Close()

	dialer := gateway.NewDialer(gwSigner)

	go func() {
		for {
			tc, err := frontLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				srv, srvChans, srvReqs, err := ssh.NewServerConn(conn, frontCfg)
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cli, cliChans, cliReqs, err := dialer.Dial(ctx, backendAddr)
				if err != nil {
					srv.Close()
					return
				}
				_ = gateway.Proxy(srv, srvChans, srvReqs, cli, cliChans, cliReqs)
			}(tc)
		}
	}()

	// Now act as the client: connect to frontLn, open a session, write
	// "hello", read it back.
	_, clientPriv, _ := ed25519.GenerateKey(nil)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)

	tc, err := net.Dial("tcp", frontLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(tc, frontLn.Addr().String(), &ssh.ClientConfig{
		User:            "alice+hello",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cli := ssh.NewClient(conn, chans, reqs)
	defer cli.Close()

	session, err := cli.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()
	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()
	if err := session.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}

	_, _ = stdin.Write([]byte("hello\n"))
	_ = stdin.Close()

	buf := make([]byte, 6)
	if _, err := io.ReadFull(stdout, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "hello\n" {
		t.Errorf("got %q, want %q", string(buf), "hello\n")
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

```bash
go test ./internal/gateway/... -run TestProxy -v
```

Expected: FAIL — `gateway.Proxy` undefined.

- [ ] **Step 3: Create `internal/gateway/proxy.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Proxy bridges a client SSH connection (server-side from our POV) and
// a backend SSH connection (client-side from our POV). It runs until
// either side closes, then returns the close reason from the client
// side.
//
// The shape is a straight port of OpenNG/ngssh/proxy.go's HandleSSH:
// four goroutines fan channels and global requests in both directions,
// and proxyChannel handles per-channel data and channel requests.
func Proxy(
	clientConn ssh.Conn,
	clientChans <-chan ssh.NewChannel,
	clientReqs <-chan *ssh.Request,
	backendConn ssh.Conn,
	backendChans <-chan ssh.NewChannel,
	backendReqs <-chan *ssh.Request,
) error {
	var wg sync.WaitGroup

	// Client → backend channel opens.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for nc := range clientChans {
			go proxyChannel(nc, backendConn)
		}
	}()

	// Backend → client channel opens (e.g., forwarded-tcpip).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for nc := range backendChans {
			go proxyChannel(nc, clientConn)
		}
	}()

	// Client → backend global requests.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for req := range clientReqs {
			ok, payload, _ := backendConn.SendRequest(req.Type, req.WantReply, req.Payload)
			_ = req.Reply(ok, payload)
		}
	}()

	// Backend → client global requests.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for req := range backendReqs {
			if req.Type == "hostkeys-00@openssh.com" {
				// OpenSSH-specific extension; not safe to forward.
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			ok, payload, _ := clientConn.SendRequest(req.Type, req.WantReply, req.Payload)
			_ = req.Reply(ok, payload)
		}
	}()

	// Wait for either side to close.
	closeErr := make(chan error, 2)
	go func() { closeErr <- clientConn.Wait() }()
	go func() { closeErr <- backendConn.Wait() }()

	err := <-closeErr
	_ = clientConn.Close()
	_ = backendConn.Close()

	// Drain the goroutines so we don't leak them.
	wg.Wait()

	if err == io.EOF {
		return nil
	}
	return err
}

// proxyChannel handles a single channel: opens the corresponding channel
// on remote, accepts the local channel, and pumps stdin/stdout/stderr +
// channel requests in both directions.
func proxyChannel(local ssh.NewChannel, remote ssh.Conn) {
	rch, rreqs, err := remote.OpenChannel(local.ChannelType(), local.ExtraData())
	if err != nil {
		var ocErr *ssh.OpenChannelError
		switch e := err.(type) {
		case *ssh.OpenChannelError:
			ocErr = e
			_ = local.Reject(e.Reason, e.Message)
		default:
			_ = local.Reject(ssh.ConnectionFailed, fmt.Sprintf("open channel: %v", err))
		}
		_ = ocErr // silence unused-on-non-OCE path
		return
	}

	lch, lreqs, err := local.Accept()
	if err != nil {
		_ = rch.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(4)

	// stdout: local <- remote
	go func() {
		defer wg.Done()
		_, _ = io.Copy(lch, rch)
		_ = lch.CloseWrite()
	}()
	// stdin: local -> remote
	go func() {
		defer wg.Done()
		_, _ = io.Copy(rch, lch)
		_ = rch.CloseWrite()
	}()
	// stderr: local <- remote
	go func() {
		defer wg.Done()
		_, _ = io.Copy(lch.Stderr(), rch.Stderr())
	}()
	// channel requests: forward in both directions.
	go func() {
		defer wg.Done()
		var rwg sync.WaitGroup
		rwg.Add(2)
		go func() {
			defer rwg.Done()
			for r := range lreqs {
				ok, _ := rch.SendRequest(r.Type, r.WantReply, r.Payload)
				_ = r.Reply(ok, nil)
			}
		}()
		go func() {
			defer rwg.Done()
			for r := range rreqs {
				ok, _ := lch.SendRequest(r.Type, r.WantReply, r.Payload)
				_ = r.Reply(ok, nil)
			}
		}()
		rwg.Wait()
	}()

	wg.Wait()
	_ = lch.Close()
	_ = rch.Close()
}
```

- [ ] **Step 4: Run, expect PASS**

```bash
go test ./internal/gateway/... -run TestProxy -v
```

Expected: 1 PASS. (Test combines auth + dial + proxy; takes ~200ms.)

- [ ] **Step 5: Run all gateway tests**

```bash
go test ./internal/gateway/... -v 2>&1 | tail -10
```

Expected: all PASS (login + auth + dialer + proxy).

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/proxy.go internal/gateway/proxy_test.go
git commit -m "internal/gateway: Proxy — bidirectional SSH channel/request pump (ported from OpenNG)"
```

---

### Task 9: `cmd/gateway/main.go` — wire informer cache + new server

**Files:**
- Rewrite: `cmd/gateway/main.go`

The skeleton main needs to grow into a real gateway: build an informer-cached client (controller-runtime), read flags, load the host key + internal key, construct Authenticator + Dialer, and run a loop that for each TCP accept does:

1. Speak SSH server.
2. In `PublicKeyCallback`, call the Authenticator. On success, stash `AuthResult` in `ssh.Permissions.Extensions` so we don't re-fetch it.
3. After `ssh.NewServerConn` succeeds, dial backend + `Proxy(...)`.

- [ ] **Step 1: Rewrite `cmd/gateway/main.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-gateway is the DevPod SSH gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		listenAddr      string
		hostKeyDir      string
		devpodNamespace string
	)
	flag.StringVar(&listenAddr, "listen", ":22", "TCP address to listen on")
	flag.StringVar(&hostKeyDir, "host-key-dir", "/etc/devpod/gateway",
		"directory containing ssh_host_ed25519_key (the gateway host key) and internal_key (the gateway's outbound signing key)")
	flag.StringVar(&devpodNamespace, "devpod-namespace", "devpods",
		"namespace where DevPod objects live")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	hostSigner, internalSigner, err := loadKeys(hostKeyDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: load kubeconfig: %v\n", err)
		os.Exit(1)
	}

	c, err := newCachedClient(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}

	authn := gateway.NewAuthenticator(c, devpodNamespace)
	dialer := gateway.NewDialer(internalSigner)

	if err := run(ctx, listenAddr, hostSigner, authn, dialer); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

func loadKeys(dir string) (host ssh.Signer, internal ssh.Signer, err error) {
	hostBytes, err := os.ReadFile(filepath.Join(dir, "ssh_host_ed25519_key"))
	if err != nil {
		return nil, nil, fmt.Errorf("read host key: %w", err)
	}
	host, err = ssh.ParsePrivateKey(hostBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse host key: %w", err)
	}

	// The internal-key Secret is mounted at the same dir under a separate
	// key. We accept either layout: a "ssh_host_ed25519_key" file in a
	// nested "internal" dir (common when a single mountPath is reused for
	// two Secrets via separate volumes) or a top-level "internal_key".
	candidates := []string{
		filepath.Join(dir, "internal", "ssh_host_ed25519_key"),
		filepath.Join(dir, "internal_key"),
	}
	var intBytes []byte
	for _, p := range candidates {
		intBytes, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read internal key (looked in %v): %w", candidates, err)
	}
	internal, err = ssh.ParsePrivateKey(intBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse internal key: %w", err)
	}
	return host, internal, nil
}

// newCachedClient builds an informer-backed client.Reader over User and
// DevPod kinds, starts the cache, and waits for the initial list to sync.
func newCachedClient(ctx context.Context, cfg *ctrl.Config) (client.Reader, error) {
	cch, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create cache: %w", err)
	}

	// Touch each kind so the informer starts a watch.
	if _, err := cch.GetInformer(ctx, &devpodv1alpha1.User{}); err != nil {
		return nil, fmt.Errorf("informer User: %w", err)
	}
	if _, err := cch.GetInformer(ctx, &devpodv1alpha1.DevPod{}); err != nil {
		return nil, fmt.Errorf("informer DevPod: %w", err)
	}

	go func() { _ = cch.Start(ctx) }()
	if !cch.WaitForCacheSync(ctx) {
		return nil, errors.New("cache failed to sync")
	}
	return cch, nil
}

func run(ctx context.Context, addr string, hostSigner ssh.Signer, authn *gateway.Authenticator, dialer *gateway.Dialer) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()

	fmt.Fprintf(os.Stderr, "devpod-gateway listening on %s\n", addr)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	g, _ := errgroup.WithContext(ctx)
	var connCount atomic.Uint64
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return g.Wait()
			}
			return fmt.Errorf("accept: %w", err)
		}
		id := connCount.Add(1)
		fmt.Fprintf(os.Stderr, "accept: id=%d from=%s\n", id, conn.RemoteAddr())
		g.Go(func() error {
			handle(ctx, id, conn, hostSigner, authn, dialer)
			return nil
		})
	}
}

func handle(parent context.Context, id uint64, conn net.Conn, hostSigner ssh.Signer, authn *gateway.Authenticator, dialer *gateway.Dialer) {
	defer conn.Close()

	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-devpod-gateway",
		BannerCallback: func(_ ssh.ConnMetadata) string {
			return "devpod-gateway\n"
		},
	}
	cfg.AddHostKey(hostSigner)

	cfg.PublicKeyCallback = func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		res, err := authn.Authenticate(ctx, meta.User(), key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "auth-rejected: id=%d login=%s reason=%v\n", id, meta.User(), err)
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "auth-ok: id=%d user=%s pod=%s endpoint=%s\n", id, res.User, res.DevPodName, res.Endpoint)
		return &ssh.Permissions{
			Extensions: map[string]string{
				"devpod.io/user":     res.User,
				"devpod.io/devpod":   res.DevPodName,
				"devpod.io/endpoint": res.Endpoint,
			},
		}, nil
	}

	srvConn, srvChans, srvReqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer srvConn.Close()

	endpoint := srvConn.Permissions.Extensions["devpod.io/endpoint"]
	dctx, dcancel := context.WithTimeout(parent, gateway.DialTimeout)
	cliConn, cliChans, cliReqs, err := dialer.Dial(dctx, endpoint)
	dcancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial-failed: id=%d endpoint=%s err=%v\n", id, endpoint, err)
		return
	}
	defer cliConn.Close()

	fmt.Fprintf(os.Stderr, "proxy-start: id=%d\n", id)
	if err := gateway.Proxy(srvConn, srvChans, srvReqs, cliConn, cliChans, cliReqs); err != nil {
		fmt.Fprintf(os.Stderr, "proxy-end: id=%d err=%v\n", id, err)
	} else {
		fmt.Fprintf(os.Stderr, "proxy-end: id=%d\n", id)
	}
}
```

- [ ] **Step 2: Tidy + build**

```bash
go mod tidy
go build -o bin/devpod-gateway ./cmd/gateway
```

Expected: clean.

- [ ] **Step 3: Smoke run (still fails without a real kubeconfig, but should print a clearer error than before)**

```bash
KUBECONFIG=/nonexistent ./bin/devpod-gateway --listen 127.0.0.1:0 --host-key-dir /tmp 2>&1 | head -3 || true
```

Expected: error mentioning kubeconfig or host-key dir; no panic.

- [ ] **Step 4: Commit**

```bash
git add cmd/gateway/main.go go.mod go.sum
git commit -m "cmd/gateway: real proxy — informer-cached auth + Dialer + Proxy"
```

---

### Task 10: e2e — `ssh alice+hello@gateway uname -a` returns alpine info

**Files:**
- Create: `test/e2e/smoke_proxy_test.go`
- Modify: `hack/e2e-up.sh` (mount the internal key into the gateway pod via Helm `--set`)

This task builds on the existing kind smoke. After the chart is up:
1. Generate a separate `internal_key` and create the `devpod-gateway-internal-key` Secret (already referenced by GatewayConfig).
2. Mount it into the gateway Deployment under `/etc/devpod/gateway/internal/`.
3. Create `User/alice` with a real pubkey.
4. Create `DevPod/hello` owned by alice.
5. Wait for `status.endpoint`.
6. `ssh -p <forwarded-port> alice+hello@127.0.0.1 uname -a` → returns `Linux alice-hello ...`.

- [ ] **Step 1: Update `hack/e2e-up.sh`**

Append a section after the helm install that:

a) Creates both gateway Secrets if missing.

b) Patches the gateway Deployment to add a second mount for the internal key.

```bash
# Create per-deploy SSH keys if not already present.
if ! kubectl -n devpod-system get secret devpod-gateway-host-key >/dev/null 2>&1; then
  TMP=$(mktemp -d)
  ssh-keygen -t ed25519 -N '' -f "$TMP/host" -q
  kubectl -n devpod-system create secret generic devpod-gateway-host-key \
    --from-file=ssh_host_ed25519_key="$TMP/host" \
    --from-file=ssh_host_ed25519_key.pub="$TMP/host.pub"
  /bin/rm -rf "$TMP"
fi
if ! kubectl -n devpod-system get secret devpod-gateway-internal-key >/dev/null 2>&1; then
  TMP=$(mktemp -d)
  ssh-keygen -t ed25519 -N '' -f "$TMP/internal" -q
  kubectl -n devpod-system create secret generic devpod-gateway-internal-key \
    --from-file=ssh_host_ed25519_key="$TMP/internal" \
    --from-file=ssh_host_ed25519_key.pub="$TMP/internal.pub"
  /bin/rm -rf "$TMP"
fi

# Restart controller + gateway to pick up new secrets / config.
kubectl -n devpod-system rollout restart deploy/devpod-controller deploy/devpod-gateway
kubectl -n devpod-system rollout status deploy/devpod-controller --timeout=120s
kubectl -n devpod-system rollout status deploy/devpod-gateway    --timeout=120s
```

- [ ] **Step 2: Mount the internal key in the chart**

In `deploy/chart/templates/gateway.yaml`, extend the volumes and volumeMounts:

```yaml
        volumeMounts:
        - name: host-key
          mountPath: /etc/devpod/gateway
          readOnly: true
        - name: internal-key
          mountPath: /etc/devpod/gateway/internal
          readOnly: true
      volumes:
      - name: host-key
        secret:
          secretName: devpod-gateway-host-key
      - name: internal-key
        secret:
          secretName: devpod-gateway-internal-key
```

Also mount the internal-key into the controller so it can read the pubkey at startup:

```yaml
# in deploy/chart/templates/controller.yaml controller container:
        volumeMounts:
        - name: internal-key-pub
          mountPath: /etc/devpod/internal
          readOnly: true
      volumes:
      - name: internal-key-pub
        secret:
          secretName: devpod-gateway-internal-key
          items:
          - key: ssh_host_ed25519_key.pub
            path: ssh_host_ed25519_key.pub
```

And add a flag to the controller for the internal-pubkey path:

```yaml
        args:
        - --devpod-namespace={{ .Values.namespaces.devpods }}
        - --gateway-namespace={{ .Values.namespaces.system }}
        - --sidecar-image={{ .Values.image.sidecar.repository }}:{{ .Values.image.sidecar.tag }}
        - --leader-elect=true
        - --zap-devel=false
        - --internal-pubkey-file=/etc/devpod/internal/ssh_host_ed25519_key.pub
```

- [ ] **Step 3: Update `cmd/controller/main.go` to read the pubkey**

Add a `--internal-pubkey-file` flag. At startup, read its contents (or print a warning and continue with empty if the file is missing — useful for envtest). Pass the bytes to the DevPodReconciler:

```go
	flag.StringVar(&internalPubFile, "internal-pubkey-file", "", "path to the gateway internal public key (authorized_keys line); when set, embedded into per-DevPod host-key Secrets")
	// ...
	var internalPub []byte
	if internalPubFile != "" {
		b, err := os.ReadFile(internalPubFile)
		if err != nil {
			die(err, "read internal pubkey")
		}
		internalPub = b
	}
	// ...
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		GwConfig:           gw,
		GatewayNamespace:   gatewayNamespace,
		GatewayInternalPub: internalPub,
```

- [ ] **Step 4: Write `test/e2e/smoke_proxy_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestSmoke_SSHProxy_ReachesSidecar(t *testing.T) {
	// Bring up the cluster + install chart (idempotent).
	repo := repoRoot()
	cmd := exec.Command("bash", "hack/e2e-up.sh")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("e2e-up.sh: %v\n%s", err, out)
	}

	scheme := apiruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Generate a client key + register it as alice's pubkey.
	keyDir := t.TempDir()
	priv := filepath.Join(keyDir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-q").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pubBytes, err := os.ReadFile(priv + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{strings.TrimSpace(string(pubBytes))}},
	}
	_ = c.Delete(ctx, user)
	if err := c.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), user) })

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "alice",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "dev",
						Image:   "alpine:3.20",
						Command: []string{"sh", "-c", "sleep infinity"},
					}},
				},
			},
		},
	}
	_ = c.Delete(ctx, dp)
	if err := c.Create(ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), dp) })

	// Wait for status.endpoint to be populated.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if err := c.Get(ctx, client.ObjectKey{Name: "smoke", Namespace: "devpods"}, &got); err == nil && got.Status.Endpoint != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Start a port-forward to the gateway.
	pf := exec.Command("kubectl", "-n", "devpod-system", "port-forward", "svc/devpod-gateway", "12222:22")
	pf.Stdout = os.Stderr
	pf.Stderr = os.Stderr
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(2 * time.Second)

	// Try the actual SSH proxy.
	deadline = time.Now().Add(60 * time.Second)
	var out []byte
	for time.Now().Before(deadline) {
		sshCmd := exec.Command("ssh",
			"-i", priv,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "BatchMode=yes",
			"-p", "12222",
			"alice+smoke@127.0.0.1",
			"uname", "-a",
		)
		out, err = sshCmd.CombinedOutput()
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatalf("ssh: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "Linux") {
		t.Fatalf("expected Linux in output; got:\n%s", out)
	}
	t.Logf("proxy works! ssh output:\n%s", out)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}
```

- [ ] **Step 5: Run the e2e**

```bash
go test -tags e2e ./test/e2e/... -run TestSmoke_SSHProxy -v -count=1 -timeout=20m
```

Expected: PASS. The `ssh` output should be the alpine `uname -a`.

- [ ] **Step 6: Confirm unit tests still pass**

```bash
bash hack/test.sh 2>&1 | tail -5
```

Expected: all unit tests pass; coverage may bump a bit.

- [ ] **Step 7: Commit**

```bash
git add test/e2e/ hack/e2e-up.sh deploy/chart/templates/gateway.yaml deploy/chart/templates/controller.yaml cmd/controller/main.go
git commit -m "e2e: ssh proxy reaches sidecar via real backend dial"
```

---

## Self-review (run after Task 10 lands)

1. **Spec coverage:** §4.4 (direct client) — implemented. §4.3 (sidecar internals) — sshd config unchanged; ForceCommand wraps to `devpod-sidecar`. §3.4 (controller writes status.endpoint) — done. §5 (security): the sidecar's authorized_keys is single-key gateway-internal-only ✓. Cross-tenant: covered by Authenticator's owner/collaborator check ✓.

2. **Placeholders:** none remaining. Verify with `grep -n "TODO\|FIXME\|TBD" $(find . -name '*.go' -not -path './bin/*' -not -path './.git/*')`. Pre-existing TODO(M2) is fine.

3. **Type / name consistency:** `AuthResult`, `Authenticator`, `Dialer`, `Proxy` — all in `internal/gateway`. `GatewayInternalPub` field on the reconciler — referenced from `suite_test.go` and `cmd/controller/main.go`. ✓

4. **Test coverage:** auth (6 cases), dialer (2 cases), proxy (1 echo case), controller status write (1 case), e2e (1 case).

5. **FOLLOWUPS** (add after the plan completes):
   - Real nsenter into user container (next plan, M2).
   - `trustedProxyKeys` bypass path.
   - `lastActivityTime` patching + idle hibernate.
   - Wire ValidatingWebhookConfiguration so the webhook handler is actually served.
   - Migrate the webhook to `admission.CustomValidator` shape.
   - Per-DevPod sidecar SSH server-host-key rotation policy.

---

## Execution choice

Plan complete. Two execution options:

**1. Subagent-Driven (recommended)** — fresh subagent per task, opus model, review between tasks.

**2. Inline Execution** — same session, batched.

Which approach?
