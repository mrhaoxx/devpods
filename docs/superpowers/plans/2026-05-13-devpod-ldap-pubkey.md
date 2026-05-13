# LDAP-pubkey-source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add external LDAP as a secondary `IdentitySource` queried after the `User` CRD when resolving SSH authorized keys. CRD path stays unchanged in the happy case; LDAP-backed users authenticate without any User CRD; same-username CRD/LDAP coexistence falls back from CRD to LDAP on key mismatch; LDAP outages serve from stale cache within a grace window.

**Architecture:** `internal/gateway` gains a small `IdentitySource` interface, two implementations (`crdSource`, `ldapSource`), and an Authenticator that iterates sources in injected order. The LDAP source owns its own go-ldap connection, in-memory TTL cache, RFC 4515 filter escape, and singleflight collapse. Secrets reach the gateway via Helm-rendered volume mounts (matching the existing `HostKeyRef` / `InternalKeyRef` pattern); the CRD `caSecretRef` / `bindPasswordSecretRef` fields are how the chart knows what to mount.

**Tech Stack:** Go 1.25, controller-runtime v0.20, `github.com/go-ldap/ldap/v3` (runtime), `github.com/jimlambrt/gldap` + `golang.org/x/sync/singleflight` (singleflight is already used in the gateway; gldap is test-only), kubebuilder CRD generation.

**Spec reference:** `docs/superpowers/specs/2026-05-13-devpod-ldap-pubkey.md`.

---

## File map

**Create**
- `internal/gateway/identity.go` — `IdentitySource` interface + `crdSource`
- `internal/gateway/identity_test.go`
- `internal/gateway/ldap.go` — `ldapSource` (construction, cache, live lookup, singleflight, reconnect)
- `internal/gateway/ldap_test.go` — covers all of the above with an in-process LDAPS test server
- `internal/gateway/ldap_escape.go` — RFC 4515 escape helper (separate file to keep `ldap.go` focused)
- `internal/gateway/ldap_escape_test.go`
- `hack/e2e-ldap.sh` — e2e against an in-cluster OpenLDAP

**Modify**
- `api/v1alpha1/gatewayconfig_types.go` — add `LDAPSpec` + `LDAP *LDAPSpec`
- `api/v1alpha1/zz_generated.deepcopy.go` — regen
- `config/crd/bases/devpod.io_gatewayconfigs.yaml` — regen
- `deploy/chart/templates/crds/devpod.io_gatewayconfigs.yaml` — sync (via `hack/sync-crd-chart`)
- `internal/gateway/auth.go` — refactor to consult `[]IdentitySource`
- `internal/gateway/auth_test.go` — decision-table cases over the new flow
- `internal/gateway/audit.go` — add `Source`, `ServedStale`; emit them on `session_open`
- `internal/gateway/audit_test.go`
- `internal/gateway/metrics.go` — new LDAP metrics; widen existing `AuthFailuresTotal` consumers
- `internal/gateway/metrics_test.go`
- `cmd/gateway/main.go` — build `[]IdentitySource`, fail-fast on disk Secrets
- `cmd/gateway/main_test.go` — LDAP=nil and bad-disk-Secret startup cases
- `deploy/chart/values.yaml` — `gateway.ldap` subtree
- `deploy/chart/templates/gateway.yaml` — conditional volumes for LDAP Secrets
- `deploy/chart/templates/gatewayconfig.yaml` — render `spec.ldap` when enabled
- `go.mod` / `go.sum` — `github.com/go-ldap/ldap/v3`, `github.com/jimlambrt/gldap` (test)

---

## Task 1: CRD `LDAPSpec` + `GatewayConfig.spec.ldap` + regen

**Files:**
- Modify: `api/v1alpha1/gatewayconfig_types.go`
- Modify (regen): `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/devpod.io_gatewayconfigs.yaml`, `deploy/chart/templates/crds/devpod.io_gatewayconfigs.yaml`

- [ ] **Step 1: Add the `LDAPSpec` struct**

Edit `api/v1alpha1/gatewayconfig_types.go`. Immediately above the `GatewayConfigSpec` struct declaration (the line `type GatewayConfigSpec struct {`), insert:

```go
// LDAPSpec configures a single external LDAP identity source queried
// after the User CRD source. Disabled when GatewayConfigSpec.LDAP is
// nil.
type LDAPSpec struct {
	// URL is the LDAP server URL. Only ldaps:// is supported
	// (plaintext ldap:// is refused at admission).
	//
	// +kubebuilder:validation:Pattern=`^ldaps://[^/\s]+(:[0-9]+)?$`
	URL string `json:"url"`

	// CASecretRef points to a Secret whose key "ca.crt" holds the
	// PEM-encoded LDAP server CA bundle. The system trust store is
	// never consulted, so a CA rotation here is an explicit,
	// observable event rather than a silent drift.
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
	// rendered against {.Username string}. Username has already
	// passed the DevPod login-name regex ([a-z0-9-]{1,32}); the
	// gateway additionally RFC 4515-escapes it before substitution
	// as defense-in-depth.
	//
	// Default: `(&(objectClass=posixAccount)(uid={{.Username}}))`
	//
	// +optional
	UserFilter string `json:"userFilter,omitempty"`

	// PubkeyAttribute is the LDAP attribute that carries
	// OpenSSH-format authorized_keys lines. Defaults to
	// "sshPublicKey" (the OpenSSH schema OID
	// 1.3.6.1.4.1.24552.500.1.1.1.13). Multi-valued attributes are
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

	// NegativeCacheTTLSeconds is the lifetime of a "this user is not
	// in LDAP" cache entry.
	//
	// +optional
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=0
	NegativeCacheTTLSeconds int32 `json:"negativeCacheTTLSeconds,omitempty"`

	// StaleGraceSeconds is the soft-fail window. When LDAP is
	// currently failing, a cache entry whose age is between
	// CacheTTLSeconds and CacheTTLSeconds+StaleGraceSeconds is still
	// served (audit: served_stale=true). Beyond that, the entry is
	// treated as evicted.
	//
	// +optional
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=0
	StaleGraceSeconds int32 `json:"staleGraceSeconds,omitempty"`
}
```

- [ ] **Step 2: Add `LDAP *LDAPSpec` to `GatewayConfigSpec`**

In the same file, inside `GatewayConfigSpec`, immediately after the `Banner string` field (currently the last field), insert:

```go
	// LDAP, when non-nil, registers a secondary identity source
	// queried after the User CRD source. Disabled when nil.
	//
	// +optional
	LDAP *LDAPSpec `json:"ldap,omitempty"`
```

- [ ] **Step 3: Regenerate deepcopy + CRD + chart-mirrored CRD**

Run:
```bash
go generate ./api/v1alpha1/...
```

Expected: three files change. `zz_generated.deepcopy.go` adds a `LDAPSpec.DeepCopyInto` / `GatewayConfigSpec.DeepCopyInto` extension (the pointer field needs a nil-check copy). `config/crd/bases/devpod.io_gatewayconfigs.yaml` gains the `ldap` property with the URL pattern, enum-less defaults, etc. `deploy/chart/templates/crds/devpod.io_gatewayconfigs.yaml` mirrors it (existing `hack/sync-crd-chart` step is wired into `go generate`).

- [ ] **Step 4: Verify CRD output**

Run:
```bash
grep -A 5 'ldap:' config/crd/bases/devpod.io_gatewayconfigs.yaml | head -20
```
Expected: includes `properties:` with `url:`, the `pattern: ^ldaps://...` line, and the nested SecretRef shape.

- [ ] **Step 5: Compile-check**

Run:
```bash
go build ./...
```
Expected: no output, exit 0. (No call sites use `LDAPSpec` yet — this only verifies the type compiles.)

- [ ] **Step 6: Commit**

```bash
git add api/v1alpha1/gatewayconfig_types.go \
        api/v1alpha1/zz_generated.deepcopy.go \
        config/crd/bases/devpod.io_gatewayconfigs.yaml \
        deploy/chart/templates/crds/devpod.io_gatewayconfigs.yaml
git commit -m "api/v1alpha1: GatewayConfig.spec.ldap — LDAPSpec scaffolding"
```

---

## Task 2: `IdentitySource` interface + `crdSource` (TDD)

**Files:**
- Create: `internal/gateway/identity.go`
- Create: `internal/gateway/identity_test.go`

The extraction is a pure refactor: today's `c.Get(User) → parse pubkeys → match` logic moves out of `Authenticator.Authenticate` into `crdSource.Resolve`. Authenticator wiring lands in Task 3.

- [ ] **Step 1: Write the failing test (interface + crdSource happy/notfound/garbage)**

Create `internal/gateway/identity_test.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

func TestCRDSource_NotFound_ReturnsIdentityNotFound(t *testing.T) {
	src := gateway.NewCRDSource(fakeClient(t))
	_, err := src.Resolve(context.Background(), "ghost")
	if !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("err = %v, want ErrIdentityNotFound", err)
	}
}

func TestCRDSource_FoundReturnsParsedKeys(t *testing.T) {
	_, lineA := ed25519Pubkey(t)
	_, lineB := ed25519Pubkey(t)
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{lineA, lineB}},
	}
	src := gateway.NewCRDSource(fakeClient(t, u))
	keys, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len keys = %d, want 2", len(keys))
	}
}

func TestCRDSource_SkipsUnparseableLines(t *testing.T) {
	_, line := ed25519Pubkey(t)
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec: devpodv1alpha1.UserSpec{
			Pubkeys: []string{line, "not a key", "also-garbage"},
		},
	}
	src := gateway.NewCRDSource(fakeClient(t, u))
	keys, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("len keys = %d, want 1 (garbage lines dropped)", len(keys))
	}
}

func TestCRDSource_PassesThroughRealError(t *testing.T) {
	src := gateway.NewCRDSource(boomReader{})
	_, err := src.Resolve(context.Background(), "alice")
	if err == nil || errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("err = %v, want non-NotFound transport error", err)
	}
}

func TestCRDSource_Name(t *testing.T) {
	src := gateway.NewCRDSource(fakeClient(t))
	if got := src.Name(); got != "crd" {
		t.Errorf("Name() = %q, want %q", got, "crd")
	}
}

// boomReader is a client.Reader that returns a non-NotFound error on
// every call, simulating an apiserver transport problem.
type boomReader struct{}

func (boomReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return errReaderBoom
}
func (boomReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return errReaderBoom
}

var errReaderBoom = errBoom{}

type errBoom struct{}

func (errBoom) Error() string { return "transport boom" }

// Quiet "unused" linter warning if we later trim a test.
var _ = bytes.Equal
```

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/gateway/ -run TestCRDSource -v
```
Expected: FAIL — `undefined: gateway.NewCRDSource`, `undefined: gateway.ErrIdentityNotFound`.

- [ ] **Step 3: Implement `IdentitySource` + `crdSource`**

Create `internal/gateway/identity.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// ErrIdentityNotFound — the source does not know this username.
// Callers should try the next source in the chain. This is not an
// error in the log-it-and-page sense; it's the normal "I have nothing
// for this name" signal.
var ErrIdentityNotFound = errors.New("identity not found in this source")

// IdentitySource resolves a username to a set of authorized SSH
// public keys. Implementations do NOT compare keys; that stays in
// Authenticator so the comparison and the cross-source ordering live
// in one place.
type IdentitySource interface {
	Resolve(ctx context.Context, username string) ([]ssh.PublicKey, error)
	Name() string
}

// NewCRDSource returns an IdentitySource backed by the User CRD.
// r is typically the controller-runtime cache client; the User CRD
// is cluster-scoped so no namespace argument is needed.
func NewCRDSource(r client.Reader) IdentitySource {
	return &crdSource{r: r}
}

type crdSource struct {
	r client.Reader
}

func (s *crdSource) Name() string { return "crd" }

func (s *crdSource) Resolve(ctx context.Context, username string) ([]ssh.PublicKey, error) {
	var u devpodv1alpha1.User
	if err := s.r.Get(ctx, types.NamespacedName{Name: username}, &u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrIdentityNotFound
		}
		return nil, fmt.Errorf("crd get user %q: %w", username, err)
	}
	keys := make([]ssh.PublicKey, 0, len(u.Spec.Pubkeys))
	for _, line := range u.Spec.Pubkeys {
		parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
		if perr != nil {
			PubkeyParseErrorsTotal.WithLabelValues("crd").Inc()
			continue
		}
		keys = append(keys, parsed)
	}
	return keys, nil
}
```

`PubkeyParseErrorsTotal` does not exist yet — it lands in Task 5. **Temporarily** comment out that line for this task and uncomment in Task 5; OR, simpler: omit the metric increment here and add it back in Task 5's edit. We omit:

Replace the `for _, line := range u.Spec.Pubkeys` loop with the version below (no metric reference yet):

```go
	for _, line := range u.Spec.Pubkeys {
		parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
		if perr != nil {
			continue
		}
		keys = append(keys, parsed)
	}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/gateway/ -run TestCRDSource -v
```
Expected: all 5 cases PASS.

- [ ] **Step 5: Confirm the existing auth_test suite still passes (no regressions)**

```bash
go test ./internal/gateway/ -v
```
Expected: ALL existing tests still PASS — `crdSource` is unused by `Authenticator` yet, just new code.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/identity.go internal/gateway/identity_test.go
git commit -m "internal/gateway: IdentitySource interface + crdSource"
```

---

## Task 3: Refactor `Authenticator` to use `[]IdentitySource` (behavior-preserving)

**Files:**
- Modify: `internal/gateway/auth.go`
- Modify: `internal/gateway/auth_test.go` (only adds `.WithSources(...)` setup; no new cases yet)

Goal: introduce the sources slice, wire `crdSource` as the sole default source so the existing auth path behaves identically. New decision-table cases come in Task 11.

- [ ] **Step 1: Add `WithSources` and refactor `Authenticate`**

Edit `internal/gateway/auth.go`. Replace the entire file with:

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
	ErrPubkeyMismatch  = errors.New("pubkey does not match any identity source")
	ErrDevPodNotFound  = errors.New("devpod not found")
	ErrDevPodNotReady  = errors.New("devpod not running")
	ErrAccessDenied    = errors.New("access denied: not owner or collaborator")
	ErrLoginNameFormat = errors.New("login name must be <user>+<pod>")
)

// AuthResult is what Authenticate returns on success.
type AuthResult struct {
	User       string
	DevPodName string
	Endpoint   string
	AuthPath   AuthPath
}

// Authenticator validates SSH client public keys against an ordered
// chain of IdentitySources and resolves the target DevPod.
type Authenticator struct {
	c         client.Reader
	dpNS      string
	proxyKeys map[string]string // SHA256 fingerprint → alias
	sources   []IdentitySource  // ordered; first match wins
}

// NewAuthenticator returns an Authenticator. Call WithSources to
// register identity sources; if none are registered, the legacy
// User-CRD-only path is used automatically (so existing call sites
// don't break).
func NewAuthenticator(c client.Reader, devpodNamespace string) *Authenticator {
	return &Authenticator{c: c, dpNS: devpodNamespace}
}

// WithProxyKeys attaches the trusted-proxy index. Pass nil/empty to
// disable trusted-proxy auth.
func (a *Authenticator) WithProxyKeys(idx map[string]string) *Authenticator {
	a.proxyKeys = idx
	return a
}

// WithSources installs the ordered identity-source chain. Sources are
// tried in slice order; the first one to authorize the offered key
// wins.
func (a *Authenticator) WithSources(srcs []IdentitySource) *Authenticator {
	a.sources = srcs
	return a
}

// Authenticate validates the offered SSH key for loginName and
// resolves the target DevPod.
func (a *Authenticator) Authenticate(ctx context.Context, loginName string, key ssh.PublicKey) (*AuthResult, error) {
	user, pod, err := ParseLoginName(loginName)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoginNameFormat, err)
	}

	// Fetch DevPod up front so a bad pod name fails fast regardless
	// of identity-source latency.
	var dp devpodv1alpha1.DevPod
	if err := a.c.Get(ctx, types.NamespacedName{Name: pod, Namespace: a.dpNS}, &dp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q in ns %q", ErrDevPodNotFound, pod, a.dpNS)
		}
		return nil, fmt.Errorf("get devpod: %w", err)
	}

	ap := AuthPath{User: user, Pod: pod}

	// Trusted-proxy short-circuit — fingerprint-only, identity
	// sources not consulted.
	if alias, ok := a.proxyKeys[ssh.FingerprintSHA256(key)]; ok {
		ap.Kind = "trusted_proxy"
		ap.Alias = alias
		return a.finalize(&dp, user, pod, ap)
	}

	// Resolve via ordered sources. Default to a single in-process
	// crdSource if WithSources was never called, so legacy callers
	// keep working.
	sources := a.sources
	if len(sources) == 0 {
		sources = []IdentitySource{NewCRDSource(a.c)}
	}

	ap.Kind = "direct"
	matched := false
	anyKnown := false
	for _, src := range sources {
		keys, rerr := src.Resolve(ctx, user)
		if errors.Is(rerr, ErrIdentityNotFound) {
			continue
		}
		if rerr != nil {
			// Real failure on this source — record and continue.
			ap.LastSourceErr = src.Name() + ": " + rerr.Error()
			continue
		}
		anyKnown = true
		if matchesAnyParsed(key, keys) {
			ap.Source = src.Name()
			ap.ServedStale = sourceServedStale(src)
			matched = true
			break
		}
		// Source knew this user but the key didn't match — fall
		// through to the next source (the "static-first, LDAP-
		// fallback" contract).
	}
	if !matched {
		if anyKnown {
			return nil, fmt.Errorf("%w: user %q", ErrPubkeyMismatch, user)
		}
		return nil, fmt.Errorf("%w: %q", ErrUserNotFound, user)
	}

	return a.finalize(&dp, user, pod, ap)
}

// finalize runs the shared post-match checks (access + readiness).
func (a *Authenticator) finalize(dp *devpodv1alpha1.DevPod, user, pod string, ap AuthPath) (*AuthResult, error) {
	if !accessAllowed(dp, user) {
		return nil, fmt.Errorf("%w: user %q on devpod %q", ErrAccessDenied, user, pod)
	}
	if dp.Status.Phase != devpodv1alpha1.DevPodRunning || dp.Status.Endpoint == "" {
		return nil, fmt.Errorf("%w: %q phase=%q endpoint=%q", ErrDevPodNotReady, pod, dp.Status.Phase, dp.Status.Endpoint)
	}
	return &AuthResult{
		User:       user,
		DevPodName: pod,
		Endpoint:   dp.Status.Endpoint,
		AuthPath:   ap,
	}, nil
}

// accessAllowed returns true if user is the owner or a collaborator.
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

// matchesAnyParsed returns true if any pre-parsed pubkey in keys
// equals offered.
func matchesAnyParsed(offered ssh.PublicKey, keys []ssh.PublicKey) bool {
	want := offered.Marshal()
	for _, k := range keys {
		if bytes.Equal(k.Marshal(), want) {
			return true
		}
	}
	return false
}

// sourceServedStale lets sources advertise that they served from a
// stale cache during the last Resolve. Sources that don't implement
// the optional StaleAware interface report false.
func sourceServedStale(s IdentitySource) bool {
	sa, ok := s.(StaleAware)
	if !ok {
		return false
	}
	return sa.LastServedStale()
}

// StaleAware is an optional interface for sources whose Resolve may
// return data from a stale-while-error cache (e.g. ldapSource).
// Authenticator queries this immediately after a successful Resolve
// so the audit row can record served_stale=true.
type StaleAware interface {
	LastServedStale() bool
}
```

The `matchesAny` (parses lines from `[]string`) helper used by the old code is no longer called from `Authenticator`, but **leave** the existing private symbol in place for any tests that referenced it — actually, the existing test file references it via `matchesAny` only inside `auth.go`. We removed the function; verify by running tests in step 3.

Actually re-check: the original `matchesAny([]string)` was *only* used by the inline path inside `Authenticate`. With Task 2's refactor, sources return `[]ssh.PublicKey` (already parsed). So `matchesAny` on string slices is unused; deletion is clean. The new `matchesAnyParsed` takes parsed keys.

- [ ] **Step 2: Update `audit.go` to carry the new fields (forward-declare)**

Edit `internal/gateway/audit.go`. Replace the `AuthPath` struct definition with:

```go
// AuthPath captures how a session was authenticated.
type AuthPath struct {
	Kind          string `json:"kind"`                      // "direct" | "trusted_proxy"
	Alias         string `json:"alias,omitempty"`           // proxy alias when Kind=="trusted_proxy"
	User          string `json:"user,omitempty"`            // authenticated User name
	Pod           string `json:"pod,omitempty"`             // target DevPod
	Source        string `json:"source,omitempty"`          // "crd" | "ldap" | "" (proxy)
	ServedStale   bool   `json:"served_stale,omitempty"`    // true only when Source served from stale cache
	LastSourceErr string `json:"last_source_err,omitempty"` // surfaces the most recent IdentitySource hard error
}
```

`SessionOpen` and `SessionClose` are extended in Task 4. For now the struct fields exist so `auth.go` compiles.

- [ ] **Step 3: Verify all existing tests still pass (behavioral parity)**

Run:
```bash
go test ./internal/gateway/ -v
```
Expected: ALL existing auth_test.go cases continue to PASS. The new code paths (sources slice) aren't exercised yet beyond the default `[]IdentitySource{NewCRDSource(a.c)}` fallback, which behaves identically to the old inline `c.Get(User) → matchesAny` path.

If any test fails, the most likely reason is the renamed sentinel-error message (`ErrPubkeyMismatch` message changed slightly). Update the message back to:

```go
ErrPubkeyMismatch  = errors.New("pubkey does not match any User entry")
```

If the original tests assert on `errors.Is` only (no string compare), that adjustment isn't required.

- [ ] **Step 4: Commit**

```bash
git add internal/gateway/auth.go internal/gateway/audit.go
git commit -m "internal/gateway: Authenticator now consults []IdentitySource"
```

---

## Task 4: Audit — emit `source`, `served_stale`, `last_source_err` (TDD)

**Files:**
- Modify: `internal/gateway/audit.go` (extend `SessionOpen`)
- Modify: `internal/gateway/audit_test.go`

- [ ] **Step 1: Write failing tests**

Append to `internal/gateway/audit_test.go`:

```go
func TestSessionOpen_EmitsSourceFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	ap := gateway.AuthPath{
		Kind:        "direct",
		User:        "alice",
		Pod:         "smoke",
		Source:      "ldap",
		ServedStale: true,
	}
	gateway.SessionOpen(logger, "sess-1", ap, "1.2.3.4:5678", "SHA256:abc")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if got["source"] != "ldap" {
		t.Errorf("source = %v, want %q", got["source"], "ldap")
	}
	if got["served_stale"] != true {
		t.Errorf("served_stale = %v, want true", got["served_stale"])
	}
}

func TestSessionOpen_OmitsSourceForTrustedProxy(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	ap := gateway.AuthPath{
		Kind:  "trusted_proxy",
		User:  "alice",
		Pod:   "smoke",
		Alias: "fw1",
	}
	gateway.SessionOpen(logger, "sess-2", ap, "1.2.3.4:5678", "SHA256:abc")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if _, ok := got["source"]; ok {
		t.Errorf("source unexpectedly present: %v", got["source"])
	}
	if v, ok := got["served_stale"]; ok && v != false {
		t.Errorf("served_stale unexpectedly true: %v", v)
	}
}
```

Add the `bytes`, `encoding/json`, `log/slog`, and `testing` imports at the top of `audit_test.go` if not already present, plus the `gateway` import.

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/gateway/ -run TestSessionOpen_Emits -v
```
Expected: FAIL — `source` key not found in log JSON.

- [ ] **Step 3: Extend `SessionOpen`**

Edit `internal/gateway/audit.go`. Replace the `SessionOpen` body with:

```go
func SessionOpen(logger *slog.Logger, sessionID string, ap AuthPath, clientIP, pubkeyFP string) {
	attrs := []any{
		"session_id", sessionID,
		"auth_path", ap.Kind,
		"user", ap.User,
		"devpod", ap.Pod,
		"client_ip", clientIP,
		"pubkey_fp", pubkeyFP,
	}
	if ap.Alias != "" {
		attrs = append(attrs, "proxy_alias", ap.Alias)
	}
	if ap.Source != "" {
		attrs = append(attrs, "source", ap.Source)
		// served_stale is only meaningful when a source actually ran
		// (i.e., not trusted-proxy auth).
		attrs = append(attrs, "served_stale", ap.ServedStale)
	}
	if ap.LastSourceErr != "" {
		attrs = append(attrs, "last_source_err", ap.LastSourceErr)
	}
	logger.Info("session_open", attrs...)
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/gateway/ -run TestSessionOpen -v
```
Expected: all session-open tests PASS (including any pre-existing ones — the changes are additive).

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/audit.go internal/gateway/audit_test.go
git commit -m "internal/gateway: audit emits source/served_stale/last_source_err"
```

---

## Task 5: Metrics — LDAP counters + parse-error counter + auth-result rewiring (TDD)

**Files:**
- Modify: `internal/gateway/metrics.go`
- Modify: `internal/gateway/metrics_test.go`
- Modify: `internal/gateway/identity.go` (uncomment the `PubkeyParseErrorsTotal.Inc()` call deferred in Task 2)

- [ ] **Step 1: Write failing tests**

Append to `internal/gateway/metrics_test.go`:

```go
func TestNewMetrics_LDAPCountersRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	gateway.MustRegisterMetrics(reg)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"devpod_gateway_ldap_lookups_total",
		"devpod_gateway_ldap_lookup_duration_seconds",
		"devpod_gateway_ldap_cache_entries",
		"devpod_gateway_ldap_connection_state",
		"devpod_gateway_ldap_pubkey_parse_errors_total",
	} {
		if !names[want] {
			t.Errorf("metric %q not registered", want)
		}
	}
}

func TestPubkeyParseErrorsTotal_IncBySource(t *testing.T) {
	reg := prometheus.NewRegistry()
	gateway.MustRegisterMetrics(reg)
	gateway.PubkeyParseErrorsTotal.WithLabelValues("crd").Inc()
	gateway.PubkeyParseErrorsTotal.WithLabelValues("ldap").Inc()
	gateway.PubkeyParseErrorsTotal.WithLabelValues("ldap").Inc()
	count := func(label string) float64 {
		return testutil.ToFloat64(gateway.PubkeyParseErrorsTotal.WithLabelValues(label))
	}
	if got := count("crd"); got != 1 {
		t.Errorf("crd = %v, want 1", got)
	}
	if got := count("ldap"); got != 2 {
		t.Errorf("ldap = %v, want 2", got)
	}
}
```

Add `"github.com/prometheus/client_golang/prometheus/testutil"` to `metrics_test.go`'s import list if missing.

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/gateway/ -run 'TestNewMetrics_LDAPCountersRegistered|TestPubkeyParseErrors' -v
```
Expected: FAIL — undefined references / missing metrics.

- [ ] **Step 3: Add the new metrics**

Edit `internal/gateway/metrics.go`. Append the following block before the existing `MustRegisterMetrics` function:

```go
// LDAPLookupsTotal counts LDAP source lookups by outcome.
//   outcome ∈ {hit, miss, error, cache_fresh, cache_stale}
var LDAPLookupsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "devpod_gateway_ldap_lookups_total",
	Help: "LDAP identity-source lookups, grouped by outcome.",
}, []string{"outcome"})

// LDAPLookupDuration is the wire-time histogram for live LDAP
// lookups (cache hits are not recorded).
var LDAPLookupDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "devpod_gateway_ldap_lookup_duration_seconds",
	Help:    "Wall-clock duration of live LDAP lookups (cache misses).",
	Buckets: prometheus.ExponentialBuckets(0.001, 4, 7), // 1ms .. ~4s
})

// LDAPCacheEntries is the current cache size by kind (positive vs
// negative). The gauge is set in bulk on every Resolve as a
// best-effort snapshot.
var LDAPCacheEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "devpod_gateway_ldap_cache_entries",
	Help: "Number of entries in the LDAP source cache.",
}, []string{"kind"})

// LDAPConnectionState is 1 while a bound LDAP connection is held,
// 0 when nil.
var LDAPConnectionState = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "devpod_gateway_ldap_connection_state",
	Help: "1 if an LDAP bind is held, 0 otherwise.",
})

// PubkeyParseErrorsTotal counts authorized_keys lines we couldn't
// parse, labeled by source ("crd"|"ldap").
var PubkeyParseErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "devpod_gateway_ldap_pubkey_parse_errors_total",
	Help: "authorized_keys lines that failed to parse, by source.",
}, []string{"source"})
```

Then update `MustRegisterMetrics` to register them. The existing body currently has something like:

```go
func MustRegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		SessionsTotal,
		DialFailuresTotal,
		AuthFailuresTotal,
		SessionDurationSeconds,
	)
}
```

Append the new vars to the variadic argument list:

```go
func MustRegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(
		SessionsTotal,
		DialFailuresTotal,
		AuthFailuresTotal,
		SessionDurationSeconds,
		LDAPLookupsTotal,
		LDAPLookupDuration,
		LDAPCacheEntries,
		LDAPConnectionState,
		PubkeyParseErrorsTotal,
	)
}
```

- [ ] **Step 4: Wire the parse-error increment in `crdSource`**

Edit `internal/gateway/identity.go`. In `crdSource.Resolve`, replace the parse loop with:

```go
	for _, line := range u.Spec.Pubkeys {
		parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
		if perr != nil {
			PubkeyParseErrorsTotal.WithLabelValues("crd").Inc()
			continue
		}
		keys = append(keys, parsed)
	}
```

- [ ] **Step 5: Run, verify pass**

```bash
go test ./internal/gateway/ -v
```
Expected: ALL tests PASS — new metric tests green, no regressions.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/metrics.go internal/gateway/metrics_test.go internal/gateway/identity.go
git commit -m "internal/gateway: LDAP metrics scaffolding + crdSource parse-error counter"
```

---

## Task 6: RFC 4515 filter-escape helper (TDD, pure)

**Files:**
- Create: `internal/gateway/ldap_escape.go`
- Create: `internal/gateway/ldap_escape_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/ldap_escape_test.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import "testing"

func TestEscapeRFC4515(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"safe", "alice", "alice"},
		{"hyphen-and-digits", "alice-42", "alice-42"},
		{"paren-open", "alice(", `alice\28`},
		{"paren-close", "alice)", `alice\29`},
		{"asterisk", "*", `\2a`},
		{"backslash", `alice\`, `alice\5c`},
		{"nul", "alice\x00", `alice\00`},
		{"injection-attempt", "alice)(uid=*", `alice\29\28uid=\2a`},
		{"backslash-must-be-first", `\(`, `\5c\28`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeRFC4515(tc.in); got != tc.want {
				t.Errorf("escapeRFC4515(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/gateway/ -run TestEscapeRFC4515 -v
```
Expected: FAIL — `undefined: escapeRFC4515`.

- [ ] **Step 3: Implement the escape**

Create `internal/gateway/ldap_escape.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import "strings"

// escapeRFC4515 escapes a value for safe substitution inside an LDAP
// search filter (RFC 4515 §3). Order matters: backslash MUST be
// rewritten first so the subsequent escapes don't get re-escaped.
func escapeRFC4515(s string) string {
	if s == "" {
		return s
	}
	// Fast path: all bytes safe.
	safe := true
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', 0x00, '(', ')', '*':
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\5c`)
		case 0x00:
			b.WriteString(`\00`)
		case '(':
			b.WriteString(`\28`)
		case ')':
			b.WriteString(`\29`)
		case '*':
			b.WriteString(`\2a`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/gateway/ -run TestEscapeRFC4515 -v
```
Expected: all 10 cases PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/ldap_escape.go internal/gateway/ldap_escape_test.go
git commit -m "internal/gateway: RFC 4515 filter-value escape helper"
```

---

## Task 7: `ldapSource` construction — disk-mounted Secrets + template parse (TDD)

**Files:**
- Modify: `go.mod`, `go.sum` (add `github.com/go-ldap/ldap/v3`)
- Create: `internal/gateway/ldap.go` (partial — construction only)
- Create: `internal/gateway/ldap_test.go` (partial — construction tests)

LDAP Secrets are mounted into the gateway Pod by the Helm chart at:

```
/etc/devpod/gateway/ldap/ca.crt    (from BindCAFromCASecretRef, key "ca.crt")
/etc/devpod/gateway/ldap/password  (from BindPasswordSecretRef, key "password")
```

`NewLDAPSource` takes a `Config` struct (the CRD spec subset it needs)
plus those two file paths. `cmd/gateway/main.go` will wire them in
Task 12.

- [ ] **Step 1: Add the go-ldap dependency**

Run:
```bash
go get github.com/go-ldap/ldap/v3@v3.4.8
go mod tidy
```

Expected: `go.mod` adds the require line; `go.sum` updates. Pin to a recent stable v3 release (3.4.8 is current at writing).

- [ ] **Step 2: Write the construction tests**

Create `internal/gateway/ldap_test.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

// writeFile writes data to dir/name and returns the full path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// pemEncodedCA returns a freshly minted self-signed CA in PEM form.
// Used to populate the ca.crt Secret content in tests.
func pemEncodedCA(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ldap-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func validLDAPConfig(t *testing.T) (gateway.LDAPConfig, string) {
	t.Helper()
	dir := t.TempDir()
	caPath := writeFile(t, dir, "ca.crt", pemEncodedCA(t))
	pwPath := writeFile(t, dir, "password", []byte("hunter2"))
	cfg := gateway.LDAPConfig{
		URL:                     "ldaps://ldap.example.test:636",
		CAPath:                  caPath,
		BindDN:                  "cn=svc,dc=example,dc=test",
		BindPasswordPath:        pwPath,
		BaseDN:                  "dc=example,dc=test",
		UserFilter:              "", // exercise default
		PubkeyAttribute:         "sshPublicKey",
		RequestTimeout:          5 * time.Second,
		CacheTTL:                5 * time.Minute,
		NegativeCacheTTL:        30 * time.Second,
		StaleGrace:              15 * time.Minute,
	}
	return cfg, dir
}

func TestNewLDAPSource_OK(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	src, err := gateway.NewLDAPSource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewLDAPSource: %v", err)
	}
	if got := src.Name(); got != "ldap" {
		t.Errorf("Name() = %q, want %q", got, "ldap")
	}
}

func TestNewLDAPSource_RejectsPlaintextURL(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.URL = "ldap://ldap.example.test:389"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for plaintext ldap://")
	}
}

func TestNewLDAPSource_MissingCAFile(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.CAPath = "/nonexistent/ca.crt"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestNewLDAPSource_MalformedCA(t *testing.T) {
	cfg, dir := validLDAPConfig(t)
	cfg.CAPath = writeFile(t, dir, "ca-bad.crt", []byte("not a pem block"))
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for malformed CA")
	}
}

func TestNewLDAPSource_MissingPasswordFile(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.BindPasswordPath = "/nonexistent/password"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for missing password file")
	}
}

func TestNewLDAPSource_BadFilterTemplate(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.UserFilter = "(&(uid={{.Username" // unclosed action
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for unparseable template")
	}
}

func TestNewLDAPSource_FilterMustReferenceUsername(t *testing.T) {
	cfg, _ := validLDAPConfig(t)
	cfg.UserFilter = "(uid=hardcoded)"
	if _, err := gateway.NewLDAPSource(context.Background(), cfg); err == nil {
		t.Fatal("expected error for filter that ignores Username")
	}
}
```

- [ ] **Step 3: Run, verify failure**

```bash
go test ./internal/gateway/ -run TestNewLDAPSource -v
```
Expected: FAIL — `undefined: gateway.LDAPConfig`, `undefined: gateway.NewLDAPSource`.

- [ ] **Step 4: Implement `LDAPConfig`, `NewLDAPSource`, partial `ldapSource`**

Create `internal/gateway/ldap.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/go-ldap/ldap/v3"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/singleflight"
)

// LDAPConfig is the runtime view of GatewayConfig.spec.ldap. Path
// fields point to the files mounted from the referenced Secrets.
type LDAPConfig struct {
	URL              string
	CAPath           string        // file: PEM CA bundle, "ca.crt"
	BindDN           string
	BindPasswordPath string        // file: bind password, "password"
	BaseDN           string
	UserFilter       string        // empty ⇒ DefaultUserFilter
	PubkeyAttribute  string        // empty ⇒ "sshPublicKey"
	RequestTimeout   time.Duration
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	StaleGrace       time.Duration
}

// DefaultUserFilter is used when LDAPConfig.UserFilter is empty.
const DefaultUserFilter = `(&(objectClass=posixAccount)(uid={{.Username}}))`

// defaultPubkeyAttribute is used when LDAPConfig.PubkeyAttribute is empty.
const defaultPubkeyAttribute = "sshPublicKey"

type ldapSource struct {
	cfg        LDAPConfig
	bindPass   []byte
	caPool     *x509.CertPool
	filterTmpl *template.Template
	pubkeyAttr string
	clock      func() time.Time

	connMu sync.Mutex
	conn   *ldap.Conn

	cacheMu sync.Mutex
	cache   map[string]*ldapCacheEntry
	flight  singleflight.Group

	staleMu     sync.Mutex
	lastStale   bool
}

type ldapCacheEntry struct {
	keys      []ssh.PublicKey
	fetchedAt time.Time
	lastErrAt time.Time
}

// NewLDAPSource validates cfg, loads the on-disk Secret-mounted CA and
// bind password, and returns a ready (un-dialed) IdentitySource. The
// first call to Resolve will dial and bind.
func NewLDAPSource(_ context.Context, cfg LDAPConfig) (IdentitySource, error) {
	if !strings.HasPrefix(cfg.URL, "ldaps://") {
		return nil, fmt.Errorf("ldap: URL must use ldaps:// scheme, got %q", cfg.URL)
	}
	caBytes, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("ldap: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bytes.TrimSpace(caBytes)) {
		return nil, errors.New("ldap: CA file did not contain any PEM certificates")
	}
	pw, err := os.ReadFile(cfg.BindPasswordPath)
	if err != nil {
		return nil, fmt.Errorf("ldap: read bind password: %w", err)
	}
	filterSrc := cfg.UserFilter
	if filterSrc == "" {
		filterSrc = DefaultUserFilter
	}
	tmpl, err := template.New("ldap-filter").Parse(filterSrc)
	if err != nil {
		return nil, fmt.Errorf("ldap: parse UserFilter: %w", err)
	}
	// Probe-render with a known marker to ensure (a) template
	// executes and (b) the Username field actually substitutes
	// somewhere — a filter that ignores .Username would let any
	// authenticated bind match any username, which is a footgun.
	const probe = "DEVPODPROBE0"
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"Username": probe}); err != nil {
		return nil, fmt.Errorf("ldap: execute UserFilter (probe): %w", err)
	}
	if !strings.Contains(buf.String(), probe) {
		return nil, fmt.Errorf("ldap: UserFilter must reference {{.Username}}; got %q", filterSrc)
	}
	pubkeyAttr := cfg.PubkeyAttribute
	if pubkeyAttr == "" {
		pubkeyAttr = defaultPubkeyAttribute
	}
	return &ldapSource{
		cfg:        cfg,
		bindPass:   bytes.TrimRight(pw, "\r\n"),
		caPool:     pool,
		filterTmpl: tmpl,
		pubkeyAttr: pubkeyAttr,
		clock:      time.Now,
		cache:      make(map[string]*ldapCacheEntry),
	}, nil
}

func (s *ldapSource) Name() string { return "ldap" }

// LastServedStale satisfies the StaleAware interface in auth.go.
// Returns true if the most recent Resolve returned stale-cache data.
// Authenticator queries this immediately after Resolve, so the
// single-flag implementation is sufficient under normal call patterns.
func (s *ldapSource) LastServedStale() bool {
	s.staleMu.Lock()
	defer s.staleMu.Unlock()
	return s.lastStale
}

func (s *ldapSource) setLastStale(v bool) {
	s.staleMu.Lock()
	s.lastStale = v
	s.staleMu.Unlock()
}

// Resolve is implemented incrementally — full version in Task 8/9/10.
// Stub returns ErrIdentityNotFound to keep the type satisfying
// IdentitySource for the moment.
func (s *ldapSource) Resolve(_ context.Context, _ string) ([]ssh.PublicKey, error) {
	return nil, ErrIdentityNotFound
}
```

- [ ] **Step 5: Run, verify construction tests pass**

```bash
go test ./internal/gateway/ -run TestNewLDAPSource -v
```
Expected: all 7 construction cases PASS.

- [ ] **Step 6: Confirm existing tests still green**

```bash
go test ./internal/gateway/ -v
```
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/ldap.go internal/gateway/ldap_test.go go.mod go.sum
git commit -m "internal/gateway: ldapSource construction (config validation, CA + password load, template probe)"
```

---

## Task 8: `ldapSource` live lookup against an in-process LDAPS server (TDD)

**Files:**
- Modify: `go.mod`, `go.sum` (add `github.com/jimlambrt/gldap`)
- Modify: `internal/gateway/ldap.go` (implement `Resolve` live path)
- Modify: `internal/gateway/ldap_test.go`

This task wires the real go-ldap client into `Resolve` and exercises
it against a small in-process LDAPS server. Cache + soft-fail layers
land in Task 9; singleflight + reconnect in Task 10.

- [ ] **Step 1: Add the gldap test dependency**

```bash
go get -t github.com/jimlambrt/gldap@v0.1.10
go mod tidy
```

Expected: go-ldap appears as a regular `require`, gldap as either an
indirect require (used only by tests) or under `require (...)`. Either
is fine.

- [ ] **Step 2: Write the live-lookup tests with a shared LDAPS fixture**

Append to `internal/gateway/ldap_test.go`:

```go
// --- LDAPS test fixture using jimlambrt/gldap --------------------------

import _ "embed" // for any future LDIF embedding

// fixtureLDAP starts a TLS-terminating LDAP server bound to 127.0.0.1
// and returns its ldaps:// URL plus a CA PEM path the gateway can
// trust. The server handles bind + search; the entries argument is a
// flat map keyed by uid → []sshPublicKey lines.
func fixtureLDAP(t *testing.T, entries map[string][]string) (url, caPath string, server *gldap.Server) {
	t.Helper()
	// generate self-signed leaf usable as both CA and serving cert
	// for 127.0.0.1.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"127.0.0.1", "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	caPath = writeFile(t, dir, "ca.crt", certPEM)
	certPath := writeFile(t, dir, "tls.crt", certPEM)
	keyPath := writeFile(t, dir, "tls.key", keyPEM)

	// gldap server with bind/search handlers
	srv, err := gldap.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	router, err := gldap.NewMux()
	if err != nil {
		t.Fatal(err)
	}
	router.Bind(func(w *gldap.ResponseWriter, r *gldap.Request) {
		w.Write(r.NewBindResponse(gldap.WithResponseCode(gldap.ResultSuccess)))
	})
	router.Search(func(w *gldap.ResponseWriter, r *gldap.Request) {
		msg, _ := r.GetSearchMessage()
		// Filter looks like (&(objectClass=posixAccount)(uid=alice))
		// — naive substring extraction is fine for the test.
		f := msg.Filter
		uid := extractUIDFromFilter(f)
		if vals, ok := entries[uid]; ok {
			entry := &gldap.Entry{
				DN: "uid=" + uid + ",ou=People,dc=example,dc=test",
				Attributes: []*gldap.EntryAttribute{
					{Name: "sshPublicKey", Values: vals},
				},
			}
			_ = w.Write(r.NewSearchResponseEntry(entry.DN, gldap.WithAttributes(map[string][]string{
				"sshPublicKey": vals,
			})))
		}
		_ = w.Write(r.NewSearchDoneResponse())
	})
	srv.Router(router)

	if err := srv.Run(gldap.WithTLSConfig(&tls.Config{Certificates: []tls.Certificate{mustX509KeyPair(t, certPath, keyPath)}}), gldap.WithBindAddress("127.0.0.1:0")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	url = "ldaps://" + srv.Address()
	return url, caPath, srv
}

func mustX509KeyPair(t *testing.T, certPath, keyPath string) tls.Certificate {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// extractUIDFromFilter pulls the first uid=<val> token out of an LDAP
// filter rendered from the default template. Test-only.
func extractUIDFromFilter(f string) string {
	const tok = "(uid="
	i := strings.Index(f, tok)
	if i < 0 {
		return ""
	}
	rest := f[i+len(tok):]
	j := strings.IndexAny(rest, ")")
	if j < 0 {
		return rest
	}
	return rest[:j]
}
```

The imports for this test file now include: `crypto/tls`, `encoding/pem`, `crypto/x509/pkix`, `math/big`, `net`, `strings`, plus `github.com/jimlambrt/gldap`.

Then append the live-lookup test cases:

```go
func TestLDAPSource_Resolve_SingleEntrySingleKey(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src, err := gateway.NewLDAPSource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	keys, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("len keys = %d, want 1", len(keys))
	}
}

func TestLDAPSource_Resolve_MultiValueKeys(t *testing.T) {
	_, a := ed25519Pubkey(t)
	_, b := ed25519Pubkey(t)
	_, c := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{"alice": {a, b, c}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)
	keys, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("len keys = %d, want 3", len(keys))
	}
}

func TestLDAPSource_Resolve_ZeroEntries_ReturnsNotFound(t *testing.T) {
	url, ca, _ := fixtureLDAP(t, map[string][]string{})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)
	_, err := src.Resolve(context.Background(), "ghost")
	if !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("err = %v, want ErrIdentityNotFound", err)
	}
}

func TestLDAPSource_Resolve_DropsUnparseableValues(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{
		"alice": {line, "not-a-key", "still-garbage"},
	})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)
	keys, err := src.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("len keys = %d, want 1 (garbage dropped)", len(keys))
	}
}

func TestLDAPSource_Resolve_FilterInjectionIsEscaped(t *testing.T) {
	// Only alice has a key. A malicious username tries to broaden
	// the search; the escape MUST prevent it from matching alice.
	_, line := ed25519Pubkey(t)
	url, ca, _ := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)
	_, err := src.Resolve(context.Background(), "alice)(uid=*")
	if !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("err = %v, want ErrIdentityNotFound (escape rejected injection)", err)
	}
}
```

The `errors` import was already there from Task 7; ensure it is.

- [ ] **Step 3: Run, verify failure**

```bash
go test ./internal/gateway/ -run TestLDAPSource_Resolve -v
```
Expected: FAIL — Resolve currently returns `ErrIdentityNotFound` unconditionally.

- [ ] **Step 4: Implement live `Resolve` (no cache yet — pass-through)**

Edit `internal/gateway/ldap.go`. Replace the stub `Resolve` with:

```go
func (s *ldapSource) Resolve(ctx context.Context, username string) ([]ssh.PublicKey, error) {
	s.setLastStale(false)
	keys, err := s.liveLookup(ctx, username)
	if err != nil {
		LDAPLookupsTotal.WithLabelValues("error").Inc()
		return nil, err
	}
	if keys == nil {
		LDAPLookupsTotal.WithLabelValues("miss").Inc()
		return nil, ErrIdentityNotFound
	}
	LDAPLookupsTotal.WithLabelValues("hit").Inc()
	return keys, nil
}

// liveLookup performs one round-trip against LDAP. Returns
// (keys, nil) on a hit, (nil, nil) on "no such user", (nil, err) on a
// real failure.
func (s *ldapSource) liveLookup(ctx context.Context, username string) ([]ssh.PublicKey, error) {
	start := s.clock()
	defer func() {
		LDAPLookupDuration.Observe(s.clock().Sub(start).Seconds())
	}()

	conn, err := s.acquireConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("ldap dial/bind: %w", err)
	}
	filter, err := s.renderFilter(username)
	if err != nil {
		return nil, fmt.Errorf("ldap render filter: %w", err)
	}
	req := ldap.NewSearchRequest(
		s.cfg.BaseDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		2, // SizeLimit
		int(s.cfg.RequestTimeout/time.Second),
		false,
		filter,
		[]string{s.pubkeyAttr},
		nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		// On any LDAP error, drop the conn so the next call redials.
		s.resetConn()
		return nil, fmt.Errorf("ldap search: %w", err)
	}
	if len(res.Entries) == 0 {
		return nil, nil
	}
	vals := res.Entries[0].GetAttributeValues(s.pubkeyAttr)
	keys := make([]ssh.PublicKey, 0, len(vals))
	for _, v := range vals {
		parsed, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(v))
		if perr != nil {
			PubkeyParseErrorsTotal.WithLabelValues("ldap").Inc()
			continue
		}
		keys = append(keys, parsed)
	}
	return keys, nil
}

// renderFilter executes the configured template against an escaped
// username.
func (s *ldapSource) renderFilter(username string) (string, error) {
	var buf bytes.Buffer
	if err := s.filterTmpl.Execute(&buf, map[string]string{"Username": escapeRFC4515(username)}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// acquireConn returns a connected, bound LDAP conn, redialing on the
// first call or after a previous error reset it.
func (s *ldapSource) acquireConn(ctx context.Context) (*ldap.Conn, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		return s.conn, nil
	}
	c, err := ldap.DialURL(s.cfg.URL,
		ldap.DialWithTLSConfig(s.tlsConfig()),
	)
	if err != nil {
		return nil, err
	}
	c.SetTimeout(s.cfg.RequestTimeout)
	if err := c.Bind(s.cfg.BindDN, string(s.bindPass)); err != nil {
		_ = c.Close()
		return nil, err
	}
	s.conn = c
	LDAPConnectionState.Set(1)
	return c, nil
}

// resetConn closes the current conn (if any) so the next acquire
// dials fresh. Safe to call without holding connMu.
func (s *ldapSource) resetConn() {
	s.connMu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
		LDAPConnectionState.Set(0)
	}
	s.connMu.Unlock()
}

func (s *ldapSource) tlsConfig() *tls.Config {
	host := strings.TrimPrefix(s.cfg.URL, "ldaps://")
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return &tls.Config{
		RootCAs:    s.caPool,
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}
}
```

Add `"crypto/tls"` to the import block of `ldap.go`.

- [ ] **Step 5: Run, verify live tests pass**

```bash
go test ./internal/gateway/ -run TestLDAPSource_Resolve -v
```
Expected: all 5 live cases PASS.

- [ ] **Step 6: Confirm full package green**

```bash
go test ./internal/gateway/ -v
```
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/ldap.go internal/gateway/ldap_test.go go.mod go.sum
git commit -m "internal/gateway: ldapSource live lookup (search, parse, conn reuse, RFC 4515 escape)"
```

---

## Task 9: Cache layer + soft-fail / stale-grace (TDD)

**Files:**
- Modify: `internal/gateway/ldap.go`
- Modify: `internal/gateway/ldap_test.go`

- [ ] **Step 1: Write failing tests for cache behavior with injected clock**

Append to `internal/gateway/ldap_test.go`:

```go
// withClock swaps the clock on an *ldapSource for deterministic time
// tests. Returns a setter that advances by d on each call.
func withClock(t *testing.T, src gateway.IdentitySource, start time.Time) (advance func(d time.Duration), now func() time.Time) {
	t.Helper()
	cur := start
	now = func() time.Time { return cur }
	advance = func(d time.Duration) { cur = cur.Add(d) }
	gateway.SetLDAPClockForTesting(src, now)
	return advance, now
}

func TestLDAPSource_PositiveCache_FreshServesWithoutLDAP(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, srv := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.CacheTTL = time.Minute
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)
	adv, _ := withClock(t, src, time.Now())

	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// Stop the server; a fresh cache must hide its absence.
	_ = srv.Stop()
	adv(30 * time.Second) // still within CacheTTL
	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("cached Resolve: %v", err)
	}
}

func TestLDAPSource_StaleGrace_ServesPriorKeysDuringOutage(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, srv := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.CacheTTL = 10 * time.Second
	cfg.StaleGrace = time.Minute
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)
	adv, _ := withClock(t, src, time.Now())

	// Warm.
	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("warm Resolve: %v", err)
	}
	_ = srv.Stop()

	// Past CacheTTL → next call tries live → errors → must serve stale.
	adv(15 * time.Second)
	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("stale Resolve: %v", err)
	}
	// LastServedStale must reflect the most recent call.
	if !gateway.LastServedStaleForTesting(src) {
		t.Errorf("LastServedStale = false, want true")
	}
}

func TestLDAPSource_StaleGraceExpired_ReturnsError(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, srv := fixtureLDAP(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.CacheTTL = 10 * time.Second
	cfg.StaleGrace = 20 * time.Second
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)
	adv, _ := withClock(t, src, time.Now())

	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	_ = srv.Stop()

	adv(45 * time.Second) // past CacheTTL + StaleGrace
	if _, err := src.Resolve(context.Background(), "alice"); err == nil {
		t.Errorf("expected error past stale-grace window")
	}
}

func TestLDAPSource_NegativeCache_AvoidsLiveCallWithinTTL(t *testing.T) {
	url, ca, srv := fixtureLDAP(t, map[string][]string{})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	cfg.NegativeCacheTTL = time.Minute
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)
	adv, _ := withClock(t, src, time.Now())

	if _, err := src.Resolve(context.Background(), "ghost"); !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("first: %v", err)
	}
	_ = srv.Stop() // even if LDAP is down, negative cache must hold
	adv(30 * time.Second)
	if _, err := src.Resolve(context.Background(), "ghost"); !errors.Is(err, gateway.ErrIdentityNotFound) {
		t.Fatalf("cached negative: %v", err)
	}
}
```

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/gateway/ -run 'TestLDAPSource_(PositiveCache|StaleGrace|NegativeCache)' -v
```
Expected: FAIL — `undefined: gateway.SetLDAPClockForTesting`, `undefined: gateway.LastServedStaleForTesting`, plus cache behavior not yet implemented.

- [ ] **Step 3: Implement the cache layer**

Edit `internal/gateway/ldap.go`. Replace `Resolve` with the cache-aware version:

```go
func (s *ldapSource) Resolve(ctx context.Context, username string) ([]ssh.PublicKey, error) {
	s.setLastStale(false)

	// Cache lookup.
	now := s.clock()
	s.cacheMu.Lock()
	e := s.cache[username]
	s.cacheMu.Unlock()
	if e != nil {
		ttl := s.cfg.CacheTTL
		if e.keys == nil {
			ttl = s.cfg.NegativeCacheTTL
		}
		age := now.Sub(e.fetchedAt)
		if e.lastErrAt.IsZero() && age < ttl {
			LDAPLookupsTotal.WithLabelValues("cache_fresh").Inc()
			return s.fromEntry(e)
		}
		if !e.lastErrAt.IsZero() && age < ttl+s.cfg.StaleGrace {
			LDAPLookupsTotal.WithLabelValues("cache_stale").Inc()
			s.setLastStale(true)
			return s.fromEntry(e)
		}
	}

	// Live lookup via singleflight (full flight wiring in Task 10;
	// here we call directly so the test cases pass deterministically).
	keys, err := s.liveLookup(ctx, username)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if err != nil {
		// Mark the existing entry (if any) as currently failing.
		if e != nil {
			e.lastErrAt = now
			LDAPLookupsTotal.WithLabelValues("error").Inc()
			age := now.Sub(e.fetchedAt)
			ttl := s.cfg.CacheTTL
			if e.keys == nil {
				ttl = s.cfg.NegativeCacheTTL
			}
			if age < ttl+s.cfg.StaleGrace {
				LDAPLookupsTotal.WithLabelValues("cache_stale").Inc()
				s.setLastStale(true)
				return s.fromEntryLocked(e)
			}
		} else {
			LDAPLookupsTotal.WithLabelValues("error").Inc()
		}
		return nil, err
	}

	// Live success → install/replace cache entry.
	s.cache[username] = &ldapCacheEntry{keys: keys, fetchedAt: now}
	s.updateCacheGauges()
	if keys == nil {
		LDAPLookupsTotal.WithLabelValues("miss").Inc()
		return nil, ErrIdentityNotFound
	}
	LDAPLookupsTotal.WithLabelValues("hit").Inc()
	return keys, nil
}

// fromEntry returns the cached value translated to the IdentitySource
// contract. Callers must NOT hold cacheMu.
func (s *ldapSource) fromEntry(e *ldapCacheEntry) ([]ssh.PublicKey, error) {
	if e.keys == nil {
		return nil, ErrIdentityNotFound
	}
	return e.keys, nil
}

// fromEntryLocked is the same as fromEntry but for callers already
// holding cacheMu (so the caller's defer Unlock fires).
func (s *ldapSource) fromEntryLocked(e *ldapCacheEntry) ([]ssh.PublicKey, error) {
	return s.fromEntry(e)
}

// updateCacheGauges samples len(cache) into the positive/negative
// gauges. Best-effort; caller must hold cacheMu.
func (s *ldapSource) updateCacheGauges() {
	var pos, neg int
	for _, v := range s.cache {
		if v.keys == nil {
			neg++
		} else {
			pos++
		}
	}
	LDAPCacheEntries.WithLabelValues("positive").Set(float64(pos))
	LDAPCacheEntries.WithLabelValues("negative").Set(float64(neg))
}
```

Note: in the failure branch above, after `liveLookup` reports an
error, the previous successful entry's `keys` is **kept** so the
stale-grace branch on the next call can serve it. Only `lastErrAt`
moves forward.

The previous `liveLookup` body in Task 8 calls `s.resetConn()` on
search error, which is what makes the next live call redial; that
stays unchanged.

- [ ] **Step 4: Add test-only setters**

Append to the bottom of `internal/gateway/ldap.go`:

```go
// SetLDAPClockForTesting overrides the clock on an ldapSource.
// Returns silently if src is not an *ldapSource (e.g. a fake source
// in unit tests).
func SetLDAPClockForTesting(src IdentitySource, now func() time.Time) {
	if ls, ok := src.(*ldapSource); ok {
		ls.clock = now
	}
}

// LastServedStaleForTesting exposes the per-source stale flag for
// test inspection. Returns false for non-ldapSource implementations.
func LastServedStaleForTesting(src IdentitySource) bool {
	if ls, ok := src.(*ldapSource); ok {
		return ls.LastServedStale()
	}
	return false
}
```

- [ ] **Step 5: Run, verify pass**

```bash
go test ./internal/gateway/ -run TestLDAPSource -v
```
Expected: all cache and live tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/ldap.go internal/gateway/ldap_test.go
git commit -m "internal/gateway: ldapSource cache + soft-fail with stale-grace window"
```

---

## Task 10: Singleflight + reconnect (TDD)

**Files:**
- Modify: `internal/gateway/ldap.go`
- Modify: `internal/gateway/ldap_test.go`

- [ ] **Step 1: Write the singleflight + reconnect tests**

Append to `internal/gateway/ldap_test.go`:

```go
// gldap doesn't easily expose request counters, so we wrap a counting
// search handler around a stripped-down server fixture for these
// tests.
//
// countingFixture returns the fixture plus a pointer to a counter
// that increments on every Search call. Bind/Search succeed
// identically to fixtureLDAP for the matching uid.
func countingFixture(t *testing.T, entries map[string][]string) (url, caPath string, counter *atomic.Int64, srv *gldap.Server) {
	t.Helper()
	counter = new(atomic.Int64)
	url, caPath, srv = fixtureLDAPWithSearchHook(t, entries, func() { counter.Add(1) })
	return
}

// fixtureLDAPWithSearchHook is a thin extension of fixtureLDAP that
// runs hook() at the top of every Search handler. Implementation is
// identical except for the hook line; keep them in sync.
func fixtureLDAPWithSearchHook(t *testing.T, entries map[string][]string, hook func()) (string, string, *gldap.Server) {
	t.Helper()
	// (copy of fixtureLDAP body — inline rather than refactor to keep
	// the original test fixture self-contained for readers landing on
	// the simpler tests first. ANY change to fixtureLDAP MUST be
	// mirrored here.)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"127.0.0.1", "localhost"},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	dir := t.TempDir()
	caPath := writeFile(t, dir, "ca.crt", certPEM)
	certPath := writeFile(t, dir, "tls.crt", certPEM)
	keyPath := writeFile(t, dir, "tls.key", keyPEM)
	srv, _ := gldap.NewServer()
	router, _ := gldap.NewMux()
	router.Bind(func(w *gldap.ResponseWriter, r *gldap.Request) {
		w.Write(r.NewBindResponse(gldap.WithResponseCode(gldap.ResultSuccess)))
	})
	router.Search(func(w *gldap.ResponseWriter, r *gldap.Request) {
		hook()
		msg, _ := r.GetSearchMessage()
		uid := extractUIDFromFilter(msg.Filter)
		if vals, ok := entries[uid]; ok {
			_ = w.Write(r.NewSearchResponseEntry("uid="+uid+",ou=People,dc=example,dc=test", gldap.WithAttributes(map[string][]string{"sshPublicKey": vals})))
		}
		_ = w.Write(r.NewSearchDoneResponse())
	})
	srv.Router(router)
	_ = srv.Run(gldap.WithTLSConfig(&tls.Config{Certificates: []tls.Certificate{mustX509KeyPair(t, certPath, keyPath)}}), gldap.WithBindAddress("127.0.0.1:0"))
	t.Cleanup(func() { _ = srv.Stop() })
	return "ldaps://" + srv.Address(), caPath, srv
}

func TestLDAPSource_Singleflight_CollapsesConcurrentLookups(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, counter, _ := countingFixture(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = src.Resolve(context.Background(), "alice")
		}()
	}
	wg.Wait()

	got := counter.Load()
	if got < 1 || got > 5 {
		t.Errorf("LDAP searches under concurrent fan-out = %d, expected 1..5", got)
	}
}

func TestLDAPSource_Reconnect_AfterConnDropped(t *testing.T) {
	_, line := ed25519Pubkey(t)
	url, ca, counter, srv := countingFixture(t, map[string][]string{"alice": {line}})
	cfg, _ := validLDAPConfig(t)
	cfg.URL = url
	cfg.CAPath = ca
	src, _ := gateway.NewLDAPSource(context.Background(), cfg)

	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// Force the gateway-side conn to drop so the next Resolve must
	// redial. We don't have a clean hook for this, so the test does
	// it by stopping+restarting the server with a small backoff —
	// gldap re-uses the same port if we pin BindAddress, but the
	// fixture picked :0. So instead we just call ResetConnForTesting.
	gateway.ResetLDAPConnForTesting(src)

	if _, err := src.Resolve(context.Background(), "alice"); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if got := counter.Load(); got < 2 {
		t.Errorf("expected ≥2 LDAP searches after manual reset, got %d", got)
	}
	_ = srv // unused after reset; kept to anchor t.Cleanup ordering
}
```

Imports add: `sync`, `sync/atomic`.

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/gateway/ -run 'TestLDAPSource_(Singleflight|Reconnect)' -v
```
Expected: FAIL — `undefined: gateway.ResetLDAPConnForTesting`; singleflight not yet wired.

- [ ] **Step 3: Wire singleflight into Resolve and add the test reset helper**

Edit `internal/gateway/ldap.go`. Replace the live-lookup call site in `Resolve` to go through `singleflight.Group`:

Find the line:
```go
	keys, err := s.liveLookup(ctx, username)
```
And replace with:
```go
	v, err, _ := s.flight.Do(username, func() (any, error) {
		return s.liveLookup(ctx, username)
	})
	var keys []ssh.PublicKey
	if v != nil {
		keys, _ = v.([]ssh.PublicKey)
	}
```

Append at the bottom of `ldap.go`:

```go
// ResetLDAPConnForTesting drops the bound LDAP connection so the next
// Resolve redials. No-op when src isn't an *ldapSource.
func ResetLDAPConnForTesting(src IdentitySource) {
	if ls, ok := src.(*ldapSource); ok {
		ls.resetConn()
	}
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/gateway/ -run TestLDAPSource -v
```
Expected: all LDAP tests PASS, singleflight test reports ≤5 wire searches across 50 concurrent calls.

- [ ] **Step 5: Run the full package, no regressions**

```bash
go test ./internal/gateway/ -v
```
Expected: green across the board.

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/ldap.go internal/gateway/ldap_test.go
git commit -m "internal/gateway: ldapSource singleflight + connection reset helper"
```

---

## Task 11: Authenticator decision-table tests (multi-source, TDD)

**Files:**
- Modify: `internal/gateway/auth_test.go`

The Task 3 refactor wired the sources slice without expanding test
coverage. Spec §5 lists 10 decision-table outcomes; this task encodes
each as a stub-source test.

- [ ] **Step 1: Add a stub IdentitySource helper**

Append to `internal/gateway/auth_test.go`:

```go
// stubSource is a deterministic IdentitySource for decision-table
// tests. Set keys to ssh keys for a "known" user; set notFound to
// have Resolve always return ErrIdentityNotFound; set boom to
// simulate a hard transport error.
type stubSource struct {
	name     string
	known    map[string][]ssh.PublicKey
	boom     error
	stale    bool
	callLog  []string
}

func (s *stubSource) Name() string { return s.name }
func (s *stubSource) Resolve(_ context.Context, u string) ([]ssh.PublicKey, error) {
	s.callLog = append(s.callLog, u)
	if s.boom != nil {
		return nil, s.boom
	}
	k, ok := s.known[u]
	if !ok {
		return nil, gateway.ErrIdentityNotFound
	}
	return k, nil
}
func (s *stubSource) LastServedStale() bool { return s.stale }
```

- [ ] **Step 2: Write the decision-table cases**

```go
func TestAuth_DecisionTable_CRDMatch(t *testing.T) {
	pk, line := ed25519Pubkey(t)
	dp := newRunningDevPod(t, "smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{line}},
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), defaultNS).
		WithSources([]gateway.IdentitySource{gateway.NewCRDSource(fakeClient(t, u))})
	got, err := a.Authenticate(context.Background(), "alice+smoke", pk)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Source != "crd" {
		t.Errorf("source = %q, want %q", got.AuthPath.Source, "crd")
	}
}

func TestAuth_DecisionTable_CRDMissLDAPMatch_Fallback(t *testing.T) {
	pkCRD, lineCRD := ed25519Pubkey(t) // CRD-stored, NOT offered
	pkLDAP, _ := ed25519Pubkey(t)      // offered by client, only in LDAP
	dp := newRunningDevPod(t, "smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{lineCRD}},
	}
	_ = pkCRD
	ldap := &stubSource{
		name:  "ldap",
		known: map[string][]ssh.PublicKey{"alice": {pkLDAP}},
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), defaultNS).
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t, u)),
			ldap,
		})
	got, err := a.Authenticate(context.Background(), "alice+smoke", pkLDAP)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Source != "ldap" {
		t.Errorf("source = %q, want %q", got.AuthPath.Source, "ldap")
	}
}

func TestAuth_DecisionTable_LDAPOnly(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	dp := newRunningDevPod(t, "smoke", "lalice")
	ldap := &stubSource{
		name:  "ldap",
		known: map[string][]ssh.PublicKey{"lalice": {pk}},
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp), defaultNS).
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t)),
			ldap,
		})
	got, err := a.Authenticate(context.Background(), "lalice+smoke", pk)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Source != "ldap" {
		t.Errorf("source = %q, want %q", got.AuthPath.Source, "ldap")
	}
}

func TestAuth_DecisionTable_NoSourceKnowsUser(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	dp := newRunningDevPod(t, "smoke", "ghost")
	a := gateway.NewAuthenticator(fakeClient(t, dp), defaultNS).
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t)),
			&stubSource{name: "ldap", known: map[string][]ssh.PublicKey{}},
		})
	_, err := a.Authenticate(context.Background(), "ghost+smoke", pk)
	if !errors.Is(err, gateway.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestAuth_DecisionTable_KnownButKeyMismatch(t *testing.T) {
	_, lineCRD := ed25519Pubkey(t)
	other, _ := ed25519Pubkey(t)
	dp := newRunningDevPod(t, "smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{lineCRD}},
	}
	ldap := &stubSource{
		name:  "ldap",
		known: map[string][]ssh.PublicKey{"alice": {}},
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), defaultNS).
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t, u)),
			ldap,
		})
	_, err := a.Authenticate(context.Background(), "alice+smoke", other)
	if !errors.Is(err, gateway.ErrPubkeyMismatch) {
		t.Fatalf("err = %v, want ErrPubkeyMismatch", err)
	}
}

func TestAuth_DecisionTable_LDAPErrCRDMatch(t *testing.T) {
	pk, line := ed25519Pubkey(t)
	dp := newRunningDevPod(t, "smoke", "alice")
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{line}},
	}
	ldap := &stubSource{
		name: "ldap",
		boom: errors.New("ldap unreachable"),
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp, u), defaultNS).
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t, u)),
			ldap,
		})
	got, err := a.Authenticate(context.Background(), "alice+smoke", pk)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Source != "crd" {
		t.Errorf("source = %q, want %q", got.AuthPath.Source, "crd")
	}
}

func TestAuth_DecisionTable_TrustedProxyShortCircuits(t *testing.T) {
	pkProxy, _ := ed25519Pubkey(t)
	dp := newRunningDevPod(t, "smoke", "alice")
	idx := map[string]string{ssh.FingerprintSHA256(pkProxy): "fw1"}
	calls := &stubSource{name: "stub"}
	a := gateway.NewAuthenticator(fakeClient(t, dp), defaultNS).
		WithSources([]gateway.IdentitySource{calls}).
		WithProxyKeys(idx)
	got, err := a.Authenticate(context.Background(), "alice+smoke", pkProxy)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if got.AuthPath.Kind != "trusted_proxy" {
		t.Errorf("kind = %q, want trusted_proxy", got.AuthPath.Kind)
	}
	if len(calls.callLog) != 0 {
		t.Errorf("source consulted on proxy path: %v", calls.callLog)
	}
}

func TestAuth_DecisionTable_StaleLDAPSurfacesServedStale(t *testing.T) {
	pk, _ := ed25519Pubkey(t)
	dp := newRunningDevPod(t, "smoke", "lalice")
	ldap := &stubSource{
		name:  "ldap",
		known: map[string][]ssh.PublicKey{"lalice": {pk}},
		stale: true,
	}
	a := gateway.NewAuthenticator(fakeClient(t, dp), defaultNS).
		WithSources([]gateway.IdentitySource{
			gateway.NewCRDSource(fakeClient(t)),
			ldap,
		})
	got, err := a.Authenticate(context.Background(), "lalice+smoke", pk)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if !got.AuthPath.ServedStale {
		t.Errorf("ServedStale = false, want true")
	}
}
```

`newRunningDevPod` and `defaultNS` helpers must exist in
`auth_test.go` today (the existing tests use them). If they're named
differently, adapt accordingly — but every other test in this file
already constructs a DevPod with phase=Running + endpoint, so the
helper pattern is in place. Pattern from the file:

```go
const defaultNS = "devpods"

func newRunningDevPod(t *testing.T, name, owner string) *devpodv1alpha1.DevPod {
	t.Helper()
	return &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: defaultNS},
		Spec:       devpodv1alpha1.DevPodSpec{Owner: owner},
		Status: devpodv1alpha1.DevPodStatus{
			Phase:    devpodv1alpha1.DevPodRunning,
			Endpoint: "10.0.0.1:2222",
		},
	}
}
```

If the existing file uses different names (likely — let inspection
decide), reuse them rather than redefining. Inspect with `grep -n
'newRunningDevPod\|Endpoint: \"10\\.' internal/gateway/auth_test.go`
before pasting.

- [ ] **Step 3: Run, verify pass**

```bash
go test ./internal/gateway/ -run TestAuth_DecisionTable -v
```
Expected: all 8 decision-table cases PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/gateway/auth_test.go
git commit -m "internal/gateway: decision-table tests for multi-source Authenticator"
```

---

## Task 12: Wire LDAP source into `cmd/gateway` + disk-Secret fail-fast (TDD)

**Files:**
- Modify: `cmd/gateway/main.go`
- Modify: `cmd/gateway/main_test.go`

`cmd/gateway/main.go` learns three new flags (paths for the LDAP CA
file and the bind-password file plus an enable-when-nonempty toggle
via the GatewayConfig), reads the relevant fields off
`GatewayConfig.spec.ldap`, and builds an `[]IdentitySource` slice.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/gateway/main_test.go`:

```go
func TestBuildIdentitySources_NoLDAP_OnlyCRD(t *testing.T) {
	cfg := &devpodv1alpha1.GatewayConfig{}
	srcs, err := buildIdentitySources(context.Background(), nil /*client unused*/, cfg, "/var/empty/ldap")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("len srcs = %d, want 1 (crd only)", len(srcs))
	}
	if srcs[0].Name() != "crd" {
		t.Errorf("srcs[0].Name() = %q, want %q", srcs[0].Name(), "crd")
	}
}

func TestBuildIdentitySources_LDAPSpec_RequiresMountedSecrets(t *testing.T) {
	cfg := &devpodv1alpha1.GatewayConfig{
		Spec: devpodv1alpha1.GatewayConfigSpec{
			LDAP: &devpodv1alpha1.LDAPSpec{
				URL:    "ldaps://ldap.example.test:636",
				BindDN: "cn=svc,dc=example,dc=test",
				BaseDN: "dc=example,dc=test",
			},
		},
	}
	_, err := buildIdentitySources(context.Background(), nil, cfg, "/var/empty/ldap")
	if err == nil {
		t.Fatal("expected error: LDAP configured but no Secret files mounted")
	}
}
```

The CRD types `LDAPSpec` and `GatewayConfigSpec.LDAP` already exist
from Task 1; the test imports `devpodv1alpha1`.

- [ ] **Step 2: Run, verify failure**

```bash
go test ./cmd/gateway/ -run TestBuildIdentitySources -v
```
Expected: FAIL — `undefined: buildIdentitySources`.

- [ ] **Step 3: Add the `--ldap-secret-dir` flag + `buildIdentitySources`**

Edit `cmd/gateway/main.go`. Add the flag inside `main()`'s `flag.StringVar` block:

```go
	var ldapSecretDir string
	flag.StringVar(&ldapSecretDir, "ldap-secret-dir", "/etc/devpod/gateway/ldap",
		"directory holding the LDAP CA bundle ('ca.crt') and bind password ('password'). Required when GatewayConfig.spec.ldap is set.")
```

Below the existing `authn := gateway.NewAuthenticator(...)` line, replace:

```go
	authn := gateway.NewAuthenticator(c, devpodNamespace).WithProxyKeys(proxyKeys)
```

with:

```go
	srcs, err := buildIdentitySources(ctx, c, gw, ldapSecretDir)
	if err != nil {
		slog.Error("build_identity_sources", "err", err)
		os.Exit(1)
	}
	authn := gateway.NewAuthenticator(c, devpodNamespace).
		WithProxyKeys(proxyKeys).
		WithSources(srcs)
```

Then append the helper at the bottom of `main.go`:

```go
// buildIdentitySources composes the ordered identity-source chain
// from the cluster's GatewayConfig.
//
//   - CRD source is always present and queried first.
//   - LDAP source is appended when gw.Spec.LDAP != nil. The bind
//     password and CA bundle live as files under ldapSecretDir; the
//     chart mounts the referenced Secrets there.
func buildIdentitySources(
	ctx context.Context,
	c client.Reader,
	gw *devpodv1alpha1.GatewayConfig,
	ldapSecretDir string,
) ([]gateway.IdentitySource, error) {
	srcs := []gateway.IdentitySource{gateway.NewCRDSource(c)}
	if gw.Spec.LDAP == nil {
		return srcs, nil
	}
	lc := gateway.LDAPConfig{
		URL:              gw.Spec.LDAP.URL,
		CAPath:           filepath.Join(ldapSecretDir, "ca.crt"),
		BindDN:           gw.Spec.LDAP.BindDN,
		BindPasswordPath: filepath.Join(ldapSecretDir, "password"),
		BaseDN:           gw.Spec.LDAP.BaseDN,
		UserFilter:       gw.Spec.LDAP.UserFilter,
		PubkeyAttribute:  gw.Spec.LDAP.PubkeyAttribute,
		RequestTimeout:   time.Duration(orDefault(gw.Spec.LDAP.RequestTimeoutSeconds, 5)) * time.Second,
		CacheTTL:         time.Duration(orDefault(gw.Spec.LDAP.CacheTTLSeconds, 300)) * time.Second,
		NegativeCacheTTL: time.Duration(orDefault(gw.Spec.LDAP.NegativeCacheTTLSeconds, 30)) * time.Second,
		StaleGrace:       time.Duration(orDefault(gw.Spec.LDAP.StaleGraceSeconds, 900)) * time.Second,
	}
	ldapSrc, err := gateway.NewLDAPSource(ctx, lc)
	if err != nil {
		return nil, fmt.Errorf("ldap source: %w", err)
	}
	return append(srcs, ldapSrc), nil
}

// orDefault returns d when v == 0, otherwise int(v). Used for the
// optional-but-zero-meaning-default CRD fields.
func orDefault(v int32, d int32) int32 {
	if v == 0 {
		return d
	}
	return v
}
```

Ensure imports include `path/filepath`, `time`, and `sigs.k8s.io/controller-runtime/pkg/client` (the last is already there).

- [ ] **Step 4: Run, verify pass**

```bash
go test ./cmd/gateway/ -run TestBuildIdentitySources -v
```
Expected: both cases PASS — the no-LDAP case returns one source named "crd", and the LDAP-configured case errors when the secret dir is empty.

- [ ] **Step 5: Confirm the project compiles end-to-end**

```bash
go build ./...
```
Expected: clean.

- [ ] **Step 6: Run full project tests**

```bash
bash hack/test.sh
```
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/gateway/main.go cmd/gateway/main_test.go
git commit -m "cmd/gateway: wire LDAP IdentitySource from GatewayConfig + mounted Secret dir"
```

---

## Task 13: Helm chart — values + Deployment volumes + GatewayConfig template

**Files:**
- Modify: `deploy/chart/values.yaml`
- Modify: `deploy/chart/templates/gateway.yaml`
- Modify: `deploy/chart/templates/gatewayconfig.yaml`

- [ ] **Step 1: Extend `values.yaml`**

Edit `deploy/chart/values.yaml`. Inside the existing `gateway:` block (after the `service:` subkey, around the end of the file), append:

```yaml
  ldap:
    enabled: false
    url: ""
    bindDN: ""
    baseDN: ""
    userFilter: ""             # blank → controller default
    pubkeyAttribute: sshPublicKey
    requestTimeoutSeconds: 5
    cacheTTLSeconds: 300
    negativeCacheTTLSeconds: 30
    staleGraceSeconds: 900
    # Secrets MUST exist before the chart is installed. The chart
    # only references them; it does not generate them. Operators
    # create the Secrets out of band (or via a sealed-secret /
    # external-secrets pipeline).
    caSecret:
      name: ""                 # secretRef.name
      namespace: ""            # secretRef.namespace
    bindPasswordSecret:
      name: ""
      namespace: ""
```

- [ ] **Step 2: Mount the LDAP Secrets into the gateway Deployment**

Edit `deploy/chart/templates/gateway.yaml`. Inside the gateway container's `volumeMounts:` list (just after the `internal-key` mount, lines 46-48), insert a conditional block:

```yaml
        {{- if .Values.gateway.ldap.enabled }}
        - name: ldap-ca
          mountPath: /etc/devpod/gateway/ldap
          readOnly: true
        - name: ldap-bind
          mountPath: /etc/devpod/gateway/ldap
          readOnly: true
        {{- end }}
```

The two Secret mounts target the **same** directory, with Kubernetes
overlaying both items into `/etc/devpod/gateway/ldap/{ca.crt,password}`.
This relies on each Secret having a single, distinctly-named key
(`ca.crt` in the CA Secret, `password` in the bind Secret); collisions
across the two would be a deployment-time misconfiguration.

(Alternative if same-mountPath mounts are problematic in the cluster's
Kubernetes version: mount them at two paths and pass a different flag
value. The implementation above is what `cmd/gateway`'s
`--ldap-secret-dir` expects.)

Then in the Deployment's `volumes:` list (around line 56-62), append a
conditional block:

```yaml
      {{- if .Values.gateway.ldap.enabled }}
      - name: ldap-ca
        secret:
          secretName: {{ .Values.gateway.ldap.caSecret.name }}
          items:
          - key: ca.crt
            path: ca.crt
      - name: ldap-bind
        secret:
          secretName: {{ .Values.gateway.ldap.bindPasswordSecret.name }}
          items:
          - key: password
            path: password
      {{- end }}
```

If `.Values.gateway.ldap.caSecret.namespace` differs from the
gateway's namespace, the Secret must be copied (chart-side or via the
operator) — Kubernetes Secret mounts are namespace-scoped to the Pod.
The chart documents this in `values.yaml` (a comment about Secrets
needing to live in `namespaces.system`).

- [ ] **Step 3: Render `spec.ldap` in the GatewayConfig template**

Edit `deploy/chart/templates/gatewayconfig.yaml`. Inside `spec:`, after the existing `banner:` field (or wherever the last field lives), append:

```yaml
  {{- if .Values.gateway.ldap.enabled }}
  ldap:
    url: {{ .Values.gateway.ldap.url | quote }}
    bindDN: {{ .Values.gateway.ldap.bindDN | quote }}
    baseDN: {{ .Values.gateway.ldap.baseDN | quote }}
    {{- with .Values.gateway.ldap.userFilter }}
    userFilter: {{ . | quote }}
    {{- end }}
    pubkeyAttribute: {{ .Values.gateway.ldap.pubkeyAttribute | quote }}
    requestTimeoutSeconds: {{ .Values.gateway.ldap.requestTimeoutSeconds }}
    cacheTTLSeconds: {{ .Values.gateway.ldap.cacheTTLSeconds }}
    negativeCacheTTLSeconds: {{ .Values.gateway.ldap.negativeCacheTTLSeconds }}
    staleGraceSeconds: {{ .Values.gateway.ldap.staleGraceSeconds }}
    caSecretRef:
      name: {{ .Values.gateway.ldap.caSecret.name | quote }}
      namespace: {{ .Values.gateway.ldap.caSecret.namespace | quote }}
    bindPasswordSecretRef:
      name: {{ .Values.gateway.ldap.bindPasswordSecret.name | quote }}
      namespace: {{ .Values.gateway.ldap.bindPasswordSecret.namespace | quote }}
  {{- end }}
```

- [ ] **Step 4: Render the chart with LDAP disabled (no regression)**

```bash
helm template test deploy/chart/ > /tmp/chart-ldap-off.yaml
grep -A 3 'ldap:' /tmp/chart-ldap-off.yaml | head -10
```
Expected: no `ldap:` block in the rendered Deployment or GatewayConfig.

- [ ] **Step 5: Render with LDAP enabled (smoke check)**

```bash
helm template test deploy/chart/ \
  --set gateway.ldap.enabled=true \
  --set gateway.ldap.url=ldaps://ldap.example.test:636 \
  --set gateway.ldap.bindDN="cn=svc,dc=example,dc=test" \
  --set gateway.ldap.baseDN="dc=example,dc=test" \
  --set gateway.ldap.caSecret.name=devpod-ldap-ca \
  --set gateway.ldap.caSecret.namespace=devpod-system \
  --set gateway.ldap.bindPasswordSecret.name=devpod-ldap-bind \
  --set gateway.ldap.bindPasswordSecret.namespace=devpod-system \
  > /tmp/chart-ldap-on.yaml
grep -B 1 -A 3 'ldap:' /tmp/chart-ldap-on.yaml | head -40
```
Expected: a `spec.ldap` block appears in the GatewayConfig with the
templated values; the Deployment shows the two Secret volume mounts.

- [ ] **Step 6: Commit**

```bash
git add deploy/chart/values.yaml deploy/chart/templates/gateway.yaml deploy/chart/templates/gatewayconfig.yaml
git commit -m "deploy/chart: optional gateway.ldap subtree (Deployment volumes + GatewayConfig render)"
```

---

## Task 14: e2e — `hack/e2e-ldap.sh`

**Files:**
- Create: `hack/e2e-ldap.sh`

This script bootstraps an OpenLDAP Pod inside the existing kind
cluster, configures GatewayConfig.spec.ldap to point at it, and
runs the four paths from spec §8.6.

- [ ] **Step 1: Write the script**

Create `hack/e2e-ldap.sh`:

```bash
#!/usr/bin/env bash
# End-to-end LDAP source demo.
#
# Prereqs:
#   - bash hack/e2e-up.sh has run (kind cluster up, chart installed,
#     gateway image loaded).
#   - kubectl, openssl, ssh-keygen, ssh, nc on PATH.

set -euo pipefail

NS_SYS=devpod-system
NS_DEV=devpods
GW_PORT=2222
WORK=$(mktemp -d)
trap "rm -rf $WORK" EXIT

echo "[1/9] Mint LDAPS server cert + CA, plus three user SSH keys."
openssl req -x509 -newkey ed25519 -nodes -days 1 \
    -subj "/CN=openldap.${NS_SYS}.svc.cluster.local" \
    -addext "subjectAltName=DNS:openldap.${NS_SYS}.svc.cluster.local,DNS:openldap" \
    -keyout "$WORK/ldap-server.key" -out "$WORK/ldap-server.crt" 2>/dev/null
ssh-keygen -q -t ed25519 -f "$WORK/k-alice"     -N "" -C alice@e2e
ssh-keygen -q -t ed25519 -f "$WORK/k-lalice"    -N "" -C lalice@e2e
ssh-keygen -q -t ed25519 -f "$WORK/k-falice-ldap" -N "" -C falice-ldap@e2e

ALICE_PUB=$(cat "$WORK/k-alice.pub")
LALICE_PUB=$(cat "$WORK/k-lalice.pub")
FALICE_LDAP_PUB=$(cat "$WORK/k-falice-ldap.pub")

echo "[2/9] Apply the LDAPS Secrets."
kubectl -n "$NS_SYS" create secret generic devpod-ldap-ca \
    --from-file=ca.crt="$WORK/ldap-server.crt" \
    --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS_SYS" create secret generic devpod-ldap-bind \
    --from-literal=password='svcpass1' \
    --dry-run=client -o yaml | kubectl apply -f -

# OpenLDAP needs the server cert + key + CA inside its own Pod.
kubectl -n "$NS_SYS" create secret generic openldap-tls \
    --from-file=tls.crt="$WORK/ldap-server.crt" \
    --from-file=tls.key="$WORK/ldap-server.key" \
    --from-file=ca.crt="$WORK/ldap-server.crt" \
    --dry-run=client -o yaml | kubectl apply -f -

echo "[3/9] Apply OpenLDAP Deployment + Service + seed LDIF."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata: {name: openldap-seed, namespace: $NS_SYS}
data:
  seed.ldif: |
    dn: ou=People,dc=example,dc=test
    objectClass: organizationalUnit
    ou: People
    dn: ou=System,dc=example,dc=test
    objectClass: organizationalUnit
    ou: System
    dn: cn=svc,ou=System,dc=example,dc=test
    objectClass: applicationProcess
    objectClass: simpleSecurityObject
    cn: svc
    userPassword: svcpass1
    dn: uid=alice,ou=People,dc=example,dc=test
    objectClass: inetOrgPerson
    objectClass: posixAccount
    objectClass: ldapPublicKey
    cn: alice
    sn: alice
    uid: alice
    uidNumber: 1001
    gidNumber: 1001
    homeDirectory: /home/alice
    sshPublicKey: $ALICE_PUB
    dn: uid=lalice,ou=People,dc=example,dc=test
    objectClass: inetOrgPerson
    objectClass: posixAccount
    objectClass: ldapPublicKey
    cn: lalice
    sn: lalice
    uid: lalice
    uidNumber: 1002
    gidNumber: 1002
    homeDirectory: /home/lalice
    sshPublicKey: $LALICE_PUB
    dn: uid=falice,ou=People,dc=example,dc=test
    objectClass: inetOrgPerson
    objectClass: posixAccount
    objectClass: ldapPublicKey
    cn: falice
    sn: falice
    uid: falice
    uidNumber: 1003
    gidNumber: 1003
    homeDirectory: /home/falice
    sshPublicKey: $FALICE_LDAP_PUB
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: openldap, namespace: $NS_SYS}
spec:
  replicas: 1
  selector: {matchLabels: {app: openldap}}
  template:
    metadata: {labels: {app: openldap}}
    spec:
      containers:
      - name: openldap
        image: docker.io/bitnami/openldap:2.6
        env:
        - {name: LDAP_ROOT,           value: "dc=example,dc=test"}
        - {name: LDAP_ADMIN_USERNAME, value: "admin"}
        - {name: LDAP_ADMIN_PASSWORD, value: "adminpass"}
        - {name: LDAP_CUSTOM_LDIF_DIR, value: "/ldif"}
        - {name: LDAP_ENABLE_TLS,     value: "yes"}
        - {name: LDAP_TLS_CERT_FILE,  value: "/tls/tls.crt"}
        - {name: LDAP_TLS_KEY_FILE,   value: "/tls/tls.key"}
        - {name: LDAP_TLS_CA_FILE,    value: "/tls/ca.crt"}
        ports:
        - {name: ldaps, containerPort: 1636}
        volumeMounts:
        - {name: tls,  mountPath: /tls,  readOnly: true}
        - {name: ldif, mountPath: /ldif, readOnly: true}
      volumes:
      - {name: tls,  secret:    {secretName: openldap-tls}}
      - {name: ldif, configMap: {name: openldap-seed}}
---
apiVersion: v1
kind: Service
metadata: {name: openldap, namespace: $NS_SYS}
spec:
  selector: {app: openldap}
  ports:
  - {name: ldaps, port: 636, targetPort: ldaps}
EOF

kubectl -n "$NS_SYS" rollout status deploy/openldap --timeout=120s

echo "[4/9] Patch GatewayConfig with short-TTL spec.ldap (10s/20s for fast tests)."
kubectl patch gatewayconfig default --type=merge -p "$(cat <<EOF
{
  "spec": {
    "ldap": {
      "url": "ldaps://openldap.${NS_SYS}.svc.cluster.local:636",
      "caSecretRef":          {"name": "devpod-ldap-ca",   "namespace": "${NS_SYS}"},
      "bindDN":               "cn=svc,ou=System,dc=example,dc=test",
      "bindPasswordSecretRef":{"name": "devpod-ldap-bind", "namespace": "${NS_SYS}"},
      "baseDN":               "dc=example,dc=test",
      "userFilter":           "(&(objectClass=inetOrgPerson)(uid={{.Username}}))",
      "pubkeyAttribute":      "sshPublicKey",
      "requestTimeoutSeconds":5,
      "cacheTTLSeconds":      10,
      "negativeCacheTTLSeconds":5,
      "staleGraceSeconds":    20
    }
  }
}
EOF
)"

echo "[5/9] Recreate the gateway Deployment to pick up the new spec.ldap + Secrets."
# Upgrade chart with LDAP enabled so the Pod gets the new volume mounts.
helm upgrade --install devpod deploy/chart/ -n "$NS_SYS" \
    --set gateway.ldap.enabled=true \
    --set gateway.ldap.url=ldaps://openldap.${NS_SYS}.svc.cluster.local:636 \
    --set gateway.ldap.bindDN="cn=svc,ou=System,dc=example,dc=test" \
    --set gateway.ldap.baseDN="dc=example,dc=test" \
    --set gateway.ldap.userFilter='(&(objectClass=inetOrgPerson)(uid={{.Username}}))' \
    --set gateway.ldap.cacheTTLSeconds=10 \
    --set gateway.ldap.negativeCacheTTLSeconds=5 \
    --set gateway.ldap.staleGraceSeconds=20 \
    --set gateway.ldap.caSecret.name=devpod-ldap-ca \
    --set gateway.ldap.caSecret.namespace=${NS_SYS} \
    --set gateway.ldap.bindPasswordSecret.name=devpod-ldap-bind \
    --set gateway.ldap.bindPasswordSecret.namespace=${NS_SYS}

kubectl -n "$NS_SYS" rollout restart deploy/devpod-gateway
kubectl -n "$NS_SYS" rollout status deploy/devpod-gateway --timeout=120s

echo "[6/9] Open a port-forward to the gateway."
pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
kubectl -n "$NS_SYS" port-forward svc/devpod-gateway "$GW_PORT:22" >/dev/null 2>&1 &
trap 'pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true; rm -rf "$WORK"' EXIT
deadline=$((SECONDS + 30))
until nc -z 127.0.0.1 "$GW_PORT"; do
    [[ $SECONDS -lt $deadline ]] || { echo "FAIL: gateway port-forward never up"; exit 1; }
    sleep 1
done

ssh_run() {
    local user="$1" pod="$2" key="$3"; shift 3
    ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -p "$GW_PORT" -i "$key" "$user+$pod@127.0.0.1" -- "$@"
}

apply_devpod() {
    local name="$1" owner="$2"
    cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: $name, namespace: $NS_DEV}
spec:
  owner: $owner
  running: true
  pod:
    spec:
      containers:
      - name: dev
        image: gcr.io/distroless/static-debian12
EOF
    kubectl -n "$NS_DEV" wait --for=jsonpath='{.status.phase}'=Running devpod/"$name" --timeout=180s
}

echo "[7/9] Path 1 — CRD user 'alice' (User CRD lists alice's key)."
kubectl apply -f - <<EOF
apiVersion: devpod.io/v1alpha1
kind: User
metadata: {name: alice}
spec:
  pubkeys: ["$ALICE_PUB"]
EOF
apply_devpod alice-pod alice
out=$(ssh_run alice alice-pod "$WORK/k-alice" 'echo OK')
[[ "$out" == "OK" ]] || { echo "FAIL: alice CRD path"; exit 1; }
echo "OK: CRD path"

echo "[8/9] Path 2 — LDAP-only user 'lalice' (no User CRD)."
apply_devpod lalice-pod lalice
out=$(ssh_run lalice lalice-pod "$WORK/k-lalice" 'echo OK')
[[ "$out" == "OK" ]] || { echo "FAIL: lalice LDAP-only path"; exit 1; }
echo "OK: LDAP-only path"

echo "[9/9] Path 3 — fallback. CRD has falice with a placeholder key; LDAP has the real one."
ssh-keygen -q -t ed25519 -f "$WORK/k-falice-decoy" -N "" -C falice-decoy@e2e
kubectl apply -f - <<EOF
apiVersion: devpod.io/v1alpha1
kind: User
metadata: {name: falice}
spec:
  pubkeys: ["$(cat "$WORK/k-falice-decoy.pub")"]
EOF
apply_devpod falice-pod falice
out=$(ssh_run falice falice-pod "$WORK/k-falice-ldap" 'echo OK')
[[ "$out" == "OK" ]] || { echo "FAIL: falice fallback path"; exit 1; }
echo "OK: fallback path"

echo "[bonus] Soft-fail — kill LDAP, prove served-stale within grace window."
kubectl -n "$NS_SYS" scale deploy/openldap --replicas=0
# Cache for lalice is already warm; within staleGraceSeconds the
# auth must still succeed.
sleep 5
out=$(ssh_run lalice lalice-pod "$WORK/k-lalice" 'echo OK')
[[ "$out" == "OK" ]] || { echo "FAIL: stale-grace served-stale"; exit 1; }
echo "OK: served-stale"

echo "[bonus] Wait past CacheTTL+StaleGrace and confirm deny."
sleep 35
if ssh_run lalice lalice-pod "$WORK/k-lalice" 'echo OK' 2>/dev/null; then
    echo "FAIL: stale-grace should have expired"
    exit 1
fi
echo "OK: stale-grace expiration"

# Restore LDAP for any follow-on tests.
kubectl -n "$NS_SYS" scale deploy/openldap --replicas=1
kubectl -n "$NS_SYS" rollout status deploy/openldap --timeout=120s

echo
echo "OK: hack/e2e-ldap.sh — CRD + LDAP-only + fallback + soft-fail all green."
```

- [ ] **Step 2: Make the script executable**

```bash
chmod +x hack/e2e-ldap.sh
```

- [ ] **Step 3: Run the script against a fresh kind cluster**

```bash
bash hack/e2e-up.sh
bash hack/e2e-ldap.sh
```

Expected: every `OK:` line emitted; exit 0.

- [ ] **Step 4: Run the existing v2 e2e for regression**

```bash
bash hack/e2e-v2.sh
```
Expected: clean PASS — LDAP toggle is independent of the v2 SSH path.

- [ ] **Step 5: Commit**

```bash
git add hack/e2e-ldap.sh
git commit -m "hack/e2e-ldap: CRD + LDAP-only + fallback + soft-fail round-trip"
```

---

## Task 15: Final integration check + summary

**Files:** none (verification only)

- [ ] **Step 1: Full unit + envtest suite**

```bash
bash hack/test.sh
```
Expected: all PASS.

- [ ] **Step 2: Lint**

```bash
go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.0 run
```
Expected: clean. Common nits: unused parameter in `_ context.Context` for `NewLDAPSource` — the parameter is reserved for future cancellation of the bind probe; suppress with `_ = ctx` or simply rename to `_` per Go style.

- [ ] **Step 3: All three e2e scripts back-to-back**

```bash
bash hack/e2e-up.sh && \
  bash hack/e2e-v2.sh && \
  bash hack/e2e-v2-shells.sh && \
  bash hack/e2e-ldap.sh
```
Expected: all four green.

- [ ] **Step 4: Verify `kubectl explain` surfaces the new field**

```bash
kubectl explain gatewayconfig.spec.ldap
```
Expected: shows the LDAPSpec description with `url`, `bindDN`, `caSecretRef`, etc.

- [ ] **Step 5: No commit; this task is a gate.**

---

## Self-review summary

- **Spec coverage:**
  - §1 goal & non-goals → embedded in the plan's `Goal` + Task 1's
    field comments + Task 12's wiring (no User CRD changes; no hot
    reload).
  - §2.1 interface → Task 2.
  - §2.2 Authenticator rewiring → Task 3.
  - §2.3 source implementations → Task 2 (crdSource) + Tasks 7-10
    (ldapSource).
  - §2.4 `cmd/gateway` wiring → Task 12.
  - §3 CRD changes → Task 1.
  - §3 Helm chart + RBAC (via SA volume mounts) → Task 13.
  - §4 ldapSource internals → Tasks 7-10.
  - §4.4 filter escape → Task 6.
  - §5 decision table → Task 11.
  - §6.1 audit fields → Task 4.
  - §6.2 metrics → Task 5.
  - §6.4 startup fail-fast → Task 7 (construction validation) +
    Task 12 (LDAP=nil vs missing-files cases).
  - §7 code-change map → File map at top of this plan.
  - §8.1-8.5 unit/envtest/CEL → Tasks 2-12 (envtest auto-regression in
    Task 12 step 6; CEL the `^ldaps://...` pattern is enforced by
    controller-gen in Task 1).
  - §8.6 e2e → Task 14.

- **Placeholder scan:** No TBD/TODO/"handle edge cases" remain. Two
  places explicitly defer cross-task work but supply the full code in
  the later task (the metric increment deferred from Task 2 to Task 5;
  the singleflight wrapper deferred from Task 8 to Task 10).

- **Type consistency:** `IdentitySource.Resolve` signature is identical
  across Tasks 2, 7, 8, 9, 10, 11, 12. `LDAPConfig` field names match
  between Tasks 7 and 12. `AuthPath.Source` / `ServedStale` /
  `LastSourceErr` names match between Tasks 3, 4, and 11.

- **Spec deviations noted inline:**
  - Spec §3 said "RBAC: extend the same Role to the two LDAP
    Secrets". The plan delivers this via volume mounts (chart-side),
    matching how `HostKeyRef` / `InternalKeyRef` already reach the
    gateway. No new ClusterRole/Role is needed; kubelet ingresses
    the Secrets via the existing ServiceAccount.
  - Spec §4.3 algorithm step 3 says "stale-in-grace path serves
    immediately without calling step 4". The implementation in
    Task 9's Resolve does exactly that for the `lastErrAt != 0`
    branch in the upfront cache check.
