# DevPod M3 — Multi-replica gateway, PROXY protocol, trusted proxies

**Status:** Approved
**Date:** 2026-05-12
**Depends on:** `2026-05-12-devpod-design.md` (umbrella spec);
M0+M1+M2 already merged.

---

## 1. Goal

Deliver the M3 milestone from the umbrella spec:

> Multi-replica gateway, PROXY protocol, trusted proxies: gateway
> becomes a stateless multi-replica Deployment sharing host and
> internal keys via Secrets; PROXY protocol v2 listener with
> `trustedCIDRs`; `trustedProxyKeys` enforced; audit `auth_path`
> field populated.

Concretely M3 adds:

1. The gateway scales horizontally (chart default → 2 replicas).
2. PROXY protocol v2 support (**opt-in via `listen.proxyProtocol.enabled`**;
   default off so existing single-replica deployments behave identically).
3. Trusted-proxy authentication: connections that present a key in
   `spec.trustedProxyKeys` bypass the `User.spec.pubkeys` check and
   may impersonate any *existing* User.
4. Structured audit logging (slog/JSON) with `auth_path` and the field
   set listed in spec §8.
5. Prometheus metrics (`sessions_total`, `dial_failures_total`,
   `auth_failures_total`, `session_duration_seconds`).

Non-goals (deferred):

- Per-user proxy allow-lists
  (`TrustedProxyKey.allowedUsers`) — umbrella spec §10 already defers.
- `devpod_gateway_bytes_total` metric — umbrella spec marks optional;
  skipped to avoid the byte-counting cost on every conn.
- Connection draining on rolling update — replicas stop accepting new
  connections immediately on SIGTERM and in-flight sessions get
  dropped. Documented; not mitigated in M3.
- Live `GatewayConfig` re-watch for `trustedProxyKeys` — startup-only
  index is enough for v1; operator restarts gateway to roll keys.

---

## 2. API additions

None. The schema for `listen.proxyProtocol.{enabled, trustedCIDRs}`
and `trustedProxyKeys[].{alias, pubkey}` was added in M0 and is
unused until M3.

---

## 3. Multi-replica gateway

### 3.1 What changes

- `deploy/chart/values.yaml`: `gateway.replicas` default flipped from
  `1` to `2`. Operator-overridable.
- `deploy/chart/templates/gateway.yaml`: explicit rolling-update
  strategy with `maxSurge: 1` and `maxUnavailable: 0`. New replicas
  start before old ones drain.
- No leader election, no state coordination. Each replica:
  - Mounts the same host-key Secret + internal-key Secret.
  - Runs its own informer cache for `User`, `DevPod`, `GatewayConfig`.
  - Accepts TCP, authenticates, dials backend, proxies SSH.
  - Patches `DevPod.status.lastActivityTime` on session events.

### 3.2 Concurrency

`status.lastActivityTime` patching uses `client.MergeFrom(dp)` so the
JSON merge-patch only sets the one field. Two replicas writing the
same field land "last-write-wins"; this is acceptable for an activity
timestamp where ordering within a second doesn't matter.

`status.endpoint` / `status.phase` are written by the *controller*,
not the gateway, so no contention.

### 3.3 Session affinity

Not required. The Service is plain ClusterIP/LoadBalancer with default
kube-proxy round-robin. Each SSH connection — once accepted — stays
on the replica that accepted it. New connections from the same client
may land on a different replica, which is fine because state is in
`DevPod.status`, not in-memory.

### 3.4 Rolling update behavior

On `kubectl rollout restart deploy/devpod-gateway`:

- New pod starts, becomes Ready, joins the Service endpoints.
- Old pod gets SIGTERM, fails its readiness check (cookie file), is
  removed from endpoints. New connections route to the new pod.
- The old pod's `ssh.NewServerConn`s keep running until the existing
  TCP conns close or the pod is force-killed by terminationGracePeriod.
  In-flight sessions on the old pod see a TCP RST when the pod
  finally exits.

This is the "umbrella spec §1 non-goal: Server-side SSH session
migration across gateway restarts" — we explicitly don't migrate.

---

## 4. PROXY protocol v2 (opt-in)

### 4.1 Toggle

`GatewayConfig.spec.listen.proxyProtocol.enabled` is the on/off switch.
**Default false.** When false, the gateway listens with plain
`net.Listen` exactly as it does today; PROXY support adds zero code
path overhead.

When true, `trustedCIDRs` MUST be non-empty
(`+kubebuilder:validation:XValidation` already enforces this on the
schema).

### 4.2 Implementation

Use `github.com/pires/go-proxyproto` (BSD-3, well-maintained).

In `cmd/gateway/main.go`:

```go
listener, err := net.Listen("tcp", addr)
if err != nil { ... }

if gw.Spec.Listen.ProxyProtocol.Enabled {
    cidrs, err := parseCIDRs(gw.Spec.Listen.ProxyProtocol.TrustedCIDRs)
    if err != nil { ... }
    listener = wrapProxyProtocol(listener, cidrs)
}
```

`wrapProxyProtocol` lives in `internal/gateway/proxyproto.go`:

```go
func wrapProxyProtocol(inner net.Listener, trusted []*net.IPNet) net.Listener {
    return &proxyproto.Listener{
        Listener: inner,
        Policy: func(remote net.Addr) (proxyproto.Policy, error) {
            ip := remoteIP(remote)
            for _, n := range trusted {
                if n.Contains(ip) {
                    return proxyproto.USE, nil  // expect a PROXY header
                }
            }
            return proxyproto.REJECT, nil       // no PROXY from this source
        },
        ReadHeaderTimeout: 5 * time.Second,
    }
}
```

Behavior:
- Source in `trustedCIDRs` → PROXY header expected; once parsed,
  `conn.RemoteAddr()` returns the real client IP for downstream
  logging and metrics.
- Source NOT in `trustedCIDRs` → connection rejected at the listener
  layer. Fail-secure: if you can't tell whether the upstream is
  proxying, you can't safely accept either way.

The reject policy is the v1 simplification. A later refinement could
allow raw SSH from non-trusted sources alongside PROXY from trusted
ones, but that complicates the listener and lets attackers bypass
trustedCIDRs by simply not sending a header. v1 says: if PP is on,
the LB is the only entrypoint.

### 4.3 What gets the real IP

Every consumer of "client IP" must use `conn.RemoteAddr()`:
- `audit.SessionOpen("client_ip", conn.RemoteAddr().String())`
- `metrics.AuthFailures.WithLabelValues("...", "...").Inc()` — labels
  don't include IP (high-cardinality), but the failure log does.

---

## 5. Trusted-proxy authentication

### 5.1 Index built at startup

`cmd/gateway/main.go` builds `proxyKeyIndex map[string]string` —
`SHA256 fingerprint → alias` — from
`GatewayConfig.spec.trustedProxyKeys`:

```go
proxyKeyIndex := map[string]string{}
for _, k := range gw.Spec.TrustedProxyKeys {
    pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Pubkey))
    if err != nil {
        die(err, "trustedProxyKey "+k.Alias)
    }
    proxyKeyIndex[ssh.FingerprintSHA256(pk)] = k.Alias
}
```

Static for the lifetime of the process. Rolling the keys = `kubectl
rollout restart deploy/devpod-gateway`. Live re-watch is a v1+
nicety, not in scope.

### 5.2 PublicKeyCallback

The current callback parses `<user>+<pod>` from `conn.User()`, looks
up the User CR by name, tries each `pubkey` against the offered key.
M3 extends this to check the trusted-proxy index first.

Pseudocode:

```go
PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
    fp := ssh.FingerprintSHA256(key)
    user, pod, err := parseLogin(conn.User())
    if err != nil {
        return nil, err  // bad login format
    }

    // Trusted-proxy check.
    if alias, ok := proxyKeyIndex[fp]; ok {
        if _, err := userCache.Get(user); err != nil {
            audit.AuthFailure(conn, "user_not_found", "trusted_proxy", alias, fp)
            return nil, errUserUnknown
        }
        return permsFor(AuthPath{Kind: "trusted_proxy", Alias: alias, User: user, Pod: pod}, fp), nil
    }

    // Direct user pubkey check.
    userCR, err := userCache.Get(user)
    if err != nil {
        audit.AuthFailure(conn, "user_not_found", "direct", "", fp)
        return nil, errUserUnknown
    }
    for _, pkLine := range userCR.Spec.Pubkeys {
        if matchesKey(pkLine, key) {
            return permsFor(AuthPath{Kind: "direct", User: user, Pod: pod}, fp), nil
        }
    }
    audit.AuthFailure(conn, "key_mismatch", "direct", "", fp)
    return nil, errKeyMismatch
}
```

`AuthPath` rides through to the session-open phase via
`ssh.Permissions.Extensions["devpod.io/auth-path"]` (JSON-encoded);
the proxy.go layer pulls it for the audit log.

### 5.3 Ownership check unchanged

Same as M1: after authentication, the gateway looks up
`DevPod devpods/<pod>` and verifies
`spec.owner == authPath.User || authPath.User in spec.collaborators`.
Direct and trusted-proxy auth go through this gate identically.

A trusted proxy that claims `alice+bob-pod` will be authenticated as
alice (proxy is trusted) but then fail the ownership check because
bob owns bob-pod, not alice.

---

## 6. Audit logging — slog JSON

### 6.1 Top-level switch

`cmd/gateway/main.go` builds a `*slog.Logger` with `JSONHandler` on
stderr:

```go
logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelInfo,
}))
slog.SetDefault(logger)
```

`internal/gateway/audit.go` (NEW) wraps slog with field-set
helpers so callers don't repeat key names:

```go
package gateway

// AuthPath records how the connection was authenticated.
type AuthPath struct {
    Kind  string // "direct" | "trusted_proxy"
    Alias string // proxy alias when Kind=="trusted_proxy"
    User  string
    Pod   string
}

// SessionOpen emits a structured event for a new authenticated session.
func SessionOpen(logger *slog.Logger, sessionID string, ap AuthPath, clientIP, pubkeyFP string) { ... }

// SessionClose emits the close event with byte counts / duration.
func SessionClose(logger *slog.Logger, sessionID string, dur time.Duration, bytesIn, bytesOut int64, reason string) { ... }

// AuthFailure emits a denied-auth event.
func AuthFailure(logger *slog.Logger, conn ssh.ConnMetadata, reason, authPath, alias, pubkeyFP string) { ... }
```

Fields match umbrella spec §8:

```
{ts, session_id, user, devpod, auth_path, pubkey_fingerprint,
 client_ip, bytes_in, bytes_out, duration_seconds, close_reason}
```

`ts` is automatic via slog. The other fields are passed in by the
caller.

### 6.2 Migrate existing text logs

Every `fmt.Fprintf(os.Stderr, ...)` in `cmd/gateway/main.go` and
`internal/gateway/proxy.go` is replaced by `logger.Info(...)` /
`logger.Warn(...)` calls. The keys are kept stable so log shipping
is straightforward.

Existing log lines and their successors:
- `accept: id=N from=...` → `logger.Info("accept", "id", n, "from", remote)`
- `auth-ok: id=N user=... pod=... endpoint=...` →
  `audit.SessionOpen(logger, id, ap, clientIP, fp)`
- `auth-rejected: id=N login=... reason=...` →
  `audit.AuthFailure(...)`
- `proxy-start: id=N`, `proxy-end: id=N err=...` → kept as plain
  Info events alongside the structured SessionOpen/Close pair.

### 6.3 Bytes counted

To populate `bytes_in` / `bytes_out` on `SessionClose`, the data
pumps in `internal/gateway/proxy.go` `proxyChannel` swap their raw
`io.Copy(rch, lch)` for an `io.Copy(rch, &countingReader{lch, &in})`
or similar. Two `int64` counters keyed by direction.

The bytes per-channel aggregate up to the connection level via a
small `connStats` struct held by the top-level `Proxy`. M3 is the
right time to introduce it because Prometheus metric labels by
direction will also use it.

---

## 7. Prometheus metrics

### 7.1 Metrics

`internal/gateway/metrics.go` (NEW) declares:

```go
var (
    sessionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "devpod_gateway_sessions_total",
        Help: "Number of authenticated SSH sessions.",
    }, []string{"user", "devpod", "auth_path", "result"})

    dialFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "devpod_gateway_dial_failures_total",
        Help: "Failures dialing backend sshd.",
    }, []string{"devpod", "reason"})

    authFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "devpod_gateway_auth_failures_total",
        Help: "Authentication failures by reason.",
    }, []string{"reason", "auth_path"})

    sessionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name: "devpod_gateway_session_duration_seconds",
        Help: "Duration of an authenticated session.",
        Buckets: prometheus.ExponentialBuckets(1, 4, 8), // 1s..16384s
    }, []string{"user", "devpod", "auth_path"})
)
```

Note: `bytes_total` is explicitly omitted (umbrella spec §8 marks it
optional).

### 7.2 Endpoint

The gateway already exposes a metrics endpoint via
controller-runtime; M3 just hooks the registrations in `init()` of
`internal/gateway/metrics.go` against
`controllerruntimemetrics.Registry`. The Helm chart's existing
`port: metrics` survives unchanged.

---

## 8. Test plan

### 8.1 Unit

- `internal/gateway/proxyproto_test.go` (new): feed a v2 header
  to a fake net.Conn → verify parsed RemoteAddr; verify
  non-trusted source is REJECTed.
- `internal/gateway/auth_test.go` (new — extracts PublicKeyCallback
  into a testable seam): trusted-proxy key match → AuthPath
  `trusted_proxy`; user pubkey match → `direct`; unknown key →
  `errKeyMismatch`; trusted proxy + missing User → `errUserUnknown`.
- `internal/gateway/audit_test.go` (new): SessionOpen/Close/AuthFailure
  emit the right field set; JSON output round-trips.

### 8.2 envtest

(controllers/devpod is unaffected by M3; gateway is integration-tested
via the OrbStack flow in §8.3.)

### 8.3 OrbStack e2e — `hack/e2e-m3.sh`

Three flows:

1. **Multi-replica round-robin**:
   - Patch `gateway.replicas` to 2.
   - Run 10 `ssh -- uname -a` connections in parallel.
   - Assert at least two distinct replica pod names appear in
     `accept` log lines (`kubectl logs -l app=devpod-gateway --all-containers --prefix=true`).

2. **PROXY v2**:
   - Patch `GatewayConfig.spec.listen.proxyProtocol.enabled=true` +
     `trustedCIDRs=[127.0.0.0/8]`.
   - Open a TCP conn to gateway and write a v2 header claiming
     `1.2.3.4:5678` as source.
   - Read the SSH banner.
   - Verify a recent log entry has `"client_ip":"1.2.3.4:5678"`.

3. **Trusted proxy**:
   - Generate a key pair `proxy-test.ed25519`.
   - Patch `GatewayConfig.spec.trustedProxyKeys=[{alias:"e2e", pubkey:<proxy-test.pub>}]`.
   - Restart gateway pods (kubectl rollout restart).
   - `ssh -i proxy-test alice+m2demo@gw -- uname -a` (alice's user
     pubkey is NOT this key).
   - Expect success and a log entry
     `"auth_path":"trusted_proxy","proxy_alias":"e2e"`.

---

## 9. Files touched (sketch)

```
cmd/gateway/main.go                     (slog setup, proxyproto wrap, auth callback rewrite, metrics register)
internal/gateway/auth.go                NEW  (PublicKeyCallback factory)
internal/gateway/audit.go               NEW  (SessionOpen/Close/AuthFailure)
internal/gateway/metrics.go             NEW  (Prometheus collectors)
internal/gateway/proxyproto.go          NEW  (listener wrapper)
internal/gateway/proxy.go               (counting reader; pass logger; emit close event)
deploy/chart/values.yaml                (gateway.replicas: 2)
deploy/chart/templates/gateway.yaml     (rolling strategy maxUnavailable=0 / maxSurge=1)
hack/e2e-m3.sh                          NEW
```

Plus tests:
```
internal/gateway/auth_test.go           NEW
internal/gateway/audit_test.go          NEW
internal/gateway/proxyproto_test.go     NEW
internal/gateway/metrics_test.go        (optional — collectors register cleanly)
```

---

## 10. Open questions answered during brainstorming

- Q: Trusted proxy require User CR existence? **A:** Yes. Returns
  `errUserUnknown` if missing; logged via `audit.AuthFailure`.
- Q: PROXY protocol always on? **A:** No — opt-in via
  `listen.proxyProtocol.enabled`. Default false. When disabled the
  listener is plain `net.Listen`; behavior identical to M1/M2.
- Q: Audit log format? **A:** Migrate everything in `cmd/gateway` and
  `internal/gateway` to `slog.JSONHandler`. Audit events become
  thin wrapper functions in `internal/gateway/audit.go` with the
  fields fixed by umbrella spec §8.
- Q: bytes_total metric? **A:** Defer; spec already marked optional.
  Bytes are recorded into the per-session SessionClose event though,
  so debugging is still possible from logs.
