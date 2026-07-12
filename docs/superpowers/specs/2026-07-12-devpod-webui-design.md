# DevPod Web UI Design

**Status:** Approved (2026-07-12)
**Date:** 2026-07-12
**Audience:** project author and future implementers

A web UI for DevPod: developers self-serve their development environments
from a browser (create, hibernate/wake, snapshot, logs, terminal), and
platform admins get a global view with per-user resource quotas. Login is
OAuth against a self-hosted GitLab; ordinary users never touch kubectl.

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

### Non-goals

- Quota enforcement below the UI layer. The controller does not read
  quota; anyone with kubectl access bypasses it. See §8 Security
  boundary.
- OIDC/SSO for the SSH gateway (webui only).
- Multi-IdP support. GitLab OIDC only; the provider surface is small
  enough to generalize later if needed.
- Billing, usage metering, or cluster-level ResourceQuota integration.

---

## 2. Architecture

Approach chosen: a fourth binary, `devpod-webui`, deployed as its own
stateless Deployment in `devpod-system`. The gateway (security-critical
SSH path) and controller are not modified, with one exception: the
`User` CRD grows a `spec.quota` field that only the webui reads and
writes.

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
- `devpod.io` users: full verbs via ClusterRole — User is
  cluster-scoped, so this is unavoidably a cluster-wide grant; it is
  the webui's only cluster-scoped write.
- core: `pods/log` (M2), `pods/exec` (M3), `events` (read) in the
  devpods namespace.

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

## 4. CRD change: `User.spec.quota`

The only API change in this project. The controller does not consume it.

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
   webui-only by design (§8).
6. Defaults for users with nil quota come from `--default-quota-file`.

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
| `POST /api/devpods` | create; body is form-JSON or raw YAML — one validation + quota path |
| `GET /api/devpods/{name}` | detail |
| `PATCH /api/devpods/{name}` | hibernate/wake (`running`), spec edits |
| `DELETE /api/devpods/{name}` | delete |
| `GET /api/devpods/{name}/events` | related k8s Events |
| `GET /api/devpods/{name}/logs` | M2, SSE/WS stream |
| `GET /api/devpods/{name}/terminal` | M3, WebSocket |
| `POST /api/snapshots`, `GET /api/snapshots?devpod=` | M2 |
| `GET /api/admin/users`, `PATCH /api/admin/users/{name}` | M2, admin: users + usage, quota edit |
| `GET /api/admin/devpods`, `POST /api/admin/devpods/{name}/stop` | M2, admin: global view, force hibernate |

Watch streams are fed from the controller-runtime informer cache — no
polling. On SSE reconnect the client does one full list refresh to
close event gaps.

### Configuration flags

`--gitlab-issuer-url`, `--oauth-client-id`,
`--oauth-client-secret-file`, `--redirect-url`, `--user-prefix`,
`--admins`, `--session-key-file`, `--default-quota-file`,
`--devpod-namespace`, `--listen`, optional `--tls-cert`/`--tls-key`
(default: TLS terminated at the Ingress).

---

## 6. Frontend

React 18 + TypeScript + Vite; output embedded via `go:embed` (single
image, no CDN, matches distroless image style). Tailwind + headless
components — no heavyweight UI kit, small dependency surface. State:
React Query + the SSE watch stream; no Redux.

```
/login                  # "Sign in with GitLab" button
/                       # my DevPods: status badges, endpoint, quick hibernate/wake
/devpods/new            # create: form mode ⇄ YAML mode toggle
/devpods/{name}         # detail: status, events, quota usage, snapshots (M2), logs (M2), terminal (M3)
/settings/pubkeys       # pubkey management + ssh command line display
/admin/users            # (admin, M2) users, usage vs quota, edit
/admin/devpods          # (admin, M2) global view, force hibernate
```

---

## 7. Error handling

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

## 8. Security boundary (explicit)

- **Quota is UI-layer policy, not a security barrier.** Anyone holding
  kubectl credentials for the devpods namespace bypasses it. The
  deployment model assumes ordinary users have no kubeconfig; admins
  handing out kubeconfigs must know this.
- The webui SA holds full CRD rights in the devpods namespace plus a
  cluster-wide grant on the cluster-scoped User CRD, and is a
  high-value target: minimal RBAC (§2), NetworkPolicy on ingress and
  egress, no other cluster-scoped write.
- All ownership checks live server-side in the handler sequence (§2);
  the SPA is untrusted.

---

## 9. Testing

| Layer | Method |
|---|---|
| quota aggregation, username mapping, cookie signing | table-driven unit tests |
| API handlers + ownership checks | envtest (existing `hack/test.sh` infra), forged sessions hit handlers directly |
| OAuth flow | httptest fake OIDC issuer signing test id_tokens |
| frontend | Vitest component tests, thin — heavy logic stays in the backend |
| e2e | `hack/e2e-webui.sh`: kind + fake IdP container; login → create → hibernate → delete |

CI gains a frontend build step via a `hack/` script (no Makefile, per
project convention).

---

## 10. Milestones

- **M1 — skeleton + core self-service.** OAuth login, auto-provision,
  pubkey management, list/detail/hibernate/wake/delete, form + YAML
  create, quota enforcement, chart + RBAC + e2e.
  *Acceptance: a new GitLab user goes from login to
  `ssh <prefix>xxx+pod@gateway` with zero admin involvement.*
- **M2 — observability + admin.** Snapshots (initiate/progress/
  history), log streaming, events, admin panel (global view, quota
  editing, force hibernate).
  *Acceptance: admins no longer need kubectl for day-to-day operations.*
- **M3 — terminal.** xterm.js over `pods/exec` WebSocket, auto
  reconnect.
  *Acceptance: run commands in a DevPod container from the browser.*
