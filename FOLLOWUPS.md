# Follow-ups deferred during M0+M1-skeleton implementation

Issues raised during code review of the initial milestones that are not
blockers but should be addressed before the API stabilizes (v1alpha1 →
v1beta1).

## API design

- **`PersistenceSpec.Size string` → `resource.Quantity`** (`api/v1alpha1/devpod_types.go`).
  Today a malformed quantity like `"foo"` passes admission and only fails
  at PVC creation. Either switch to `resource.Quantity` (controller-gen
  emits the right `anyOf: [int, string]` + pattern) or add
  `+kubebuilder:validation:Pattern=` matching K8s quantity syntax.

- **`WorkloadRef.Kind`** should be an enum
  (`+kubebuilder:validation:Enum=Pod;VirtualMachine`) once the controller
  knows what it writes there.

- **`User.spec.oidcSubject`** is reserved for future OIDC binding. Before
  v1, decide whether a single string is enough or whether a nested
  `oidcConfig` struct is needed.

- **`GatewayConfigStatus.{ReadyReplicas, Conditions}`** lack Go doc
  comments → `kubectl explain` shows no description. Add comments.

- **`SecretRef.{Name, Namespace}`** lack `MinLength=1` and DNS-1123 pattern
  validation. Empty strings currently pass admission.

- **`TrustedProxyKey.Pubkey`** has no format validation. Either add an
  OpenSSH-pubkey regex or document the format in the Go comment.

- **`Listen.Port` field type `int32` with `omitempty`** — `0` and "unset"
  are indistinguishable. Default + Minimum=1 prevent 0 from being valid,
  but using `*int32` would be more canonical for an explicitly-defaulted
  field. Same comment applies to `DefaultIdleTimeoutSeconds` where `0`
  *is* meaningful (= disabled).

- **Reusable refs**: `SecretRef`, `LocalObjectRef`, `WorkloadRef` are all
  in the same package without a `_refs.go` file. Fine today, refactor
  once a second consumer exists.

## CRD operational

- **`devpod.io_devpods.yaml` is 573 KB** because the full Kubernetes
  PodSpec schema is embedded. Confirm server-side-apply works on this
  size across Argo CD / Flux / `kubectl apply`. If size becomes a
  problem, fall back to
  `+kubebuilder:pruning:PreserveUnknownFields +kubebuilder:validation:Schemaless`
  on `PodWorkloadSpec.Spec` — the admission webhook (planned) will still
  catch sidecar-name collisions.

- **CRD YAMLs have no `# DO NOT EDIT` banner.** `controller-gen` doesn't
  emit one for CRDs. Consider adding via a build step or kustomization.

## Helm chart (Task 20 coordination)

- **Gateway Deployment binds privileged port 22** with no `securityContext`
  in the chart template. PodSecurity baseline/restricted will reject it.
  Either:
  1. Add `capabilities: add: [NET_BIND_SERVICE]` + `runAsNonRoot: true`
     + `runAsUser: <nonzero>` (cleanest).
  2. Run as `runAsUser: 0` (larger blast radius).
  3. Default `listen.port` to 2222 and have the Service map 22 → 2222
     (then the deployment is unprivileged for free).
  Recommend option 1 in the Task 20 implementation.

## Testing

- **No golden-file test** for "critical CRD invariants survive regen"
  (e.g., `spec.running.default == true`). Consider one if drift becomes
  a worry.

## Rendered-name invariants (from Task 7 review)

- **Name collision risk in `render.PodName`** (`internal/render/render.go`).
  Encoding is `<owner>-<dpName>`. When either side contains `-`, two
  distinct (owner, dpName) tuples can map to the same Pod name. Examples:
  - alice / frontend-dev ↔ alice-frontend / dev → both → `alice-frontend-dev`

  This is a cross-tenant ambiguity: the second create would fail with
  `AlreadyExists` against an object owned by a different tenant, leaking
  existence and giving a name-squatting DoS vector. Fix in the admission
  webhook (Task 14 or its successor): reject DevPod creation when
  `render.PodName(dp)` collides with any existing DevPod in the
  namespace. Cheap, no schema change, preserves `kubectl get pods`
  readability.

- **DevPod name length budget** (`api/v1alpha1/devpod_types.go`).
  Service names are DNS-1035 labels, max 63 chars. Hence
  `len(owner) + 1 + len(dp.Name) + len("-hostkey") ≤ 63`. With the User
  name regex `[a-z0-9-]{1,32}`, the safe upper bound on DevPod name is
  about 22 chars (32 + 1 + 22 + 8 = 63). Add
  `+kubebuilder:validation:MaxLength=22` to `DevPod.metadata.name` (or
  similar — pick the budget for the longest derived name across
  `PodName` / `ServiceName` / `HostKeySecretName` / `HomePVCName`).

- **Optional: DNS-validity property test** in
  `internal/render/render_test.go`. Feed each of `PodName`, `ServiceName`,
  `HostKeySecretName`, `HomePVCName`, `OwnerNetPolName` to
  `validation.IsDNS1123Subdomain` / `IsDNS1035Label` /
  `IsValidLabelValue` from `k8s.io/apimachinery/pkg/util/validation` and
  assert no errors across representative inputs. Catches a future
  length-budget regression at unit-test time.

## Pod rendering (from Task 8 review)

- **Broaden `TestRenderPod_DoesNotMutateUserContainer`** to snapshot the
  full `dp.Spec.Pod` (Volumes, ObjectMeta, ShareProcessNamespace) with
  `cmp.Diff` before/after `render.Pod`. Today only `Containers[0]` is
  asserted, so future regressions in volume/metadata mutation would
  slip through.

- **Reject reserved label/annotation keys in `spec.pod.metadata`** in
  the validating webhook (Task 14). The render layer silently no-ops
  user attempts to override `devpod.io/owner`, `devpod.io/devpod`,
  `app.kubernetes.io/managed-by`; better to fail loudly at admission.

- **Make sidecar resources configurable** via
  `GatewayConfig.Spec.SidecarResources` (currently hardcoded
  50m/64Mi request, 200m/128Mi limit). SFTP / `scp` of large trees can
  saturate 200m and 128Mi.

- **Webhook must enforce `len(spec.pod.spec.containers) >= 1`** so the
  render-layer access `Containers[0].Name` (used for
  `DEVPOD_TARGET_CONTAINER`) never panics.

- **Drop `SYS_PTRACE` from the sidecar** once the nsenter wrapper lands
  and is the only way the sidecar reaches the user container.
  `setns(2)` needs `CAP_SYS_ADMIN`; `CAP_SYS_PTRACE` is redundant once
  the wrapper enters the PID namespace.

- **Document or expose target-container selection.** Today
  `DEVPOD_TARGET_CONTAINER` is hardcoded to
  `spec.pod.spec.containers[0].Name`. Multi-container DevPods (e.g.,
  user + companion init/sidecar) need either explicit selection in
  `PodWorkloadSpec` (e.g., `targetContainer: <name>`) or clear docs.

## Host-key Secret (from Task 9 review)

- **Unexport `DefaultRand`** (`internal/render/secret.go`). Mutable
  exported package var is more API surface than needed; tests already
  inject `randSrc` directly. Rename to `defaultRand` (unexported) or
  inline `rand.Reader` in the nil fallback.

- **Test coverage gaps in `internal/render/secret_test.go`**:
  - No test for the `randSrc == nil → DefaultRand` fallback path.
  - No "two calls → different keys" assertion (pins post-simplification
    behavior and catches accidental reintroduction of a deterministic
    seed).
  - `TestHostKeySecret_KeyTypeMarker` doesn't go through
    `HostKeySecret`'s output — parse `sec.Data["ssh_host_ed25519_key.pub"]`
    and assert `parsed.Type() == ssh.KeyAlgoED25519` to actually pin the
    algorithm through the production function.
  - Round-trip the **private** key via `ssh.ParsePrivateKey(priv)` in
    addition to the public-key parse, as a stronger end-to-end check.

- **Strengthen the doc comment on `HostKeySecret`** with a "callers must
  not overwrite an existing Secret of the same name" caveat (the
  controller's `ensureHostKeySecret` is already a get-then-create-if-missing
  shape, so this is a documentation issue, not a code issue).

## NetworkPolicies (from Task 11 review)

- **DNS egress is broader than strictly necessary**
  (`internal/render/networkpolicy.go`). `NamespaceSelector: {}` matches
  all namespaces on UDP/TCP 53. Tightening to
  `{kubernetes.io/metadata.name: kube-system}` + `{k8s-app: kube-dns}`
  would be the principle-of-least-privilege choice — but breaks on
  clusters where CoreDNS lives elsewhere or NodeLocalDNSCache is used.
  Make it configurable on `GatewayConfig.Spec.DNSNamespace` /
  `DNSPodSelector` in a follow-up.

- **Egress to public internet via `0.0.0.0/0 except RFC1918` is hardcoded.**
  Spec §5.2 says egress to the public internet should be "configurable".
  Add `GatewayConfig.Spec.AllowPublicEgress: bool` (or a CIDR allow-list)
  and parameterize `OwnerAllowNetworkPolicy`. Also: the RFC1918 except
  list assumes the pod CIDR is RFC1918 — incorrect for some CNI setups
  (GKE Autopilot, Cilium with custom IPAM, EKS service CIDRs outside
  RFC1918). Surface `GatewayConfig.Spec.PodCIDR` / `ServiceCIDR` to make
  this precise.

- **Per-owner allow NetworkPolicy lifecycle** (Task 12/13 concern, not
  render). The policy must be deleted when the owner's last DevPod is
  removed (spec §5.2: "deleted with the owner's last DevPod"). Today the
  controller installs it lazily on first DevPod create; needs a
  finalizer or DevPod-delete reconcile path that counts remaining
  same-owner DevPods and unrenders this policy when zero.

- **Drift reconciliation** (Task 12 concern). The plan's `upsert` is
  `Create-or-empty-Patch`, so manual edits to NetworkPolicy / Service /
  Pod are never reverted. For shared, namespace-scoped objects
  (default-deny, per-owner allow), use server-side apply (SSA) or do a
  proper diff-based update so the controller actually owns the spec.

## DevPod controller (from Task 12 review)

- **Synchronize `mgrCancel` with `envTestEnv.Stop()`** in
  `internal/controllers/suite_test.go`. Today the manager goroutine
  outlives the test briefly (cancel returns immediately; Stop SIGKILLs
  apiserver while the manager unwinds). Wrap with
  `done := make(chan struct{}); go func() { defer close(done); _ = mgr.Start(mgrCtx) }(); …; <-done`
  before calling Stop.

- **`setupSuite` writes package-level globals** (`envTestEnv`,
  `k8sClient`, `scheme`). Tests must remain serial — adding
  `t.Parallel()` to any controller test will race. Either document the
  constraint or move state onto `testEnv`.

- **Per-owner allow NetworkPolicy lifecycle.** When the last DevPod for
  an owner is deleted, the `devpod-allow-<owner>` policy is orphaned.
  Task 22 / a follow-up plan must reference-count by listing same-owner
  DevPods in the deletion finalizer, OR keep a status field on the
  policy, OR use OwnerReferences on a per-owner aggregator object.

- **Finalizer is added before the owner User existence check.** Mostly
  fine (deletion is a clean no-op), but means
  `kubectl get devpod -o yaml` shows a finalizer even for objects whose
  owner never resolved. Cosmetic; consider moving finalizer-add below
  the User Get.

- **VM-vs-Pod precedence comment.** Reconcile currently does
  `if VM != nil { skip } else if Pod == nil { error/log+nil }`. Add a
  comment that the precedence is intentional defense-in-depth against
  a CEL bypass, OR switch to an explicit `(Pod, VM) == (nil, nil) →
  error; VM != nil → skip; Pod != nil → render` form.

- **`r.GwConfig` is unchecked for nil** in `applyAll` /
  `ensureHostKeySecret`. Single `if r.GwConfig == nil { return error }`
  at the top of `applyAll` would prevent a future-wiring nil-deref.

- **Test coverage at 60%.** Deletion + VM-skip paths uncovered. Add a
  single deletion test before Task 22 lands real finalizer work.

- **Switch the controller-name workaround to a one-shot suite setup**
  once the controller package grows beyond two reconcilers. Today's
  per-test `setupSuite` pays ~5s of envtest startup per test; a
  `sync.OnceFunc`-gated setup amortizes it.

## User controller (from Task 13 review)

- **Add a field indexer for `DevPod.spec.owner`** (Task 15
  `cmd/controller`). Today `UserReconciler.Reconcile` does a full
  `r.List(ctx, &devpods)` per User reconcile and filters in-memory.
  At hundreds of DevPods this is fine; at thousands it bites. Add
  `mgr.GetFieldIndexer().IndexField(ctx, &DevPod{}, "spec.owner", ...)`
  in cmd/controller before `mgr.Start`, then restore the
  `client.MatchingFields{"spec.owner": u.Name}` fast path inside the
  reconciler.

- **Webhook should reject DevPod creates whose `spec.owner` does not
  resolve to a User** (Task 14). The reconciler currently logs and
  returns nil silently, so the user gets no admission-time feedback.

- **Surface `status.Conditions` on User** with at least a `Ready`
  condition (controller has observed; finalizer installed) once a real
  use surfaces. The schema is already in place from Task 3.

- **`SkipNameValidation: ptr.To(true)`** is set in `suite_test.go` to
  let multiple `setupSuite` calls register the same controller names.
  Production `cmd/controller` must NOT set this — controller-runtime's
  name-uniqueness check is a real safeguard there.

## Validating webhook (from Task 14 review)

- **`InjectDecoder` is dead code in controller-runtime v0.20.** The
  framework no longer calls `inject.Decoder`. The current setter works
  only because tests call it explicitly. Task 15 (`cmd/controller`)
  must either call `InjectDecoder` at wire-up, OR — better — migrate
  `DevPodValidator` to the typed `admission.CustomValidator` interface
  (`ValidateCreate / ValidateUpdate / ValidateDelete`) and register via
  `admission.WithCustomValidator(scheme, &DevPod{}, validator)`. The
  latter removes the setter entirely and is the modern idiom.

- **PodName collision detection** (referenced from Task 7/8 sections
  too). Webhook needs a `client.Client` to LIST existing DevPods and
  reject creates whose `<owner>-<name>` would collide. Bigger refactor;
  tracked.

- **User existence check.** Webhook should reject DevPod creates whose
  `spec.owner` doesn't resolve to a User. Today the controller logs and
  no-ops silently — no admission-time feedback. Requires the validator
  to hold a `client.Client`.

- **Reserved label / annotation key check.** Webhook should reject
  `spec.pod.metadata.labels` / `.annotations` that try to override
  reserved keys (`devpod.io/owner`, `devpod.io/devpod`,
  `app.kubernetes.io/managed-by`). The render layer silently drops
  these; loud rejection at admission is friendlier.

- **VM blob content validation.** Today the webhook treats `spec.vm`
  as opaque (it's a RawExtension). When KubeVirt support lands (M4),
  decide whether the webhook should round-trip-validate the
  VirtualMachineSpec or trust KubeVirt's own admission to catch
  errors.

## Final-review gaps (silent omissions from M1 spec)

- **Controller does not write `DevPod.status.endpoint` / `phase` /
  `workloadRef` / `persistentVolumeClaimRef`.** Spec §4.1 step 4
  requires `status.endpoint = "<podIP>:22"` and `status.phase = Running`
  once the workload is up. Gateway (M2) reads `status.endpoint` for
  dialing — **M2 is blocked on this**. Implementation: extend
  `applyAll` to Get the rendered Pod, read `pod.Status.PodIP`,
  patch `dp.Status.Endpoint` + `dp.Status.Phase`. Use status
  subresource. Watch for the rest of the status fields too.

- **Webhook is built and unit-tested but NOT served by `cmd/controller`
  and NOT deployed by the chart.** Currently CEL alone enforces
  `spec.pod xor spec.vm`; everything else (reserved sidecar name,
  shareProcessNamespace=false, owner immutability across updates,
  empty containers) is unenforced at admission. M2 needs:
  1. Migrate `DevPodValidator` to `admission.CustomValidator`
     (`ValidateCreate/Update/Delete`) — see existing InjectDecoder
     note for context.
  2. Wire the webhook server into `cmd/controller`'s manager.
  3. Add cert-manager (or alternative) Certificate to the chart.
  4. Add `ValidatingWebhookConfiguration` to the chart.

- **Gateway has no K8s client.** `cmd/gateway/main.go` reads only
  flags + the host-key file on disk. M2 needs informer-cached
  watches on `User` (for pubkey lookup), `DevPod` (for endpoint
  resolution and owner enforcement), and `GatewayConfig` (for
  trustedProxyKeys / per-cluster settings).

- **CRD YAMLs are duplicated** between `config/crd/bases/` and
  `deploy/chart/templates/crds/`. Today they were copied manually.
  Future `go generate ./...` will regenerate the former without
  touching the latter — silent skew. Either symlink, add a sync
  script in `hack/`, or have controller-gen emit straight into the
  chart.

## LDAP source (from 2026-05-13 feature)

- **`LDAPSpec.{caSecretRef,bindPasswordSecretRef}.namespace` is dead data**
  at runtime. The gateway reads Secrets from the chart-mounted
  `/etc/devpod/gateway/ldap/{ca.crt,password}` paths; the CRD fields
  exist only for chart resolution / `kubectl explain` documentation.
  Either wire them into the gateway's startup (read via `client.Reader`
  instead of disk) or remove them from the CRD schema. Spec change
  required either way.
