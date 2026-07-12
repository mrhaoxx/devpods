# DevPod Web UI Design

**Status:** Approved (2026-07-12)
**Date:** 2026-07-12
**Audience:** project author and future implementers

A web UI for DevPod: developers self-serve their development environments
from a browser (create, hibernate/wake, snapshot, logs, terminal), and
platform admins get a global view with per-user resource quotas. Login is
OAuth against a self-hosted GitLab; ordinary users never touch kubectl.

The UI also integrates with Kore (github.com/zjusct/kore), the cluster's
CPU-pinning / NUMA-binding / CPU-pool system. CPU bindings are
**template-mediated**: admins curate `DevPodTemplate` CRs that carry the
Kore binding block; ordinary users pick a template (and can see its
binding) but can never author Kore annotations themselves. DevPod
detail pages show actual core bindings, and admins get a per-core
cluster topology visualization (a web `kubectl kore top`) that doubles
as the template editor's visual aid. See §7.

The original design doc (2026-05-12) listed a web UI as a v1 non-goal.
This spec lifts that non-goal now that persistence, hibernation, LDAP,
and snapshots have shipped.

---

## 1. Goals and non-goals

### Goals

- A `devpod-webui` component through which a user can go from "has a
  GitLab account" to "ssh'd into a running DevPod" with zero admin
  intervention.
- OAuth login via a self-hosted GitLab instance (standard OIDC
  authorization code flow + PKCE).
- Deterministic identity mapping: GitLab user `xxxx` acts as DevPod user
  `<prefix>xxxx`, prefix configurable.
- Self-service: list/status, create (form or raw YAML), hibernate/wake,
  delete, SSH pubkey management, snapshots (M2), logs (M2), web terminal
  (M3).
- Admin panel: global DevPod view, per-user quota editing, force
  hibernate (M2).
- Per-user aggregate resource quotas, stored on the `User` CRD, enforced
  by the webui backend at create/mutate/wake time.
- Kore compatibility and visualization, template-mediated: admin-curated
  `DevPodTemplate` CRs are the only way bindings reach non-admin
  DevPods; users get read-only template visibility and binding readback
  on the detail page; admins get a per-core topology view fed by
  `KoreNodeTopology` CRs, embedded in the template editor. All Kore
  features are gated and degrade to hidden when Kore is not installed
  (`--kore=auto|on|off`).

### Non-goals

- Quota enforcement below the UI layer. The controller does not read
  quota; anyone with kubectl access bypasses it. See §9 Security
  boundary.
- OIDC/SSO for the SSH gateway (webui only).
- Multi-IdP support. GitLab OIDC only; the provider surface is small
  enough to generalize later if needed.
- Billing, usage metering, or cluster-level ResourceQuota integration.

---

## 2. Architecture

Approach chosen: a fourth binary, `devpod-webui`, deployed as its own
stateless Deployment in `devpod-system`. The gateway (security-critical
SSH path) and controller are not modified, with two API exceptions the
controller never consumes: the `User` CRD grows a `spec.quota` field,
and a new cluster-scoped `DevPodTemplate` CRD is added (§4). Both are
read and written only by the webui.

```
┌──────────────────────────────────────────────────────────────┐
│ devpod-system namespace                                      │
│                                                              │
│  devpod-webui (Deployment, N replicas, stateless)            │
│    ├─ HTTP :8080 ── go:embed React SPA + JSON API            │
│    ├─ OAuth client ──→ self-hosted GitLab (OIDC code+PKCE)   │
│    ├─ controller-runtime cached client ──→ kube-apiserver    │
│    │    (dedicated SA, minimal RBAC)                         │
│    └─ WebSocket/SSE ── watch stream, logs (M2), term (M3)    │
│                                                              │
│  devpod-controller / devpod-gateway  (unchanged†)            │
└──────────────────────────────────────────────────────────────┘
```

The webui executes every Kubernetes operation under its own
ServiceAccount and enforces per-user authorization (ownership checks)
itself — the same trusted-intermediary model the gateway already uses
for SSH.

Every handler runs the same fixed sequence:

1. verify session cookie signature
2. map GitLab username → `<prefix><username>`
3. ownership / admin check
4. (mutations) quota check
5. execute against the API server under the webui SA

### Code layout

```
cmd/webui/main.go            # flags, manager, HTTP server assembly
internal/webui/
  server.go                  # routes, middleware
  oauth.go                   # GitLab OIDC flow, callback, prefix mapping
  session.go                 # HMAC-signed cookie sessions (stateless)
  api_devpods.go             # list/get/create/patch/delete DevPods
  api_users.go               # pubkeys, auto-provision, (admin) quota
  api_templates.go           # template list (all users), CRUD (admin, M2),
                             #   server-side template application
  api_snapshots.go           # M2
  quota.go                   # quota aggregation and validation
  terminal.go                # M3: pods/exec ↔ WebSocket
web/                         # React + TypeScript + Vite sources
  dist/                      # build output, go:embed'ed
images/webui/Dockerfile
deploy/chart/templates/webui-*.yaml
hack/e2e-webui.sh
```

### RBAC (webui ServiceAccount)

- `devpod.io` devpods, devpodsnapshots: full verbs, namespaced Role in
  the devpods namespace only.
- `devpod.io` users, devpodtemplates: full verbs via ClusterRole — both
  CRDs are cluster-scoped, so these are unavoidably cluster-wide
  grants; they are the webui's only cluster-scoped writes.
- core: `pods` (read — Kore binding readback lives in Pod annotations),
  `pods/log` (M2), `pods/exec` (M3), `events` (read) in the devpods
  namespace.
- `kore.zjusct.io` korenodetopologies: get/list/watch via ClusterRole
  (cluster-scoped, read-only; only bound when Kore integration is on).

A NetworkPolicy restricts webui ingress to the Ingress controller and
egress to the API server and GitLab.

---

## 3. Identity: OAuth, mapping, sessions

### OIDC flow

- Standard discovery at `<gitlab>/.well-known/openid-configuration`;
  authorization code flow with PKCE; scopes `openid profile`.
- Username taken from the `preferred_username` claim of the verified
  id_token.

### Mapping

- `devpodUser = <prefix> + preferred_username`, prefix from config
  (may be empty).
- The result must be a valid DNS-1123 label; otherwise login is refused
  with an explicit error page stating the offending name.
- DevPod CR names must start with `<owner>-` and are capped at 22
  characters (existing CEL rules). The create form lets the user type
  only the suffix and live-displays the remaining budget:
  `22 - len(devpodUser) - 1`. Deployment docs warn that a long prefix
  eats this budget.

### Sessions

- HMAC-SHA256-signed cookie carrying `{username, expiry, admin}`;
  TTL 24h; `HttpOnly + Secure + SameSite=Lax`; mutating requests also
  require a matching `Origin` header (CSRF belt-and-braces).
- HMAC key mounted from a Secret; all replicas share it → no sticky
  sessions, no server-side session store. OAuth `state` + PKCE verifier
  live in short-lived cookies, also stateless.

### First-login provisioning

On first successful login the webui idempotently creates the `User` CR
`<prefix><username>` (empty pubkeys, nil quota = global defaults);
`IsAlreadyExists` is ignored. The pubkey management page then lets the
user paste SSH keys and shows the exact
`ssh <devpodUser>+<pod>@<gateway>` command line — full self-service from
OAuth login to SSH.

### Admin determination

A configured list of GitLab usernames (Helm values). Upgradeable to a
GitLab group claim later; out of scope now.

---

## 4. CRD changes

Two additions, neither consumed by the controller.

### 4.1 `User.spec.quota`

```go
// UserQuota caps aggregate resources across a User's DevPods.
// nil = webui global defaults apply; admins are exempt.
type UserQuota struct {
    // MaxDevPods limits how many DevPod CRs the user may own.
    // +optional
    MaxDevPods *int32 `json:"maxDevPods,omitempty"`

    // Compute caps the SUM of container resource limits across the
    // user's *running* DevPods. Keys: cpu, memory, extended resources
    // (e.g. nvidia.com/gpu).
    // +optional
    Compute corev1.ResourceList `json:"compute,omitempty"`

    // Storage caps the SUM of persistence.size across ALL of the
    // user's DevPods, hibernated included (PVCs survive hibernation).
    // +optional
    Storage *resource.Quantity `json:"storage,omitempty"`
}
```

### Quota semantics

1. **Compute counts only `running: true` DevPods.** Hibernated pods
   consume nothing; therefore *waking* a DevPod re-runs the quota check
   and can be refused.
2. **Storage counts all DevPods** because PVCs persist through
   hibernation. Deleting the DevPod frees the budget.
3. **Summation is over container `limits`,** across both `containers`
   and `initContainers` of `spec.pod.spec` (simple and conservative;
   no max(init, main) refinement). Every container in a non-admin
   DevPod must declare limits for all quota'd resources it uses: the
   create form injects them; the YAML path rejects specs with missing
   limits. Admins are exempt from both quota and the
   must-declare-limits rule.
4. **Pod workloads only.** `spec.vm` is a RawExtension the webui cannot
   meter, so non-admin creates/updates carrying `spec.vm` are rejected;
   admins may submit VM DevPods via the YAML path.
5. Enforcement points: `POST /api/devpods` (create), `PATCH` that
   changes the pod spec or sets `running: true`. Enforcement is
   webui-only by design (§9).
6. Defaults for users with nil quota come from `--default-quota-file`.
7. Kore interplay: pinned CPUs are ordinary integer `cpu` limits, so
   quota needs no special casing — resources implied by a template's
   binding count against the creating user like any others. Kore's
   webhook injects its `kore.zjusct.io/cpu` gate resource at Pod
   admission — it never appears in the DevPod CR and is invisible to
   quota.

### 4.2 `DevPodTemplate` (new, cluster-scoped)

Admin-curated templates are the only path by which Kore bindings reach
non-admin DevPods (§7). One CRD, two usages, composable:

```go
// DevPodTemplateSpec: at least one of Binding / PodPreset must be set.
type DevPodTemplateSpec struct {
    DisplayName string `json:"displayName"`
    // +optional
    Description string `json:"description,omitempty"`

    // Binding, if set, is the Kore binding block stamped onto DevPods
    // created from this template.
    // +optional
    Binding *BindingSpec `json:"binding,omitempty"`

    // PodPreset, if set, makes this a full preset (one-click create).
    // +optional
    PodPreset *PodPresetSpec `json:"podPreset,omitempty"`
}

// BindingSpec carries Kore annotations plus the resources they imply.
type BindingSpec struct {
    // Annotations restricted to the kore.zjusct.io/* whitelist
    // (pin, pool, pool-size, numa-policy, memory-policy, placement,
    // smt-policy — NOT cpuset, which stays an admin escape hatch).
    Annotations map[string]string `json:"annotations"`
    // Resources the binding implies for the target container
    // (pin: integer CPU with requests == limits).
    Resources corev1.ResourceRequirements `json:"resources"`
}

// PodPresetSpec fixes the user-visible knobs of a full preset.
type PodPresetSpec struct {
    Image string `json:"image"`
    // +optional
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`
    // +optional
    Persistence *PersistenceSpec `json:"persistence,omitempty"`
    // +optional
    Shell string `json:"shell,omitempty"`
}
```

- **Binding-only** template = a binding overlay (e.g. "exclusive 8
  cores, single NUMA") users attach to an otherwise custom DevPod.
- **Binding + PodPreset** (or preset-only) = a one-click preset (e.g.
  "GPU dev box 8C64G pinned"); the user supplies only the name suffix.
- The webui validates a template's binding against the mirrored Kore
  rules when an admin saves it — authoring errors surface at
  template-save time, not when some user later instantiates it.
- Templates are stamped at DevPod create time; editing a template never
  retro-applies to existing DevPods. Live pool resizing and similar
  operations are admin direct-patches on the DevPod.

---

## 5. HTTP API

Uniform error body: `{code, message, detail}`. Kubernetes `Status`
errors (CEL rejections, admission failures) are passed through verbatim
in `detail` — the CEL messages are already user-legible. Quota
rejections return `409` with code `QUOTA_EXCEEDED` and a
`{requested, used, limit}` triple per resource.

| Route | Purpose |
|---|---|
| `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout` | OIDC flow |
| `GET /api/me` | identity, admin bit, quota + current usage |
| `GET /api/me/pubkeys`, `PUT /api/me/pubkeys` | SSH pubkey self-service (writes User CR) |
| `GET /api/devpods` (`?watch=true` → SSE) | own DevPods, live status |
| `POST /api/devpods` | create; body: form-JSON or raw YAML + optional `templateRef` — one validation + quota + template-stamping path |
| `GET /api/templates` | templates, binding details included (read-only for everyone) |
| `GET /api/devpods/{name}` | detail |
| `PATCH /api/devpods/{name}` | hibernate/wake (`running`), spec edits |
| `DELETE /api/devpods/{name}` | delete |
| `GET /api/devpods/{name}/events` | related k8s Events |
| `GET /api/devpods/{name}/logs` | M2, SSE/WS stream |
| `GET /api/devpods/{name}/terminal` | M3, WebSocket |
| `POST /api/snapshots`, `GET /api/snapshots?devpod=` | M2 |
| `GET /api/admin/users`, `PATCH /api/admin/users/{name}` | M2, admin: users + usage, quota edit |
| `GET /api/admin/devpods`, `POST /api/admin/devpods/{name}/stop` | M2, admin: global view, force hibernate |
| `POST/PUT/DELETE /api/admin/templates/{name}` | M2, admin: template CRUD (M1 seeds templates via kubectl/GitOps) |
| `GET /api/kore/topology` (`?watch=true` → SSE) | M2, per-node core ledger from KoreNodeTopology CRs |
| `GET /api/kore/pools` | M2, CPU pools + members |

Watch streams are fed from the controller-runtime informer cache — no
polling. On SSE reconnect the client does one full list refresh to
close event gaps.

### Configuration flags

`--gitlab-issuer-url`, `--oauth-client-id`,
`--oauth-client-secret-file`, `--redirect-url`, `--user-prefix`,
`--admins`, `--session-key-file`, `--default-quota-file`,
`--devpod-namespace`, `--listen`, `--kore=auto|on|off` (auto = enable
when the KoreNodeTopology CRD exists), optional
`--tls-cert`/`--tls-key` (default: TLS terminated at the Ingress).

---

## 6. Frontend

React 18 + TypeScript + Vite; output embedded via `go:embed` (single
image, no CDN, matches distroless image style). Tailwind + headless
components — no heavyweight UI kit, small dependency surface. State:
React Query + the SSE watch stream; no Redux.

```
/login                  # "Sign in with GitLab" button
/                       # my DevPods: status badges, endpoint, quick hibernate/wake
/devpods/new            # create: preset picker | custom form (+ optional
                        #   binding-overlay template) | YAML. Binding is
                        #   template-only for non-admins; template cards
                        #   show their binding details
/devpods/{name}         # detail: status, events, quota usage, Kore binding,
                        #   snapshots (M2), logs (M2), terminal (M3)
/settings/pubkeys       # pubkey management + ssh command line display
/admin/users            # (admin, M2) users, usage vs quota, edit
/admin/devpods          # (admin, M2) global view, force hibernate
/admin/topology         # (admin, M2) per-core cluster CPU map — web `kore top`
/admin/templates        # (admin, M2) template CRUD; binding editor renders
                        #   beside the live topology grid
```

---

## 7. Kore integration

Kore (github.com/zjusct/kore) drives CPU pinning, NUMA binding, and CPU
pools entirely through Pod annotations; DevPod's render layer already
merges `spec.pod.metadata.annotations` into the rendered Pod
(`internal/render/pod.go`), and Kore's own webhook injects
`schedulerName` and its gate resource at Pod admission. Compatibility
therefore requires **no controller or render changes** — the webui's job
is to mediate the annotation protocol through admin-curated templates
(§4.2) and mirror Kore's validation rules at template-save time, so
binding errors surface at authoring, not at reconcile.

### Template-mediated binding (M1)

Ordinary users never author Kore annotations. The create flow is a
three-way choice:

- **Full preset** — pick a preset template; the user supplies only the
  DevPod name suffix.
- **Custom + binding overlay** — user chooses image, persistence, etc.
  freely and optionally attaches a binding-only template; the server
  stamps the template's annotations and implied resources onto the
  generated spec.
- **Plain custom** (default) — no template, no annotations; the pod
  lands in Kore's shared pool.

**Stamping invariant:** any `kore.zjusct.io/*` annotation appearing in
a non-admin submitted spec — form or YAML, create or patch — is
rejected with an explicit error. Bindings enter DevPod specs only via
server-side template application. (A PATCH that would drop or alter
stamped annotations is likewise rejected for non-admins.)

Users have read-only visibility into templates: the picker and template
cards show the binding in full (policy, core count, pool name/size).

Admins are unrestricted: arbitrary YAML, including the explicit-cpuset
escape hatch (`kore.zjusct.io/cpuset` + `nodeName`), which is never
templated.

### Binding readback: detail page (M1)

Kore writes actual placement back onto the Pod:
`kore.zjusct.io/reserved-numa` (scheduler) and
`kore.zjusct.io/allocated-cpuset` (agent). The DevPod detail page reads
the live Pod (hence `pods` read in RBAC) and shows the bound cores, NUMA
zone, and — for pool members — the pool name, size, and co-members
(cross-referenced from `KoreNodeTopology.status.pools`).

### Topology visualization: web `kore top` (M2)

`/admin/topology`, fed by an SSE watch on KoreNodeTopology CRs (one CR
per node, `status`: zones with cpus/freeCpus/SMT-sibling pairs/memory/
devices, allocations per container, pools):

- One card per node; within it one block per NUMA zone; within that a
  per-core grid, SMT siblings stacked as columns (same layout language
  as `kubectl kore top`).
- Cell color = occupant class (exclusive pod / pool / shared /
  system-reserved); hover reveals pod/container/pool details; clicking a
  DevPod-owned allocation deep-links to its detail page.
- A pools table (name, cpuset, NUMA, members) sits below the grid.
- Live updates ride the same SSE reconnect semantics as the DevPod list
  (§8).
- The same grid component renders beside the template editor
  (`/admin/templates`), so an admin adjusting a template's binding sees
  current free cores and pool layout while making the call.

The dataset (node names, all pods' placements) is operator-level
information, so the page is admin-only; per-user binding info is
already on the detail page.

### Gating

`--kore=auto|on|off`, auto = probe for the KoreNodeTopology CRD at
startup. When off/absent: binding(-only) templates are hidden from the
picker (presets without a binding remain usable), the detail binding
panel, topology page, and `/api/kore/*` all disappear; the ClusterRole
binding for korenodetopologies is chart-conditional on the same switch.
DevPods that already carry Kore annotations still render/patch fine —
the annotations are inert without Kore installed.

---

## 8. Error handling

- API errors: uniform `{code, message, detail}`; k8s Status passthrough
  as above.
- Quota refusal: 409 + `{requested, used, limit}`; frontend renders a
  usage bar and names the exceeded resource.
- GitLab unreachable → login page banner. Illegal mapped username →
  refusal page with the reason. Expired session → 401 → frontend
  redirects to login.
- SSE disconnect → exponential backoff reconnect → full list refresh.
- webui crash/restart: stateless — cookie sessions survive, SSH path
  unaffected (separate binary by design).

---

## 9. Security boundary (explicit)

- **Quota is UI-layer policy, not a security barrier.** Anyone holding
  kubectl credentials for the devpods namespace bypasses it. The
  deployment model assumes ordinary users have no kubeconfig; admins
  handing out kubeconfigs must know this. (Kore's own admission rules
  are NOT ours to enforce — the webui mirrors them at template-save
  time only for UX; Kore's webhook/scheduler remain the real gate for
  binding validity.)
- **Non-admin specs must never carry `kore.zjusct.io/*` annotations.**
  The webui rejects them on every create and patch; bindings are
  stamped server-side from templates only (§7). This keeps admission to
  scarce exclusive cores an admin-curated decision.
- The webui SA holds full CRD rights in the devpods namespace plus a
  cluster-wide grant on the cluster-scoped User CRD, and is a
  high-value target: minimal RBAC (§2), NetworkPolicy on ingress and
  egress, no other cluster-scoped write.
- All ownership checks live server-side in the handler sequence (§2);
  the SPA is untrusted.

---

## 10. Testing

| Layer | Method |
|---|---|
| quota aggregation, username mapping, cookie signing, Kore annotation validation mirrors, template stamping + non-admin annotation rejection | table-driven unit tests |
| API handlers + ownership checks | envtest (existing `hack/test.sh` infra), forged sessions hit handlers directly |
| Kore topology API | envtest with the KoreNodeTopology CRD applied and hand-crafted status fixtures (no live Kore needed) |
| OAuth flow | httptest fake OIDC issuer signing test id_tokens |
| frontend | Vitest component tests, thin — heavy logic stays in the backend |
| e2e | `hack/e2e-webui.sh`: kind + fake IdP container; login → create → hibernate → delete |

CI gains a frontend build step via a `hack/` script (no Makefile, per
project convention).

---

## 11. Milestones

- **M1 — skeleton + core self-service.** OAuth login, auto-provision,
  pubkey management, list/detail/hibernate/wake/delete, DevPodTemplate
  CRD + three-way create (preset / custom + overlay / plain; form +
  YAML) with server-side template stamping and the non-admin
  Kore-annotation rejection rule, binding readback, quota enforcement,
  chart + RBAC + e2e. Admins seed templates via kubectl/GitOps in M1.
  *Acceptance: a new GitLab user goes from login to
  `ssh <prefix>xxx+pod@gateway` with zero admin involvement — including
  a pinned or pooled DevPod via template when Kore is installed.*
- **M2 — observability + admin.** Snapshots (initiate/progress/
  history), log streaming, events, admin panel (global view, quota
  editing, force hibernate), Kore topology visualization
  (`/admin/topology` + pools), template editor with the topology grid
  embedded.
  *Acceptance: admins no longer need kubectl for day-to-day operations —
  `kubectl kore top` and template curation included.*
- **M3 — terminal.** xterm.js over `pods/exec` WebSocket, auto
  reconnect.
  *Acceptance: run commands in a DevPod container from the browser.*
