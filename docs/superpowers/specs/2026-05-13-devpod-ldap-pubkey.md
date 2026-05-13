# DevPod — external LDAP as a secondary pubkey source

**Status:** Approved
**Date:** 2026-05-13
**Depends on:** v2 in-container sshd merged; current gateway auth path
(`internal/gateway/auth.go`).

---

## 1. Goal

Let DevPod resolve a login user's authorized SSH public keys from an
external LDAP directory **in addition to** the existing `User` CRD,
without changing the user-facing `<user>+<pod>@gateway` SSH login shape.

The two sources behave as an ordered identity stack keyed by `username`:

1. **`User` CRD** — checked first, exactly as today.
2. **LDAP** — checked next, when configured.

A login is accepted when **any** source agrees that the offered SSH
public key authorizes that username. If the User CRD has the username
but none of its `spec.pubkeys` matches, the gateway still consults LDAP
("static first, LDAP as a fallback") — this lets an admin keep a small
break-glass static key in CRD while LDAP carries the day-to-day keys.

**Non-goals:**

- LDAP for `owner` / `collaborator` *authorization* logic. Those fields
  on `DevPod` stay as opaque `username` strings; whichever source proved
  the connecting client authorized the offered key for that username is
  what wins.
- Synchronizing LDAP keys into `User.spec.pubkeys` or any new status
  field. LDAP keys never materialize on the CRD.
- Auto-creating User CRDs for LDAP users. A user who exists only in
  LDAP simply doesn't have a `User` CRD; the gateway works on the
  username string alone.
- Multiple LDAP servers. One configured server only; failover is
  delegated to whatever virtual IP / load balancer the operator points
  `spec.ldap.url` at.
- OIDC, SAML, Kerberos, or any non-LDAP source. The `IdentitySource`
  abstraction makes them trivial later; not in scope for this spec.
- Hot reload of `GatewayConfig.spec.ldap`. Param changes require
  restarting the gateway Pod (rolling deploy is already zero-downtime).

---

## 2. Architecture

### 2.1 `IdentitySource` abstraction

New file `internal/gateway/identity.go`:

```go
package gateway

import (
    "context"
    "errors"

    "golang.org/x/crypto/ssh"
)

// ErrIdentityNotFound — the source does not know this username. Callers
// should try the next source in the chain. This is not an error in the
// log-it-and-page sense; it's a normal signal that the source has
// nothing to say.
var ErrIdentityNotFound = errors.New("identity not found in this source")

// IdentitySource resolves a username to a set of authorized SSH
// public keys. Sources do NOT compare against an offered key; that
// stays in the Authenticator so the comparison and the cross-source
// ordering live in one place.
type IdentitySource interface {
    // Resolve returns the authorized pubkeys for username.
    //
    //   (keys, nil)                    — source knows this user
    //   (nil,  ErrIdentityNotFound)    — source does not know this user
    //   (nil,  err)                    — real failure (network, etc.)
    Resolve(ctx context.Context, username string) ([]ssh.PublicKey, error)

    // Name is the source's audit/metric label ("crd", "ldap").
    Name() string
}
```

### 2.2 Authenticator rewiring

`internal/gateway/auth.go` keeps its public shape; behavior changes:

- Add field `sources []IdentitySource` and builder `WithSources(...)`.
- `Authenticate(ctx, loginName, key)` flow:
  1. `ParseLoginName(loginName)` → `(user, pod)`. Unchanged.
  2. `Get` DevPod from cache (so a bad pod name fails fast and audit
     records `pod=` even when the user doesn't exist anywhere).
  3. **Trusted-proxy short-circuit** by fingerprint, **unchanged.**
  4. For each `src` in `sources` (CRD first, LDAP second):
     - `keys, err := src.Resolve(ctx, user)`
     - `errors.Is(err, ErrIdentityNotFound)` → continue.
     - other `err != nil` → record on `lastErr`, increment
       `auth_attempts_total{source=<name>, result=error}`, continue.
     - if `matchesAny(key, keys)` → set `AuthPath.Source = src.Name()`
       and break to step 5.
     - if `keys` non-empty but no match → continue (fallback semantics).
  5. If no source matched: return `ErrPubkeyMismatch` (if at least one
     source returned keys) or `ErrUserNotFound` (if no source recognized
     the username). `lastErr`, when set, goes to the audit row but
     never to the SSH client.
  6. `accessAllowed(dp, user)` + `dp.Status.Phase==Running` + endpoint
     present. Unchanged.

### 2.3 Source implementations

- **`crdSource`** (`internal/gateway/identity.go`, near the interface).
  Wraps `client.Reader` and a `devPodNamespace string` (existing
  Authenticator field reused). `Get(User)` → `IsNotFound` becomes
  `ErrIdentityNotFound`; other errors pass through. Parses
  `u.Spec.Pubkeys` with `ssh.ParseAuthorizedKey`; unparseable lines
  are skipped with a `pubkey_parse_errors_total{source=crd}` increment.
  Today's inline logic in `Authenticator.Authenticate` moves here.

- **`ldapSource`** (new file `internal/gateway/ldap.go`). Full design
  in §4.

### 2.4 Wiring in `cmd/gateway/main.go`

After loading `GatewayConfig`:

```go
srcs := []gateway.IdentitySource{
    gateway.NewCRDSource(mgr.GetClient()),     // User CRD is cluster-scoped
}
if cfg.Spec.LDAP != nil {
    ld, err := gateway.NewLDAPSource(ctx, *cfg.Spec.LDAP, secretReader)
    if err != nil {
        return fmt.Errorf("ldap source: %w", err) // fail-fast
    }
    srcs = append(srcs, ld)
}
auth := gateway.NewAuthenticator(mgr.GetClient(), cfg.Spec.DevPodNamespace).
        WithSources(srcs).
        WithProxyKeys(trustedProxyIndex)
```

`secretReader` is the existing helper used for `HostKeyRef` /
`InternalKeyRef` (a `client.Reader` against the management-cluster
namespace where those Secrets live); nothing new there.

---

## 3. CRD changes — `GatewayConfig.spec.ldap`

Only `api/v1alpha1/gatewayconfig_types.go` changes. `User` and `DevPod`
types are untouched.

```go
// LDAPSpec configures a single external LDAP identity source queried
// after the User CRD source. Disabled when the parent field is nil.
type LDAPSpec struct {
    // URL is the LDAP server URL. Only ldaps:// is supported (plaintext
    // ldap:// is refused at admission).
    //
    // +kubebuilder:validation:Pattern=`^ldaps://[^/\s]+(:[0-9]+)?$`
    URL string `json:"url"`

    // CASecretRef points to a Secret whose key "ca.crt" holds the
    // PEM-encoded LDAP server CA bundle. The system trust store is
    // never consulted, so a CA rotation here is an explicit, observable
    // event rather than a silent drift.
    CASecretRef SecretRef `json:"caSecretRef"`

    // BindDN is the DN used for simple bind.
    //
    // +kubebuilder:validation:MinLength=1
    BindDN string `json:"bindDN"`

    // BindPasswordSecretRef points to a Secret whose key "password"
    // holds the bind password.
    BindPasswordSecretRef SecretRef `json:"bindPasswordSecretRef"`

    // BaseDN is the search base for user entries.
    //
    // +kubebuilder:validation:MinLength=1
    BaseDN string `json:"baseDN"`

    // UserFilter is a Go text/template for the LDAP search filter,
    // rendered against {.Username string}. Username has already passed
    // the DevPod login-name regex ([a-z0-9-]{1,32}); the gateway
    // additionally RFC 4515-escapes it before substitution as
    // defense-in-depth.
    //
    // Default: `(&(objectClass=posixAccount)(uid={{.Username}}))`
    //
    // +optional
    UserFilter string `json:"userFilter,omitempty"`

    // PubkeyAttribute is the LDAP attribute that carries OpenSSH-format
    // authorized_keys lines. Defaults to "sshPublicKey" (OpenSSH schema
    // OID 1.3.6.1.4.1.24552.500.1.1.1.13). Multi-valued attributes are
    // supported; each value is parsed independently.
    //
    // +optional
    // +kubebuilder:default=sshPublicKey
    PubkeyAttribute string `json:"pubkeyAttribute,omitempty"`

    // RequestTimeoutSeconds bounds a single LDAP search round-trip.
    //
    // +optional
    // +kubebuilder:default=5
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=30
    RequestTimeoutSeconds int32 `json:"requestTimeoutSeconds,omitempty"`

    // CacheTTLSeconds is the positive-cache lifetime. Within this
    // window, a successful prior lookup is served without touching
    // LDAP.
    //
    // +optional
    // +kubebuilder:default=300
    // +kubebuilder:validation:Minimum=10
    CacheTTLSeconds int32 `json:"cacheTTLSeconds,omitempty"`

    // NegativeCacheTTLSeconds is the lifetime of a "this user is not in
    // LDAP" cache entry. Shorter than CacheTTLSeconds to make
    // user-onboarding feel responsive while still bounding DoS via
    // unknown-name floods.
    //
    // +optional
    // +kubebuilder:default=30
    // +kubebuilder:validation:Minimum=0
    NegativeCacheTTLSeconds int32 `json:"negativeCacheTTLSeconds,omitempty"`

    // StaleGraceSeconds is the soft-fail window. When LDAP is currently
    // failing, a cache entry whose age is between CacheTTLSeconds and
    // CacheTTLSeconds + StaleGraceSeconds is still served (audit:
    // served_stale=true). Beyond that, the entry is treated as evicted.
    //
    // +optional
    // +kubebuilder:default=900
    // +kubebuilder:validation:Minimum=0
    StaleGraceSeconds int32 `json:"staleGraceSeconds,omitempty"`
}

// In GatewayConfigSpec:
//
// LDAP, when non-nil, registers a secondary identity source queried
// after the User CRD source. Disabled when nil.
//
// +optional
LDAP *LDAPSpec `json:"ldap,omitempty"`
```

**Regen flow:** `go generate ./api/v1alpha1/...` updates
`zz_generated.deepcopy.go`, `config/crd/bases/devpod.io_gatewayconfigs.yaml`,
and (via the existing `hack/sync-crd-chart` hook) the chart's CRD copy.

**`User` CRD:** unchanged. `spec.pubkeys MinItems=1` stays — a User
written into etcd must declare at least one static key; "I want to use
LDAP" is expressed by not having a User CRD at all.

**Helm chart (`deploy/chart/`):**

- `values.yaml` grows a `gateway.ldap` subtree (off by default):
  ```yaml
  gateway:
    ldap:
      enabled: false
      url: ""
      bindDN: ""
      baseDN: ""
      caSecret: { name: "", namespace: "" }
      bindPasswordSecret: { name: "", namespace: "" }
      # remaining knobs default to the CRD defaults
  ```
- The GatewayConfig template renders `spec.ldap:` only when
  `gateway.ldap.enabled` is true. Operators bring their own Secrets;
  the chart does **not** generate them.

**RBAC:** the gateway ServiceAccount already has `get` on the Secrets
referenced by `HostKeyRef` / `InternalKeyRef`. Extend the same Role to
the two LDAP Secrets. No new ClusterRole; namespace-scoped Role binding.

---

## 4. LDAP source — internal design

`internal/gateway/ldap.go` (new). Uses `github.com/go-ldap/ldap/v3`.

### 4.1 Type

```go
type ldapSource struct {
    spec        v1alpha1.LDAPSpec
    bindPass    []byte                     // loaded once, in memory only
    caPool      *x509.CertPool             // loaded once
    filterTmpl  *template.Template         // parsed at construction
    clock       func() time.Time           // injected for tests

    connMu      sync.Mutex
    conn        *ldap.Conn                 // single long-lived conn

    cacheMu     sync.RWMutex
    cache       map[string]*cacheEntry
    flight      singleflight.Group         // collapses concurrent same-user lookups
}

type cacheEntry struct {
    keys      []ssh.PublicKey   // nil ⇒ negative cache (LDAP says no such user)
    fetchedAt time.Time
    lastErrAt time.Time         // zero ⇒ last attempt succeeded
}
```

### 4.2 Construction (`NewLDAPSource`)

1. Load Secret referenced by `CASecretRef`, read key `"ca.crt"`. Empty
   or invalid PEM → return error (fail-fast).
2. Load Secret referenced by `BindPasswordSecretRef`, read key
   `"password"`. Empty → error.
3. Parse `UserFilter` (or its default) as `text/template`. Render once
   with `Username="probe"` to catch malformed templates at startup.
4. Build `x509.CertPool` from the CA bytes; pin it on `tls.Config{
   RootCAs: pool, ServerName: <host from URL> }`.
5. **Do not dial** at construction — let the first Resolve do that.
   Reason: avoids coupling gateway readiness to LDAP availability
   (we want gateway-up + LDAP-down to still serve CRD users).

### 4.3 `Resolve(ctx, username)` algorithm

Per-entry "freshness TTL" is `CacheTTL` for positive entries and
`NegativeCacheTTL` for negative entries (`keys == nil`); both are read
off `spec` each lookup so a config change in a future hot-reload would
take effect on next call (not in scope today, but cheap to allow for).

```text
1. cacheMu.RLock
2. e, ok := cache[username]; cacheMu.RUnlock
3. if ok:
     ttl  = CacheTTL if e.keys != nil else NegativeCacheTTL
     age  = clock() - e.fetchedAt
     if e.lastErrAt == 0 and age < ttl:
         metrics.LDAPLookup("cache_fresh")
         return e.keys (or ErrIdentityNotFound if e.keys == nil)
     if e.lastErrAt != 0 and age < ttl + StaleGrace:
         metrics.LDAPLookup("cache_stale")
         return e.keys with ServedStale=true
         (or ErrIdentityNotFound if e.keys == nil)
4. v, err, _ := flight.Do(username, func() {
       return liveLDAPLookup(ctx, username)
   })
   On success: cacheMu.Lock; cache[username] = {keys: v, fetchedAt:
       now, lastErrAt: 0}; cacheMu.Unlock. Metric "hit" if keys!=nil
       else "miss".
   On error:
     cacheMu.Lock
     if existing entry e' in cache: e'.lastErrAt = now (keep keys,
         keep fetchedAt — preserves the stale-grace window for future
         calls)
     // we do NOT insert a fresh negative-style entry on error;
     // without prior data we have nothing to serve and must surface
     // the error to the caller
     cacheMu.Unlock
     metrics.LDAPLookup("error")
5. Return path:
   - step 4 success: return new value
   - step 4 error and there was a prior fresh-or-stale entry we
     already returned from step 3 above? then we never reach step 4
     (step 3 returned). So step 5 only fires when step 4 ran.
   - step 4 error and no prior cache entry: return err (the
     caller's auth gets ErrPubkeyMismatch / ErrUserNotFound via
     §2.2's decision tree).
```

Note: step 3's "stale but in grace" branch returns immediately without
calling step 4 — soft-fail is the *contract* during an outage, not
just a last-resort. This caps LDAP load when the upstream is sick.
The retry cadence to recover is driven naturally by traffic: when the
positive TTL expires *and* `lastErrAt == 0` (i.e., last attempt
succeeded — the freshness branch took it before we ever saw an
error), step 4 runs. When the entry is in the stale-grace window and
`lastErrAt != 0`, we don't retry on this call; the next call after
grace expires falls through to step 4.

`liveLDAPLookup`:

1. Acquire `connMu`. If `conn == nil` → `ldap.DialURL(spec.URL,
   ldap.DialWithTLSConfig(tlsCfg))` + `conn.Bind(bindDN, bindPass)`.
   Both with `RequestTimeoutSeconds`.
2. Render filter via template with `Username =
   escapeRFC4515(username)`.
3. `conn.SearchWithPaging` or simple `Search` (paging unnecessary for
   single-user lookup) with `BaseDN`, `ScopeWholeSubtree`,
   `DerefAlways`, `SizeLimit: 2` (so a misconfigured filter that
   returns thousands fails loud), `Attributes: [PubkeyAttribute]`,
   `TimeLimit: RequestTimeoutSeconds`.
4. 0 entries → return `(nil, nil)` meaning "negative". Caller writes
   negative cache with NegativeCacheTTLSeconds (see step 5 in
   Resolve — negative entries use a separate effective TTL during the
   freshness check; alternatively store `negativeTTL time.Duration`
   on `cacheEntry` and use it in the freshness branch).
5. 1+ entries → take the first entry, iterate
   `entry.GetAttributeValues(PubkeyAttribute)`,
   `ssh.ParseAuthorizedKey` each, drop unparseable lines with
   `pubkey_parse_errors_total{source=ldap}` increment. Return the
   non-empty key slice.
6. `*ldap.Error` with `ResultCode` in {ErrorNetwork, ErrorClosing} or
   `io.EOF` from the conn → set `conn = nil` (next call redials),
   return the error.

### 4.4 Filter escape

`escapeRFC4515(s string) string` replaces, in order:

- `\` → `\5c` (must be first to avoid double-escaping subsequent escapes)
- NUL → `\00`
- `(` → `\28`
- `)` → `\29`
- `*` → `\2a`

`UserFilter` template authors can freely use `{{.Username}}`; the
value substituted is already escape-safe.

### 4.5 Concurrency

- `connMu` serializes connection-level work (Bind + Search). LDAP conns
  are not goroutine-safe in `go-ldap/v3`.
- `cacheMu` is an RWMutex. Reads (the freshness check) take RLock; the
  rare write in step 4 takes Lock briefly.
- `singleflight.Group` collapses concurrent Resolves for the same
  username into one LDAP round-trip — important when many SSH clients
  hit the gateway at once after a fleet restart.

### 4.6 What is *not* implemented

- TCP keep-alives beyond Go's default. The conn is reused while it
  works; on the first network error we redial.
- LDAP referrals. `SearchRequest.SizeLimit=2` and a flat result mean
  referrals don't help us; we let `go-ldap` ignore them by default.
- LDAP paging. Single-user lookup never needs it.

---

## 5. Decision table — combined CRD × LDAP outcomes

| CRD has user? | CRD key matches? | LDAP has user? | LDAP key matches? | LDAP available? | Result | `AuthPath.Source` |
|---|---|---|---|---|---|---|
| yes | yes | — | — | — | ALLOW | crd |
| yes | no  | yes | yes | yes | ALLOW | ldap |
| yes | no  | yes | no  | yes | DENY `ErrPubkeyMismatch` | — |
| yes | no  | yes | yes | no, stale cache hit | ALLOW (served_stale) | ldap |
| yes | no  | —   | —   | no, no usable cache | DENY `ErrPubkeyMismatch` | — |
| no  | —   | yes | yes | yes | ALLOW | ldap |
| no  | —   | yes | no  | yes | DENY `ErrPubkeyMismatch` | — |
| no  | —   | no  | —   | yes | DENY `ErrUserNotFound` | — |
| no  | —   | —   | —   | no, no cache | DENY `ErrUserNotFound` | — |
| yes | yes | —   | —   | no  | ALLOW (LDAP never consulted) | crd |

Trusted-proxy auth bypasses this whole table — keyed on fingerprint
only.

---

## 6. Observability

### 6.1 Audit (`internal/gateway/audit.go`)

`AuthPath` gains:

- `Source string` — `"crd"`, `"ldap"`, or `""` (proxy).
- `ServedStale bool` — only meaningful when `Source == "ldap"`.

Audit log line emits one extra space-separated field
`source=<name> served_stale=true|false`.

### 6.2 Prometheus metrics (`internal/gateway/metrics.go`)

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `devpod_gateway_auth_attempts_total` | counter | `result={ok,denied,error}`, `source={crd,ldap,proxy,none}` | Existing counter; expanded label set. `source=none` = denied before any source resolved (e.g. ParseLoginName error). |
| `devpod_gateway_ldap_lookups_total` | counter | `outcome={hit,miss,error,cache_fresh,cache_stale}` | `hit` = live LDAP returned keys; `miss` = live LDAP returned 0 entries (negative). |
| `devpod_gateway_ldap_lookup_duration_seconds` | histogram | — | Live wire time only; cache hits not recorded. Buckets 1ms..5s. |
| `devpod_gateway_ldap_cache_entries` | gauge | `kind={positive,negative}` | Sampled from `len(cache)` lazily; precision is best-effort. |
| `devpod_gateway_ldap_connection_state` | gauge (0/1) | — | 1 when a bound conn is held; 0 when nil. |
| `devpod_gateway_ldap_pubkey_parse_errors_total` | counter | `source={crd,ldap}` | Bad lines in `spec.pubkeys` or in LDAP attribute values. |

### 6.3 Logging

`slog` only. LDAP failures: `level=warn` first occurrence inside a
1-minute window per `(username,errClass)`; subsequent within the
window drop to `debug`. Never log `bindPassword`, `caBytes`, or full
pubkey strings — pubkeys are logged as `ssh.FingerprintSHA256(key)`
only.

### 6.4 Startup fail-fast

The following make `cmd/gateway` exit non-zero (Pod CrashLoopBackOff):

- `spec.ldap.url` doesn't parse / isn't `ldaps://` (CEL would already
  reject at admission, but we keep the runtime check as
  defense-in-depth).
- CA Secret missing, missing key `ca.crt`, or PEM doesn't parse.
- Bind password Secret missing or missing key `password`.
- `UserFilter` template doesn't parse (rendered once with
  `Username="probe"` at startup to surface execute-time errors too).

LDAP server unreachable at startup does **not** fail-fast — the
gateway must serve CRD users even when LDAP is down.

---

## 7. Code changes (sketch)

```
api/v1alpha1/gatewayconfig_types.go         + LDAPSpec, + LDAP *LDAPSpec
api/v1alpha1/zz_generated.deepcopy.go       regen
config/crd/bases/devpod.io_gatewayconfigs.yaml  regen
deploy/chart/templates/crds/devpod.io_gatewayconfigs.yaml  sync

internal/gateway/identity.go                NEW — interface, crdSource
internal/gateway/identity_test.go           NEW
internal/gateway/ldap.go                    NEW
internal/gateway/ldap_test.go               NEW
internal/gateway/auth.go                    refactor — sources slice, loop
internal/gateway/auth_test.go               extend — multi-source cases
internal/gateway/audit.go                   + Source, ServedStale
internal/gateway/audit_test.go              extend
internal/gateway/metrics.go                 + 5 ldap metrics, expand auth labels
internal/gateway/metrics_test.go            extend

cmd/gateway/main.go                         wire LDAPSpec → NewLDAPSource
cmd/gateway/main_test.go                    + LDAP=nil and bad-Secret cases

deploy/chart/values.yaml                    + gateway.ldap subtree (off)
deploy/chart/templates/gatewayconfig.yaml   conditional spec.ldap render
deploy/chart/templates/role.yaml            + RBAC for ldap Secrets

hack/e2e-ldap.sh                            NEW

go.mod / go.sum                             + github.com/go-ldap/ldap/v3
```

No changes to `internal/controllers/`, `internal/render/`,
`internal/webhook/`, `cmd/supervisor/`, `cmd/controller/`,
`images/supervisor/`. That's the whole point of the §2 abstraction.

---

## 8. Test plan

### 8.1 Unit — `crdSource`

Direct table-driven cases in `internal/gateway/identity_test.go`:

- `User` not found → `ErrIdentityNotFound`.
- `User` found, all keys parse → keys returned.
- `User` found, one of three `spec.pubkeys` lines is garbage →
  remaining two returned, `pubkey_parse_errors_total{source=crd}` += 1.
- `client.Reader.Get` returns a real error → error surfaces (not
  `ErrIdentityNotFound`).

### 8.2 Unit — `ldapSource`

`internal/gateway/ldap_test.go` runs a real in-process LDAP server
backed by `github.com/jimlambrt/gldap` (BSD-3, used by HashiCorp Boundary;
supports real TLS), seeded via LDIF fixtures embedded in the test file.

Test setup mints a fresh self-signed CA, server cert/key, writes them
to a `t.TempDir()`, starts gldap on `127.0.0.1:0`, and points the
`ldapSource` at the resulting URL.

Cases:

- Single entry, single-value `sshPublicKey` → match.
- Single entry, three-value `sshPublicKey` → all three returned.
- Filter injection attempt: `Username = "alice)(uid=*"` → escaped to
  `alice\29\28uid=\2a`; entry not found; no second user leaks.
- Zero entries → `ErrIdentityNotFound`, negative cache populated,
  second call within `NegativeCacheTTL` doesn't dial.
- Bind failure → error returned, cache untouched.
- Network drop mid-search (stop the test server) + fresh cache →
  served from cache, `cache_fresh` metric.
- Network drop + cache age in StaleGrace + previous error → served
  stale, `cache_stale` metric, `ServedStale=true`.
- Network drop + cache age beyond StaleGrace → error returned.
- 50 concurrent `Resolve("alice")` → exactly 1 wire search
  (singleflight verification via a counter on the gldap handler).
- Reconnect: close conn server-side → next call redials and binds
  again (instrumented via gldap handler hit count).
- `ssh.ParseAuthorizedKey` rejects one of three values → other two
  returned, `pubkey_parse_errors_total{source=ldap}` += 1.

Injected `clock` controls cache freshness without `time.Sleep`.

### 8.3 Unit — `Authenticator` (multi-source)

`internal/gateway/auth_test.go` extends with:

- Stub `IdentitySource` implementations (no LDAP server) make these
  fast and deterministic.
- Cases mirror §5 decision table row-for-row.
- Trusted-proxy path retested — must continue to short-circuit before
  source resolution.

### 8.4 envtest controllers

No change. The `User` and `DevPod` controllers don't see LDAP. Run
the existing suite as a regression check.

### 8.5 CRD validation

`api/v1alpha1/gatewayconfig_types_test.go` (or a new
`_validation_test.go` if scope grows):

- `ldap.url=ldap://...` → CEL reject.
- `ldap.url=https://...` → CEL reject.
- `ldap.url=ldaps://host` and `ldaps://host:636` → admit.

### 8.6 e2e — `hack/e2e-ldap.sh` (new)

Requires `hack/e2e-up.sh` already brought a kind cluster + chart up.

1. Deploy an `openldap` Pod (`docker.io/bitnami/openldap:2.6`) with a
   ConfigMap-mounted LDIF that creates:
   - Service account `cn=devpod-svc,ou=System,dc=devpod,dc=test` with
     a known password.
   - User `uid=lalice,ou=People,dc=devpod,dc=test`,
     `sshPublicKey: <ed25519 line>` (key generated in the script).
2. Create Secrets `devpod-ldap-ca` (containing `ca.crt`) and
   `devpod-ldap-bind` (containing `password`).
3. Patch `GatewayConfig/default` with `spec.ldap.{url,bindDN,baseDN,
   caSecretRef,bindPasswordSecretRef}` pointing at the in-cluster
   service.
4. `kubectl -n devpod-system rollout restart deploy/devpod-gateway`
   (the gateway does not watch `GatewayConfig` for changes; §1 lists
   hot reload as a non-goal).
5. **CRD user path:** existing `alice` flow from `hack/e2e-v2.sh` —
   ssh succeeds, audit row has `source=crd`.
6. **LDAP user path:** create DevPod `dp-lalice` with
   `spec.owner=lalice` (no User CRD for lalice). ssh as
   `lalice+dp-lalice@gateway` using the LDAP-side key → succeeds,
   audit row has `source=ldap served_stale=false`.
7. **Fallback path:** seed LDAP with `uid=falice` carrying a second
   distinct `sshPublicKey`. Create User CRD `falice` whose
   `spec.pubkeys` lists only the FIRST (CRD-only) key. Create DevPod
   `dp-falice`. ssh using the LDAP-only key for `falice` → succeeds,
   audit row has `source=ldap` (CRD had the user but didn't match,
   LDAP picked it up).
8. **Soft-fail:** the e2e GatewayConfig overrides defaults to
   `cacheTTLSeconds: 10` and `staleGraceSeconds: 20` so the test
   completes in under a minute. Warm the cache with a successful
   `lalice` login. `kubectl scale deploy/openldap --replicas=0`. ssh
   as `lalice` within `staleGraceSeconds` → succeeds, audit row has
   `served_stale=true`. `sleep $((CacheTTL + StaleGrace + 5))` → ssh
   denied.
9. Cleanup: scale openldap back to 1, delete DevPods.

`hack/e2e-v2.sh`, `hack/e2e-v2-shells.sh`, `hack/e2e-m2.sh`,
`hack/e2e-m3.sh` continue to pass unchanged with `spec.ldap=nil`.

### 8.7 Regression gate

`bash hack/test.sh` + `bash hack/e2e-v2.sh` + `bash hack/e2e-ldap.sh`
+ `go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.0 run`
all green = ship.

---

## 9. Risks

- **LDAP library quality.** `go-ldap/ldap/v3` is the de facto choice
  but its API has rough edges (`*ldap.Error` vs `error`, no native
  pool). Containing all LDAP I/O inside `ldapSource` limits the blast
  radius if we later swap libraries.
- **Filter injection.** Defense is the §4.4 escape + the existing
  `[a-z0-9-]{1,32}` regex. Test §8.2 case "Filter injection attempt"
  is mandatory.
- **Cache poisoning across gateway replicas.** Each replica caches
  independently. A short positive TTL combined with the source-of-truth
  staying in LDAP makes this acceptable. Cross-replica consistency is
  not a goal.
- **DoS via unknown-name floods.** Negative cache + LDAP request
  timeout + (eventually) gateway-level rate limiting. The negative
  cache is the cheap first defense; rate limiting is out of scope.
- **`go-ldap` reverse TLS verification.** `tls.Config.ServerName` must
  match the URL's host. Test §8.2 covers a working TLS handshake with
  a fresh CA; misconfigured ServerName would fail handshake at startup
  if we dialed eagerly — since we dial lazily, the first auth attempt
  produces the error. Acceptable.
- **Two-replica deploy + LDAP outage:** both replicas serve from their
  own stale caches; a user who has never authed against replica B
  during freshness sees DENY on B. Documented; not solved here.

---

## 10. Open questions resolved during brainstorming

- **Where does LDAP config live?** Inside `GatewayConfig.spec.ldap`.
  No separate `IdentitySource` CRD — YAGNI until a second non-CRD
  source lands.
- **Static vs LDAP precedence?** CRD first, LDAP second; same
  `username` may legitimately appear in both, in which case CRD keys
  are tried first and LDAP keys are the fallback.
- **Are LDAP keys ever written to the User CRD?** No, never. The CRD
  stays a hand-curated static surface.
- **Do LDAP-only users need a User CRD?** No. `DevPod.spec.owner`
  accepts the bare username; whichever source proves the key wins.
- **Bind type?** Simple bind + LDAPS only. StartTLS / anonymous / SASL
  are not implemented.
- **Failure mode?** Soft-fail with stale-while-error, bounded by
  `StaleGraceSeconds`. Configurable per-tenant via the CRD knob.
- **Hot reload of LDAP params?** No. Restart the gateway Pod. Rolling
  deploy is already zero-downtime.
- **Multiple LDAP servers?** No. Operator points the URL at a VIP / LB
  if they want HA.
- **OIDC?** Out of scope, but the `IdentitySource` interface is
  intentionally small so adding one later is a `NewOIDCSource` +
  append.
