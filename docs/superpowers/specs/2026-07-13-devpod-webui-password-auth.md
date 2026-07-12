# DevPod Web UI — Built-in Password Auth

**Status:** Approved (2026-07-13)
**Date:** 2026-07-13
**Audience:** project author and future implementers

A built-in username+password login for the DevPod web UI, independent
of GitLab OAuth. Coexists with OAuth (each toggled on/off). Lets a
cluster with no external IdP run the UI end-to-end.

Extends the web UI design (`2026-07-12-devpod-webui-design.md`); reuses
its session model, `Server` handler sequence, and `User` CRD.

---

## 1. Goals / non-goals

### Goals
- Password login that mints the **same** HMAC session cookie as OAuth,
  so everything downstream (ownership, quota, SSE) is unchanged.
- Password auth and OAuth are independent switches; the login page
  renders whichever are enabled.
- Admins manage regular users' passwords from the UI; users self-serve
  their own password change.

### Non-goals
- Self-registration (admins create users).
- Admin promotion via the UI. **Who is an admin is decided only by
  kubectl** — the webui never writes `User.spec.admin`.
- Password reset via email, MFA, password policies beyond a minimum
  length.
- Rate-limiting infrastructure (bcrypt's cost is the primary brute-force
  defense; a fixed failure delay is the only addition).

---

## 2. CRD change: two `User.spec` fields

```go
// PasswordHash is a bcrypt hash of the user's password. Set = this
// user may log in with a password. Never returned by the API. Written
// only by the webui (admin create / reset, or self-service change).
//
// +optional
PasswordHash string `json:"passwordHash,omitempty"`

// Admin grants admin rights on ANY login path. Written ONLY by an
// operator via kubectl — the webui treats it as read-only. Effective
// admin = (--admins allowlist) OR this field.
//
// +optional
Admin bool `json:"admin,omitempty"`
```

Neither is consumed by the controller or gateway. bcrypt hashes live in
a cluster-scoped CRD spec: anyone who can read `User` (the webui SA,
cluster admins) can read the hashes — the same trust boundary as the
rest of the UI, and bcrypt is designed to resist offline cracking.

---

## 3. Authentication

- **Flag** `--password-auth=true|false` (default false; chart
  `webui.passwordAuth`). OAuth is a fully independent switch (its flags'
  presence). At least one must be enabled or the UI cannot be logged
  into (validated at startup).
- **`GET /api/auth/config`** (unauthenticated) → `{"password": bool,
  "oauth": bool}`. The login page uses it to render the password form,
  the "Sign in with GitLab" button, or both.
- **`POST /api/auth/password`** `{"username","password"}`:
  1. reject if `--password-auth` off (404).
  2. fetch `User` named `username`; if missing or `passwordHash` empty →
     401 (generic "invalid username or password", no user enumeration).
  3. `bcrypt.CompareHashAndPassword`; mismatch → 401 after a fixed
     ~200ms floor so success/failure timing is similar.
  4. mint the session cookie: `Session{User: username, Admin: adminFor(user)}`.
- **Admin determination** unifies both paths:
  `adminFor(user) = adminsAllowlist[username] || user.Spec.Admin`.
  OAuth login also honors `user.Spec.Admin` (one extra field read on the
  User it already provisions).
- **Username = the DevPod owner name directly** (no OAuth-style prefix).
  Validated as a DNS-1123 label leaving DevPod-name budget (reuse the
  mapping validators with an empty prefix).
- **`--password-min-length`** (default 8) gates create/reset/change.

---

## 4. User management endpoints

Self-service (any authenticated user):
- **`PUT /api/me/password`** `{"oldPassword","newPassword"}` — verifies
  the caller's current hash, then sets a new one. Available only when
  the caller has a `passwordHash` (password users); OAuth-only users get
  409.

Admin only (`session.Admin`; else 403; all 403 when `--password-auth`
off since they're meaningless without it):
- **`GET /api/admin/users`** — list users: name, displayName, admin,
  hasPassword, devpod count. Never the hash.
- **`POST /api/admin/users`** `{"username","displayName","password"}` —
  create a `User` with a bcrypt hash. Rejects an existing name, an
  invalid username, or a too-short password. **Cannot set admin** (that
  field is kubectl-only; ignored/rejected if present).
- **`PATCH /api/admin/users/{name}`** `{"password"?,"displayName"?}` —
  reset password / rename. Cannot touch `admin`.
- **`DELETE /api/admin/users/{name}`** — delete the User CR. (The
  gateway's finalizer still blocks deletion while the user owns
  DevPods.)

---

## 5. Frontend

- **`/login`** — fetch `/api/auth/config`; show the password form
  (username + password) and/or the GitLab button accordingly. Password
  submit posts to `/api/auth/password`, then redirects to `/`.
- **`/settings/password`** — old + new password; hidden for OAuth-only
  users (no `passwordHash`; surfaced via `/api/me`).
- **`/admin/users`** — admin-only table: create user (username,
  displayName, initial password), reset password, delete. Admin column
  is read-only, with a hint that admin is granted via kubectl. Linked
  from the header only when `me.admin`.
- `/api/me` gains `features.passwordAuth` and `hasPassword` so the SPA
  can gate the password UI.

---

## 6. Error handling

- Uniform `{code,message,detail}`. Login failures are generic
  (`UNAUTHORIZED`, "invalid username or password") — no enumeration.
- Password too short → `400 BAD_REQUEST`. Duplicate user → `409`.
- Password endpoints when `--password-auth` off → `404` (login) / `403`
  (management), never a 500.

---

## 7. Testing

| Layer | Method |
|---|---|
| bcrypt set/verify, username validation, admin resolution | table-driven unit tests |
| `/api/auth/password` (success cookie, wrong password 401, unknown user 401, disabled 404) | envtest |
| `/api/me/password` (change, wrong old password, OAuth-only 409) | envtest |
| admin user CRUD (create → login works, non-admin → 403, cannot set admin, min-length) | envtest |
| `/api/auth/config` reflects flags | unit/envtest |

---

## 8. Milestone fit

This is a standalone increment on top of M1. It touches: the `User`
CRD (2 fields), `internal/webui` (auth + admin handlers), `cmd/webui`
(flags), the chart (`webui.passwordAuth`, `webui.passwordMinLength`),
and the SPA (login/password/admin-users pages). The M2 admin panel
(global DevPod view, quota editing, Kore topology) is unaffected and
still deferred.
