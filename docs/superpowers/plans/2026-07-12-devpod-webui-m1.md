# DevPod Web UI — M1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the `devpod-webui` binary: GitLab-OIDC login with prefix
username mapping, first-login User auto-provisioning, pubkey
self-service, DevPod list/detail/hibernate/wake/delete with SSE live
updates, three-way create (preset / custom+overlay / plain) backed by a
new `DevPodTemplate` CRD with server-side Kore-annotation stamping, and
per-user quota enforcement — plus chart, images, and e2e.

**Architecture:** Fourth binary `cmd/webui` (mirrors `cmd/gateway`
wiring: controller-runtime manager for a cached client + plain
`net/http` server). All k8s ops run under the webui ServiceAccount;
every handler enforces session → ownership → quota → execute. React SPA
embedded via `go:embed`. Spec: `docs/superpowers/specs/2026-07-12-devpod-webui-design.md`.

**Tech Stack:** Go 1.25, controller-runtime v0.20, `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` (new deps), envtest, React 18 + TypeScript + Vite + Tailwind v4, distroless images.

## Global Constraints

- No Makefile. Tools run via `go run`; shell lives in `hack/`.
- License header on every Go file: `// Copyright 2026 The DevPod Authors.` + blank + `// SPDX-License-Identifier: Apache-2.0`.
- Codegen: `go generate ./...` (controller-gen v0.16.5 → deepcopy + CRDs + chart sync). Run after ANY `api/v1alpha1` change; commit regenerated files with the change.
- Tests: `bash hack/test.sh` (envtest, `-race`). Package under test: `internal/webui`, external test package `webui_test`.
- DevPod CR names: ≤ 22 chars, must start with `<owner>-` (existing CEL). Budget helper: `NameBudget(owner) = 22 - len(owner) - 1`.
- Kore annotation whitelist (templates may carry ONLY these): `kore.zjusct.io/pin|pool|pool-size|numa-policy|memory-policy|placement|smt-policy`. `kore.zjusct.io/cpuset` is never templated.
- Session cookie name: `devpod_session`. TTL 24h. HMAC-SHA256.
- API error body: `{"code","message","detail"}`; codes are UPPER_SNAKE (`UNAUTHORIZED`, `FORBIDDEN`, `NOT_FOUND`, `BAD_REQUEST`, `KORE_ANNOTATIONS_FORBIDDEN`, `QUOTA_EXCEEDED`, `INTERNAL`).
- Non-admin submissions must NEVER carry `kore.zjusct.io/*` annotations, `spec.vm`, or containers missing cpu/memory limits.

---

## File Structure

| Action | Path | Responsibility |
|--------|------|----------------|
| Modify | `api/v1alpha1/user_types.go` | `UserQuota` type; `UserSpec.Quota`; pubkeys become optional |
| Create | `api/v1alpha1/devpodtemplate_types.go` | `DevPodTemplate` CRD (cluster-scoped): `BindingSpec`, `PodPresetSpec` |
| Regen  | `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/*`, `deploy/chart/crds/*` | controller-gen output |
| Create | `internal/webui/mapping.go` (+`_test`) | `MapUsername`, `NameBudget` |
| Create | `internal/webui/session.go` (+`_test`) | HMAC cookie sessions: `SessionManager.Mint/Verify` |
| Create | `internal/webui/kore.go` (+`_test`) | annotation whitelist, `KoreAnnotationKeys`, `ValidateBinding` |
| Create | `internal/webui/quota.go` (+`_test`) | `EffectiveQuota`, `RequireLimits`, `CheckQuota`, `QuotaError` |
| Create | `internal/webui/template_apply.go` (+`_test`) | `ApplyTemplate` — preset build + binding stamp |
| Create | `internal/webui/oauth.go` (+`oauth_test.go`) | OIDC flow, PKCE, callback, `ensureUser` auto-provision |
| Create | `internal/webui/suite_test.go` | envtest suite for the webui package |
| Create | `internal/webui/api_devpods.go` (+`_test`) | DevPod CRUD handlers, ownership, stamping, quota, events, binding readback |
| Create | `internal/webui/watch.go` (+`_test`) | SSE `/api/devpods?watch=true` from informer events |
| Create | `internal/webui/api_users.go` (+`_test`) | `/api/me`, pubkey GET/PUT |
| Create | `internal/webui/api_templates.go` (+`_test`) | `/api/templates` list (Kore-gated) |
| Create | `internal/webui/server.go` (+`_test`) | routes, auth+origin middleware, static SPA serving |
| Create | `web/embed.go`, `web/dist/index.html` | `go:embed all:dist` + committed placeholder |
| Create | `cmd/webui/main.go` | flags, manager, Kore CRD probe, HTTP server |
| Create | `web/package.json`, `web/vite.config.ts`, `web/tsconfig.json`, `web/index.html`, `web/src/*` | React SPA |
| Create | `hack/build-webui.sh` | npm build → `web/dist` |
| Create | `images/webui/Dockerfile` | node build stage + go build stage + distroless |
| Create | `deploy/chart/templates/webui.yaml` | Deployment/Service/ConfigMap/NetworkPolicy |
| Create | `deploy/chart/templates/webui-rbac.yaml` | SA, Role, ClusterRole, bindings |
| Modify | `deploy/chart/values.yaml` | `image.webui`, `webui:` block |
| Create | `test/fakeidp/main.go` | minimal OIDC issuer for e2e |
| Create | `hack/e2e-webui.sh` | kind e2e: login → create → hibernate → delete |
| Modify | `README.md`, `QUICKSTART.md` | build/e2e commands, webui section |

---

### Task 1: User CRD — quota field, optional pubkeys

**Files:**
- Modify: `api/v1alpha1/user_types.go`
- Regen: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/devpod.io_users.yaml`, `deploy/chart/crds/devpod.io_users.yaml`

**Interfaces:**
- Produces: `devpodv1alpha1.UserQuota{MaxDevPods *int32, Compute corev1.ResourceList, Storage *resource.Quantity}`, `UserSpec.Quota *UserQuota`. Pubkeys may now be empty (webui auto-provisioning creates keyless Users; the gateway already treats "no matching key" as a miss and falls back to LDAP).

- [ ] **Step 1: Edit `api/v1alpha1/user_types.go`**

Replace the imports and `UserSpec` (keep `UserStatus`, `User`, `UserList`, `init` unchanged):

```go
import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UserQuota caps aggregate resources across a User's DevPods. All
// fields are optional; absent fields fall back to the webui's global
// defaults. Enforced by the webui backend only — the controller and
// gateway ignore it entirely (UI-layer policy, not a security
// barrier; see the webui design spec §9).
type UserQuota struct {
	// MaxDevPods limits how many DevPod CRs the user may own.
	//
	// +optional
	MaxDevPods *int32 `json:"maxDevPods,omitempty"`

	// Compute caps the SUM of container resource limits (containers +
	// initContainers) across the user's *running* DevPods. Keys: cpu,
	// memory, and extended resources such as nvidia.com/gpu.
	//
	// +optional
	Compute corev1.ResourceList `json:"compute,omitempty"`

	// Storage caps the SUM of spec.persistence.size across ALL of the
	// user's DevPods, hibernated included (PVCs survive hibernation).
	//
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
}

// UserSpec defines the desired state of a DevPod user.
type UserSpec struct {
	// Pubkeys is the list of OpenSSH-format authorized public keys for
	// this user. May be empty: the webui auto-provisions keyless Users
	// on first OAuth login, and the gateway falls back to LDAP (or
	// denies) when no key matches.
	//
	// +optional
	Pubkeys []string `json:"pubkeys,omitempty"`

	// OIDCSubject is reserved for a future OIDC binding. The v1alpha1
	// controller does nothing with this value.
	//
	// +optional
	OIDCSubject string `json:"oidcSubject,omitempty"`

	// DisplayName is a cosmetic label for UIs.
	//
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Quota caps this user's aggregate DevPod resources. nil = webui
	// global defaults.
	//
	// +optional
	Quota *UserQuota `json:"quota,omitempty"`
}
```

- [ ] **Step 2: Regenerate**

Run: `go generate ./... && go build ./...`
Expected: exit 0; `git status` shows `zz_generated.deepcopy.go`, `config/crd/bases/devpod.io_users.yaml`, `deploy/chart/crds/devpod.io_users.yaml` modified. Verify the CRD YAML now contains `quota:` under the User spec schema and `pubkeys` has no `minItems`.

- [ ] **Step 3: Run existing tests (regression)**

Run: `bash hack/test.sh`
Expected: PASS (no behavior change; MinItems removal only loosens admission).

- [ ] **Step 4: Commit**

```bash
git add api/v1alpha1/user_types.go api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/devpod.io_users.yaml deploy/chart/crds/devpod.io_users.yaml
git commit -m "api: add User.spec.quota, make pubkeys optional"
```

---

### Task 2: DevPodTemplate CRD

**Files:**
- Create: `api/v1alpha1/devpodtemplate_types.go`
- Regen: deepcopy + `config/crd/bases/devpod.io_devpodtemplates.yaml` + chart mirror

**Interfaces:**
- Produces: `DevPodTemplate` (cluster-scoped, shortName `dpt`), `DevPodTemplateSpec{DisplayName, Description string, Binding *BindingSpec, PodPreset *PodPresetSpec}`, `BindingSpec{Annotations map[string]string, Resources corev1.ResourceRequirements}`, `PodPresetSpec{Image string, Resources corev1.ResourceRequirements, Persistence *PersistenceSpec, Shell string}`.

- [ ] **Step 1: Create `api/v1alpha1/devpodtemplate_types.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BindingSpec carries the Kore binding block a template stamps onto
// DevPods created from it: the kore.zjusct.io/* annotations plus the
// container resources the binding implies. The webui validates the
// block against Kore's admission rules at template-save time and is
// the only writer of these annotations on non-admin DevPods.
type BindingSpec struct {
	// Annotations restricted to the kore.zjusct.io/* whitelist
	// (pin, pool, pool-size, numa-policy, memory-policy, placement,
	// smt-policy — NOT cpuset, which stays an admin escape hatch).
	Annotations map[string]string `json:"annotations"`

	// Resources the binding implies for the target container. For
	// pin templates: integer CPU with requests == limits.
	Resources corev1.ResourceRequirements `json:"resources"`
}

// PodPresetSpec fixes the user-visible knobs of a one-click preset.
type PodPresetSpec struct {
	// Image for the single "dev" container.
	//
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Resources for the "dev" container. A Binding on the same
	// template overrides overlapping keys.
	//
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Persistence default for DevPods created from this preset.
	//
	// +optional
	Persistence *PersistenceSpec `json:"persistence,omitempty"`

	// Shell passed through to DevPodSpec.Shell.
	//
	// +optional
	// +kubebuilder:validation:Enum=bash;zsh;fish
	Shell string `json:"shell,omitempty"`
}

// DevPodTemplateSpec defines an admin-curated template. At least one
// of Binding / PodPreset must be set: binding-only templates act as
// overlays users attach to custom DevPods; templates with PodPreset
// are one-click presets.
//
// +kubebuilder:validation:XValidation:rule="has(self.binding) || has(self.podPreset)",message="at least one of binding or podPreset must be set"
type DevPodTemplateSpec struct {
	// DisplayName is the human-readable name shown in the picker.
	//
	// +kubebuilder:validation:MinLength=1
	DisplayName string `json:"displayName"`

	// +optional
	Description string `json:"description,omitempty"`

	// +optional
	Binding *BindingSpec `json:"binding,omitempty"`

	// +optional
	PodPreset *PodPresetSpec `json:"podPreset,omitempty"`
}

// DevPodTemplate is an admin-curated create template. Cluster-scoped;
// read-only for ordinary users (via the webui), CRUD for admins.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=dpt
// +kubebuilder:printcolumn:name="Display",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DevPodTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DevPodTemplateSpec `json:"spec,omitempty"`
}

// DevPodTemplateList is a list of DevPodTemplate.
//
// +kubebuilder:object:root=true
type DevPodTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevPodTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DevPodTemplate{}, &DevPodTemplateList{})
}
```

- [ ] **Step 2: Regenerate and build**

Run: `go generate ./... && go build ./...`
Expected: exit 0; new `config/crd/bases/devpod.io_devpodtemplates.yaml` + chart mirror appear.

- [ ] **Step 3: Run tests**

Run: `bash hack/test.sh`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add api/v1alpha1/devpodtemplate_types.go api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/devpod.io_devpodtemplates.yaml deploy/chart/crds/devpod.io_devpodtemplates.yaml
git commit -m "api: add cluster-scoped DevPodTemplate CRD"
```

---

### Task 3: Username mapping

**Files:**
- Create: `internal/webui/mapping.go`
- Test: `internal/webui/mapping_test.go`

**Interfaces:**
- Produces: `func MapUsername(prefix, gitlabUsername string) (string, error)` (DNS-1123 label + name-budget validation), `func NameBudget(owner string) int`, `const MaxDevPodNameLen = 22`.

- [ ] **Step 1: Write the failing test `internal/webui/mapping_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestMapUsername(t *testing.T) {
	cases := []struct {
		name, prefix, gitlab, want string
		wantErr                    bool
	}{
		{"plain", "", "alice", "alice", false},
		{"prefixed", "gl-", "alice", "gl-alice", false},
		{"uppercase rejected", "", "Alice", "", true},
		{"dot rejected", "gl-", "a.lice", "", true},
		{"underscore rejected", "", "a_lice", "", true},
		{"empty gitlab rejected", "gl-", "", "", true},
		{"no room for pod names", "verylongprefix-", "verylonguser", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := webui.MapUsername(tc.prefix, tc.gitlab)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNameBudget(t *testing.T) {
	if got := webui.NameBudget("gl-alice"); got != 13 { // 22 - 8 - 1
		t.Fatalf("budget = %d, want 13", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webui/ -run TestMapUsername -v`
Expected: FAIL (package does not exist yet).

- [ ] **Step 3: Create `internal/webui/mapping.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package webui implements the DevPod web UI backend: OAuth login,
// session cookies, template-mediated DevPod CRUD, quota enforcement,
// and the JSON API consumed by the embedded SPA.
package webui

import (
	"fmt"
	"regexp"
)

// MaxDevPodNameLen mirrors the CEL length cap on DevPod names.
const MaxDevPodNameLen = 22

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// MapUsername maps a GitLab username to the DevPod user name by
// prepending the configured prefix. The result must be a valid
// DNS-1123 label AND leave at least one character of DevPod name
// budget; otherwise login is refused with an explicit error.
func MapUsername(prefix, gitlabUsername string) (string, error) {
	if gitlabUsername == "" {
		return "", fmt.Errorf("empty GitLab username")
	}
	name := prefix + gitlabUsername
	if !dns1123Label.MatchString(name) {
		return "", fmt.Errorf("mapped username %q is not a valid DNS-1123 label (lowercase alphanumerics and '-' only)", name)
	}
	if NameBudget(name) < 1 {
		return "", fmt.Errorf("mapped username %q leaves no room for DevPod names (limit %d chars incl. %q prefix)", name, MaxDevPodNameLen, name+"-")
	}
	return name, nil
}

// NameBudget returns how many characters remain for the user-chosen
// DevPod name suffix after "<owner>-".
func NameBudget(owner string) int {
	return MaxDevPodNameLen - len(owner) - 1
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webui/ -run 'TestMapUsername|TestNameBudget' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/mapping.go internal/webui/mapping_test.go
git commit -m "webui: username prefix mapping with DNS-1123 + budget checks"
```

---

### Task 4: HMAC cookie sessions

**Files:**
- Create: `internal/webui/session.go`
- Test: `internal/webui/session_test.go`

**Interfaces:**
- Produces: `const SessionCookie = "devpod_session"`, `type Session struct{User string; Admin bool; Expiry int64}`, `func NewSessionManager(key []byte, ttl time.Duration) *SessionManager`, `(*SessionManager) Mint(user string, admin bool, now time.Time) string`, `(*SessionManager) Verify(token string, now time.Time) (Session, error)`.

- [ ] **Step 1: Write the failing test `internal/webui/session_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestSessionRoundtrip(t *testing.T) {
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), 24*time.Hour)
	now := time.Unix(1_800_000_000, 0)

	tok := sm.Mint("gl-alice", true, now)
	sess, err := sm.Verify(tok, now.Add(23*time.Hour))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if sess.User != "gl-alice" || !sess.Admin {
		t.Fatalf("unexpected session: %+v", sess)
	}
}

func TestSessionExpiry(t *testing.T) {
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	now := time.Unix(1_800_000_000, 0)
	tok := sm.Mint("gl-alice", false, now)
	if _, err := sm.Verify(tok, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestSessionTamper(t *testing.T) {
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	now := time.Unix(1_800_000_000, 0)
	tok := sm.Mint("gl-alice", false, now)

	// Flip a payload byte: base64("gl-alice") appears in part 0.
	parts := strings.SplitN(tok, ".", 2)
	mutated := "A" + parts[0][1:] + "." + parts[1]
	if _, err := sm.Verify(mutated, now); err == nil {
		t.Fatal("expected signature error on tampered payload")
	}

	// Different key must reject.
	other := webui.NewSessionManager([]byte("ffffffffffffffffffffffffffffffff"), time.Hour)
	if _, err := other.Verify(tok, now); err == nil {
		t.Fatal("expected signature error under different key")
	}

	// Garbage.
	if _, err := sm.Verify("not-a-token", now); err == nil {
		t.Fatal("expected parse error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webui/ -run TestSession -v`
Expected: FAIL — `NewSessionManager` undefined.

- [ ] **Step 3: Create `internal/webui/session.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SessionCookie is the browser cookie carrying the signed session.
const SessionCookie = "devpod_session"

// Session is the authenticated browser identity. User is the MAPPED
// DevPod username (prefix applied), not the raw GitLab login.
type Session struct {
	User   string `json:"u"`
	Admin  bool   `json:"a"`
	Expiry int64  `json:"e"` // unix seconds
}

// SessionManager mints and verifies HMAC-SHA256-signed session tokens.
// Stateless by design: any replica holding the same key verifies any
// token, so there is no server-side session store and no sticky
// sessions.
type SessionManager struct {
	key []byte
	ttl time.Duration
}

func NewSessionManager(key []byte, ttl time.Duration) *SessionManager {
	return &SessionManager{key: key, ttl: ttl}
}

// TTL returns the configured session lifetime (cookie Max-Age).
func (m *SessionManager) TTL() time.Duration { return m.ttl }

func (m *SessionManager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Mint returns "<b64url(json)>.<b64url(hmac)>".
func (m *SessionManager) Mint(user string, admin bool, now time.Time) string {
	raw, err := json.Marshal(Session{User: user, Admin: admin, Expiry: now.Add(m.ttl).Unix()})
	if err != nil {
		panic(err) // marshal of a plain struct cannot fail
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + m.sign(payload)
}

func (m *SessionManager) Verify(token string, now time.Time) (Session, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Session{}, fmt.Errorf("malformed session token")
	}
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return Session{}, fmt.Errorf("malformed session signature")
	}
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(payload))
	if !hmac.Equal(mac.Sum(nil), want) {
		return Session{}, fmt.Errorf("invalid session signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Session{}, fmt.Errorf("malformed session payload")
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return Session{}, fmt.Errorf("malformed session payload: %w", err)
	}
	if now.Unix() >= s.Expiry {
		return Session{}, fmt.Errorf("session expired")
	}
	return s, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webui/ -run TestSession -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/session.go internal/webui/session_test.go
git commit -m "webui: stateless HMAC-signed cookie sessions"
```

---

### Task 5: Kore annotation rules

**Files:**
- Create: `internal/webui/kore.go`
- Test: `internal/webui/kore_test.go`

**Interfaces:**
- Consumes: `devpodv1alpha1.BindingSpec` (Task 2).
- Produces: `const KorePrefix = "kore.zjusct.io/"`, `func KoreAnnotationKeys(ann map[string]string) []string` (sorted offending keys; empty = clean), `func ValidateBinding(b *devpodv1alpha1.BindingSpec) error`.

- [ ] **Step 1: Write the failing test `internal/webui/kore_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestKoreAnnotationKeys(t *testing.T) {
	got := webui.KoreAnnotationKeys(map[string]string{
		"kore.zjusct.io/pin":  "true",
		"example.com/other":   "x",
		"kore.zjusct.io/pool": "team",
	})
	if len(got) != 2 || got[0] != "kore.zjusct.io/pin" || got[1] != "kore.zjusct.io/pool" {
		t.Fatalf("got %v", got)
	}
	if got := webui.KoreAnnotationKeys(map[string]string{"a": "b"}); len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
	if got := webui.KoreAnnotationKeys(nil); len(got) != 0 {
		t.Fatalf("want empty for nil, got %v", got)
	}
}

func rr(cpuReq, cpuLim string) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	if cpuReq != "" {
		out.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if cpuLim != "" {
		out.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
	}
	return out
}

func TestValidateBinding(t *testing.T) {
	pin := func(ann map[string]string, res corev1.ResourceRequirements) *devpodv1alpha1.BindingSpec {
		return &devpodv1alpha1.BindingSpec{Annotations: ann, Resources: res}
	}
	cases := []struct {
		name    string
		b       *devpodv1alpha1.BindingSpec
		wantErr bool
	}{
		{"pin integer ok", pin(map[string]string{"kore.zjusct.io/pin": "true"}, rr("8", "8")), false},
		{"pin fractional cpu", pin(map[string]string{"kore.zjusct.io/pin": "true"}, rr("1500m", "1500m")), true},
		{"pin requests != limits", pin(map[string]string{"kore.zjusct.io/pin": "true"}, rr("4", "8")), true},
		{"pin missing cpu limit", pin(map[string]string{"kore.zjusct.io/pin": "true"}, corev1.ResourceRequirements{}), true},
		{"pool ok", pin(map[string]string{"kore.zjusct.io/pool": "team-hpl", "kore.zjusct.io/pool-size": "64"}, rr("2", "32")), false},
		{"pool missing size", pin(map[string]string{"kore.zjusct.io/pool": "team-hpl"}, rr("2", "32")), true},
		{"pool bad size", pin(map[string]string{"kore.zjusct.io/pool": "t", "kore.zjusct.io/pool-size": "zero"}, rr("2", "4")), true},
		{"pin and pool exclusive", pin(map[string]string{"kore.zjusct.io/pin": "true", "kore.zjusct.io/pool": "t", "kore.zjusct.io/pool-size": "4"}, rr("4", "4")), true},
		{"neither pin nor pool", pin(map[string]string{"kore.zjusct.io/numa-policy": "single"}, rr("4", "4")), true},
		{"cpuset forbidden in templates", pin(map[string]string{"kore.zjusct.io/pin": "true", "kore.zjusct.io/cpuset": "8-15"}, rr("8", "8")), true},
		{"unknown kore key", pin(map[string]string{"kore.zjusct.io/pin": "true", "kore.zjusct.io/bogus": "x"}, rr("8", "8")), true},
		{"non-kore key forbidden", pin(map[string]string{"kore.zjusct.io/pin": "true", "example.com/x": "y"}, rr("8", "8")), true},
		{"bad numa policy value", pin(map[string]string{"kore.zjusct.io/pin": "true", "kore.zjusct.io/numa-policy": "both"}, rr("8", "8")), true},
		{"good policies", pin(map[string]string{
			"kore.zjusct.io/pin":           "true",
			"kore.zjusct.io/numa-policy":   "preferred",
			"kore.zjusct.io/memory-policy": "strict",
			"kore.zjusct.io/placement":     "scatter",
			"kore.zjusct.io/smt-policy":    "logical",
		}, rr("8", "8")), false},
		{"nil binding", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := webui.ValidateBinding(tc.b)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webui/ -run 'TestKore|TestValidateBinding' -v`
Expected: FAIL — `KoreAnnotationKeys` undefined.

- [ ] **Step 3: Create `internal/webui/kore.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// KorePrefix is the annotation namespace of the Kore CPU-pinning
// system (github.com/zjusct/kore). Non-admin DevPod submissions must
// never carry these annotations; they are stamped server-side from
// DevPodTemplates only.
const KorePrefix = "kore.zjusct.io/"

const (
	annPin          = KorePrefix + "pin"
	annPool         = KorePrefix + "pool"
	annPoolSize     = KorePrefix + "pool-size"
	annNUMAPolicy   = KorePrefix + "numa-policy"
	annMemoryPolicy = KorePrefix + "memory-policy"
	annPlacement    = KorePrefix + "placement"
	annSMTPolicy    = KorePrefix + "smt-policy"
)

// allowedBindingValues is the template whitelist: key → allowed values
// (nil = any non-empty value). kore.zjusct.io/cpuset is deliberately
// absent — explicit core numbers only make sense pinned to a node and
// stay an admin YAML escape hatch.
var allowedBindingValues = map[string][]string{
	annPin:          {"true"},
	annPool:         nil,
	annPoolSize:     nil, // validated as positive integer below
	annNUMAPolicy:   {"single", "preferred", "spread"},
	annMemoryPolicy: {"strict", "preferred"},
	annPlacement:    {"pack", "scatter"},
	annSMTPolicy:    {"full-core", "logical"},
}

// KoreAnnotationKeys returns the sorted kore.zjusct.io/* keys present
// in ann. Non-admin create/patch handlers reject any submission where
// this is non-empty.
func KoreAnnotationKeys(ann map[string]string) []string {
	var out []string
	for k := range ann {
		if strings.HasPrefix(k, KorePrefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateBinding mirrors Kore's admission rules for the template
// binding block, so authoring errors surface at template-save time
// (webui is NOT the real gate — Kore's webhook/scheduler is).
func ValidateBinding(b *devpodv1alpha1.BindingSpec) error {
	if b == nil {
		return fmt.Errorf("binding is nil")
	}
	for k, v := range b.Annotations {
		allowed, ok := allowedBindingValues[k]
		if !ok {
			return fmt.Errorf("annotation %q is not templatable (whitelist: pin, pool, pool-size, numa-policy, memory-policy, placement, smt-policy)", k)
		}
		if allowed == nil {
			if v == "" {
				return fmt.Errorf("annotation %q must not be empty", k)
			}
			continue
		}
		found := false
		for _, a := range allowed {
			if v == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("annotation %q: value %q not in %v", k, v, allowed)
		}
	}

	_, pin := b.Annotations[annPin]
	_, pool := b.Annotations[annPool]
	switch {
	case pin && pool:
		return fmt.Errorf("pin and pool are mutually exclusive")
	case !pin && !pool:
		return fmt.Errorf("binding must set %s or %s", annPin, annPool)
	}

	if pool {
		size, ok := b.Annotations[annPoolSize]
		if !ok {
			return fmt.Errorf("%s requires %s", annPool, annPoolSize)
		}
		if n, err := strconv.Atoi(size); err != nil || n < 1 {
			return fmt.Errorf("%s must be a positive integer, got %q", annPoolSize, size)
		}
	}

	if pin {
		cpu := b.Resources.Limits.Cpu()
		if cpu.IsZero() {
			return fmt.Errorf("pin binding requires an integer cpu limit")
		}
		if cpu.MilliValue()%1000 != 0 {
			return fmt.Errorf("pin binding requires integer cpu, got %s", cpu.String())
		}
		if req := b.Resources.Requests.Cpu(); !req.IsZero() && req.Cmp(*cpu) != 0 {
			return fmt.Errorf("pin binding requires cpu requests == limits (%s != %s)", req.String(), cpu.String())
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webui/ -run 'TestKore|TestValidateBinding' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/kore.go internal/webui/kore_test.go
git commit -m "webui: Kore annotation whitelist and binding validation"
```

---

### Task 6: Quota aggregation and checks

**Files:**
- Create: `internal/webui/quota.go`
- Test: `internal/webui/quota_test.go`

**Interfaces:**
- Consumes: `devpodv1alpha1.UserQuota` (Task 1).
- Produces: `type QuotaViolation{Resource, Requested, Used, Limit string}`, `type QuotaError{Violations []QuotaViolation}` (implements `error`), `func EffectiveQuota(u *devpodv1alpha1.User, def devpodv1alpha1.UserQuota) devpodv1alpha1.UserQuota`, `func RequireLimits(spec *corev1.PodSpec) error`, `func CheckQuota(q devpodv1alpha1.UserQuota, existing []devpodv1alpha1.DevPod, proposed *devpodv1alpha1.DevPod) *QuotaError`, `func PodLimits(spec *corev1.PodSpec) corev1.ResourceList`.
- Semantics (spec §4.1): compute counts limits over containers+initContainers of RUNNING DevPods only; storage counts persistence.size of ALL DevPods; `existing` must exclude the DevPod being updated (caller filters); `proposed` counts toward compute only if `proposed.Spec.Running`.

- [ ] **Step 1: Write the failing test `internal/webui/quota_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func mkDevPod(running bool, cpuLim, memLim, storage string) devpodv1alpha1.DevPod {
	dp := devpodv1alpha1.DevPod{
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "gl-alice",
			Running: running,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "dev",
						Image: "ubuntu:24.04",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(cpuLim),
								corev1.ResourceMemory: resource.MustParse(memLim),
							},
						},
					}},
				},
			},
		},
	}
	if storage != "" {
		dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
			Size:      resource.MustParse(storage),
			MountPath: "/home/dev",
		}
	}
	return dp
}

func quota(maxPods int32, cpu, mem, storage string) devpodv1alpha1.UserQuota {
	q := devpodv1alpha1.UserQuota{MaxDevPods: ptr.To(maxPods), Compute: corev1.ResourceList{}}
	if cpu != "" {
		q.Compute[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		q.Compute[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	if storage != "" {
		s := resource.MustParse(storage)
		q.Storage = &s
	}
	return q
}

func TestCheckQuotaWithinLimits(t *testing.T) {
	existing := []devpodv1alpha1.DevPod{mkDevPod(true, "4", "8Gi", "20Gi")}
	proposed := mkDevPod(true, "4", "8Gi", "20Gi")
	if err := webui.CheckQuota(quota(3, "8", "16Gi", "50Gi"), existing, &proposed); err != nil {
		t.Fatalf("unexpected violation: %v", err)
	}
}

func TestCheckQuotaComputeExceeded(t *testing.T) {
	existing := []devpodv1alpha1.DevPod{mkDevPod(true, "6", "8Gi", "")}
	proposed := mkDevPod(true, "4", "4Gi", "")
	err := webui.CheckQuota(quota(10, "8", "16Gi", ""), existing, &proposed)
	if err == nil {
		t.Fatal("expected cpu violation")
	}
	if len(err.Violations) != 1 || err.Violations[0].Resource != "cpu" {
		t.Fatalf("violations = %+v", err.Violations)
	}
	v := err.Violations[0]
	if v.Requested != "4" || v.Used != "6" || v.Limit != "8" {
		t.Fatalf("triple = %+v", v)
	}
}

func TestCheckQuotaHibernatedComputeFree(t *testing.T) {
	// A hibernated 6-cpu pod does not count toward compute.
	existing := []devpodv1alpha1.DevPod{mkDevPod(false, "6", "8Gi", "")}
	proposed := mkDevPod(true, "4", "4Gi", "")
	if err := webui.CheckQuota(quota(10, "8", "16Gi", ""), existing, &proposed); err != nil {
		t.Fatalf("unexpected violation: %v", err)
	}
}

func TestCheckQuotaStorageCountsHibernated(t *testing.T) {
	// Storage counts even for hibernated DevPods.
	existing := []devpodv1alpha1.DevPod{mkDevPod(false, "1", "1Gi", "40Gi")}
	proposed := mkDevPod(true, "1", "1Gi", "20Gi")
	err := webui.CheckQuota(quota(10, "", "", "50Gi"), existing, &proposed)
	if err == nil || err.Violations[0].Resource != "storage" {
		t.Fatalf("expected storage violation, got %v", err)
	}
}

func TestCheckQuotaMaxDevPods(t *testing.T) {
	existing := []devpodv1alpha1.DevPod{mkDevPod(false, "1", "1Gi", ""), mkDevPod(false, "1", "1Gi", "")}
	proposed := mkDevPod(false, "1", "1Gi", "")
	err := webui.CheckQuota(quota(2, "", "", ""), existing, &proposed)
	if err == nil || err.Violations[0].Resource != "devpods" {
		t.Fatalf("expected devpods violation, got %v", err)
	}
}

func TestCheckQuotaInitContainersCount(t *testing.T) {
	proposed := mkDevPod(true, "4", "4Gi", "")
	proposed.Spec.Pod.Spec.InitContainers = []corev1.Container{{
		Name: "init", Image: "busybox",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("5"),
		}},
	}}
	err := webui.CheckQuota(quota(10, "8", "", ""), nil, &proposed)
	if err == nil || err.Violations[0].Resource != "cpu" {
		t.Fatalf("expected cpu violation incl. initContainers, got %v", err)
	}
}

func TestRequireLimits(t *testing.T) {
	ok := mkDevPod(true, "1", "1Gi", "")
	if err := webui.RequireLimits(&ok.Spec.Pod.Spec); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	missing := mkDevPod(true, "1", "1Gi", "")
	missing.Spec.Pod.Spec.Containers[0].Resources.Limits = nil
	if err := webui.RequireLimits(&missing.Spec.Pod.Spec); err == nil {
		t.Fatal("expected error for missing limits")
	}
}

func TestEffectiveQuota(t *testing.T) {
	def := quota(3, "8", "16Gi", "50Gi")
	if got := webui.EffectiveQuota(&devpodv1alpha1.User{}, def); got.Compute.Cpu().String() != "8" {
		t.Fatalf("nil quota should fall back to defaults, got %+v", got)
	}
	u := &devpodv1alpha1.User{Spec: devpodv1alpha1.UserSpec{Quota: &devpodv1alpha1.UserQuota{MaxDevPods: ptr.To(int32(1))}}}
	got := webui.EffectiveQuota(u, def)
	if *got.MaxDevPods != 1 {
		t.Fatalf("explicit maxDevPods should win, got %d", *got.MaxDevPods)
	}
	if got.Compute.Cpu().String() != "8" {
		t.Fatalf("unset compute should fall back, got %s", got.Compute.Cpu())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webui/ -run 'TestCheckQuota|TestRequireLimits|TestEffectiveQuota' -v`
Expected: FAIL — `CheckQuota` undefined.

- [ ] **Step 3: Create `internal/webui/quota.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// QuotaViolation names one exceeded resource with the
// requested/used/limit triple, pre-formatted for the JSON error body.
type QuotaViolation struct {
	Resource  string `json:"resource"`
	Requested string `json:"requested"`
	Used      string `json:"used"`
	Limit     string `json:"limit"`
}

// QuotaError aggregates all violations of one check so the UI can
// show every exceeded bar at once.
type QuotaError struct {
	Violations []QuotaViolation `json:"violations"`
}

func (e *QuotaError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = fmt.Sprintf("%s: requested %s, used %s, limit %s", v.Resource, v.Requested, v.Used, v.Limit)
	}
	return "quota exceeded: " + strings.Join(parts, "; ")
}

// EffectiveQuota resolves a user's quota field-wise against the
// global defaults: any unset field falls back.
func EffectiveQuota(u *devpodv1alpha1.User, def devpodv1alpha1.UserQuota) devpodv1alpha1.UserQuota {
	out := def
	if u == nil || u.Spec.Quota == nil {
		return out
	}
	q := u.Spec.Quota
	if q.MaxDevPods != nil {
		out.MaxDevPods = q.MaxDevPods
	}
	if q.Compute != nil {
		out.Compute = q.Compute
	}
	if q.Storage != nil {
		out.Storage = q.Storage
	}
	return out
}

// PodLimits sums resource limits over containers + initContainers.
// Deliberately conservative: no max(init, main) refinement (spec §4.1).
func PodLimits(spec *corev1.PodSpec) corev1.ResourceList {
	sum := corev1.ResourceList{}
	add := func(cs []corev1.Container) {
		for _, c := range cs {
			for name, qty := range c.Resources.Limits {
				cur := sum[name]
				cur.Add(qty)
				sum[name] = cur
			}
		}
	}
	add(spec.Containers)
	add(spec.InitContainers)
	return sum
}

// RequireLimits enforces the must-declare-limits rule for non-admin
// DevPods: every container and initContainer needs cpu and memory
// limits, otherwise quota cannot be summed.
func RequireLimits(spec *corev1.PodSpec) error {
	check := func(kind string, cs []corev1.Container) error {
		for _, c := range cs {
			if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
				return fmt.Errorf("%s %q must declare cpu and memory limits (required for quota accounting)", kind, c.Name)
			}
		}
		return nil
	}
	if err := check("container", spec.Containers); err != nil {
		return err
	}
	return check("initContainer", spec.InitContainers)
}

// CheckQuota validates proposed against q given the user's other
// DevPods. existing MUST exclude the DevPod being updated. Compute
// counts running DevPods only (waking therefore re-checks); storage
// counts everything. Returns nil when within quota.
func CheckQuota(q devpodv1alpha1.UserQuota, existing []devpodv1alpha1.DevPod, proposed *devpodv1alpha1.DevPod) *QuotaError {
	var violations []QuotaViolation

	if q.MaxDevPods != nil && int32(len(existing))+1 > *q.MaxDevPods {
		violations = append(violations, QuotaViolation{
			Resource:  "devpods",
			Requested: "1",
			Used:      fmt.Sprintf("%d", len(existing)),
			Limit:     fmt.Sprintf("%d", *q.MaxDevPods),
		})
	}

	if len(q.Compute) > 0 && proposed.Spec.Running && proposed.Spec.Pod != nil {
		used := corev1.ResourceList{}
		for _, dp := range existing {
			if !dp.Spec.Running || dp.Spec.Pod == nil {
				continue
			}
			for name, qty := range PodLimits(&dp.Spec.Pod.Spec) {
				cur := used[name]
				cur.Add(qty)
				used[name] = cur
			}
		}
		requested := PodLimits(&proposed.Spec.Pod.Spec)
		names := make([]string, 0, len(q.Compute))
		for name := range q.Compute {
			names = append(names, string(name))
		}
		sort.Strings(names)
		for _, name := range names {
			rn := corev1.ResourceName(name)
			limit := q.Compute[rn]
			req := requested[rn]
			u := used[rn]
			total := u.DeepCopy()
			total.Add(req)
			if total.Cmp(limit) > 0 {
				violations = append(violations, QuotaViolation{
					Resource:  name,
					Requested: req.String(),
					Used:      u.String(),
					Limit:     limit.String(),
				})
			}
		}
	}

	if q.Storage != nil {
		used := resource.Quantity{}
		for _, dp := range existing {
			if dp.Spec.Persistence != nil {
				used.Add(dp.Spec.Persistence.Size)
			}
		}
		req := resource.Quantity{}
		if proposed.Spec.Persistence != nil {
			req = proposed.Spec.Persistence.Size
		}
		total := used.DeepCopy()
		total.Add(req)
		if total.Cmp(*q.Storage) > 0 {
			violations = append(violations, QuotaViolation{
				Resource:  "storage",
				Requested: req.String(),
				Used:      used.String(),
				Limit:     q.Storage.String(),
			})
		}
	}

	if len(violations) > 0 {
		return &QuotaError{Violations: violations}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webui/ -run 'TestCheckQuota|TestRequireLimits|TestEffectiveQuota' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/quota.go internal/webui/quota_test.go
git commit -m "webui: per-user quota aggregation and enforcement helpers"
```

---

### Task 7: Template application (stamping)

**Files:**
- Create: `internal/webui/template_apply.go`
- Test: `internal/webui/template_apply_test.go`

**Interfaces:**
- Consumes: `DevPodTemplate`/`BindingSpec`/`PodPresetSpec` (Task 2), `ValidateBinding` (Task 5).
- Produces: `func ApplyTemplate(dp *devpodv1alpha1.DevPod, tpl *devpodv1alpha1.DevPodTemplate) error`. Behavior: PodPreset (if set) constructs `dp.Spec.Pod` with a single container `dev` (+persistence/shell defaults, only when the respective DevPod field is unset); Binding (if set) requires `dp.Spec.Pod != nil`, merges binding annotations into `dp.Spec.Pod.Metadata.Annotations`, and overwrites overlapping resource keys on the target container (persistence.targetContainer or containers[0]).

- [ ] **Step 1: Write the failing test `internal/webui/template_apply_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func pinTemplate() *devpodv1alpha1.DevPodTemplate {
	return &devpodv1alpha1.DevPodTemplate{
		Spec: devpodv1alpha1.DevPodTemplateSpec{
			DisplayName: "Pinned 8C",
			Binding: &devpodv1alpha1.BindingSpec{
				Annotations: map[string]string{
					"kore.zjusct.io/pin":         "true",
					"kore.zjusct.io/numa-policy": "single",
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("8"),
						corev1.ResourceMemory: resource.MustParse("16Gi"),
					},
				},
			},
		},
	}
}

func TestApplyBindingOverlay(t *testing.T) {
	dp := mkDevPod(true, "2", "4Gi", "")
	if err := webui.ApplyTemplate(&dp, pinTemplate()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ann := dp.Spec.Pod.Metadata.Annotations
	if ann["kore.zjusct.io/pin"] != "true" || ann["kore.zjusct.io/numa-policy"] != "single" {
		t.Fatalf("annotations not stamped: %v", ann)
	}
	res := dp.Spec.Pod.Spec.Containers[0].Resources
	if res.Limits.Cpu().String() != "8" || res.Requests.Cpu().String() != "8" {
		t.Fatalf("resources not overridden: %+v", res)
	}
	// Memory limit overridden by the binding too.
	if res.Limits.Memory().String() != "16Gi" {
		t.Fatalf("memory = %s", res.Limits.Memory())
	}
}

func TestApplyBindingNeedsPod(t *testing.T) {
	dp := devpodv1alpha1.DevPod{Spec: devpodv1alpha1.DevPodSpec{Owner: "gl-alice"}}
	if err := webui.ApplyTemplate(&dp, pinTemplate()); err == nil {
		t.Fatal("expected error: binding overlay onto DevPod without pod spec")
	}
}

func TestApplyPreset(t *testing.T) {
	tpl := pinTemplate()
	tpl.Spec.PodPreset = &devpodv1alpha1.PodPresetSpec{
		Image: "ghcr.io/example/cuda-dev:12",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
		Persistence: &devpodv1alpha1.PersistenceSpec{Size: resource.MustParse("20Gi"), MountPath: "/home/dev"},
		Shell:       "zsh",
	}
	dp := devpodv1alpha1.DevPod{Spec: devpodv1alpha1.DevPodSpec{Owner: "gl-alice", Running: true}}
	if err := webui.ApplyTemplate(&dp, tpl); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if dp.Spec.Pod == nil || dp.Spec.Pod.Spec.Containers[0].Image != "ghcr.io/example/cuda-dev:12" {
		t.Fatalf("preset pod not built: %+v", dp.Spec.Pod)
	}
	if dp.Spec.Shell != "zsh" || dp.Spec.Persistence == nil {
		t.Fatalf("preset defaults not applied: shell=%q persistence=%v", dp.Spec.Shell, dp.Spec.Persistence)
	}
	// Binding on the same template overrides the preset's cpu/memory.
	if dp.Spec.Pod.Spec.Containers[0].Resources.Limits.Cpu().String() != "8" {
		t.Fatalf("binding should override preset cpu")
	}
}

func TestApplyPresetKeepsUserFields(t *testing.T) {
	tpl := &devpodv1alpha1.DevPodTemplate{Spec: devpodv1alpha1.DevPodTemplateSpec{
		DisplayName: "plain",
		PodPreset:   &devpodv1alpha1.PodPresetSpec{Image: "ubuntu:24.04", Shell: "bash"},
	}}
	dp := devpodv1alpha1.DevPod{Spec: devpodv1alpha1.DevPodSpec{Owner: "gl-alice", Shell: "fish"}}
	if err := webui.ApplyTemplate(&dp, tpl); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if dp.Spec.Shell != "fish" {
		t.Fatalf("user-set shell must win over preset default, got %q", dp.Spec.Shell)
	}
}

func TestApplyInvalidBindingRejected(t *testing.T) {
	tpl := pinTemplate()
	tpl.Spec.Binding.Annotations["kore.zjusct.io/cpuset"] = "0-7"
	dp := mkDevPod(true, "2", "4Gi", "")
	if err := webui.ApplyTemplate(&dp, tpl); err == nil {
		t.Fatal("expected invalid binding to be rejected at apply time")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webui/ -run TestApply -v`
Expected: FAIL — `ApplyTemplate` undefined.

- [ ] **Step 3: Create `internal/webui/template_apply.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// ApplyTemplate stamps tpl onto dp, server-side. This is the ONLY
// code path that writes kore.zjusct.io/* annotations onto non-admin
// DevPods. It re-validates the binding defensively — templates are
// validated at save time, but an out-of-band kubectl edit must not
// let a bad binding through.
//
// Order matters: PodPreset first (it may construct dp.Spec.Pod), then
// Binding (it requires dp.Spec.Pod and overrides resources).
func ApplyTemplate(dp *devpodv1alpha1.DevPod, tpl *devpodv1alpha1.DevPodTemplate) error {
	if tpl.Spec.PodPreset == nil && tpl.Spec.Binding == nil {
		return fmt.Errorf("template %q has neither podPreset nor binding", tpl.Name)
	}

	if p := tpl.Spec.PodPreset; p != nil {
		if dp.Spec.Pod != nil {
			return fmt.Errorf("template %q is a full preset; the request must not carry its own pod spec", tpl.Name)
		}
		dp.Spec.Pod = &devpodv1alpha1.PodWorkloadSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:      "dev",
					Image:     p.Image,
					Resources: *p.Resources.DeepCopy(),
				}},
			},
		}
		if dp.Spec.Persistence == nil && p.Persistence != nil {
			dp.Spec.Persistence = p.Persistence.DeepCopy()
		}
		if dp.Spec.Shell == "" {
			dp.Spec.Shell = p.Shell
		}
	}

	if b := tpl.Spec.Binding; b != nil {
		if err := ValidateBinding(b); err != nil {
			return fmt.Errorf("template %q binding invalid: %w", tpl.Name, err)
		}
		if dp.Spec.Pod == nil {
			return fmt.Errorf("template %q is a binding overlay; the request must supply a pod spec (or use a preset template)", tpl.Name)
		}
		if dp.Spec.Pod.Metadata.Annotations == nil {
			dp.Spec.Pod.Metadata.Annotations = map[string]string{}
		}
		for k, v := range b.Annotations {
			dp.Spec.Pod.Metadata.Annotations[k] = v
		}

		target := &dp.Spec.Pod.Spec.Containers[0]
		if dp.Spec.Persistence != nil && dp.Spec.Persistence.TargetContainer != "" {
			found := false
			for i := range dp.Spec.Pod.Spec.Containers {
				if dp.Spec.Pod.Spec.Containers[i].Name == dp.Spec.Persistence.TargetContainer {
					target = &dp.Spec.Pod.Spec.Containers[i]
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("persistence.targetContainer %q not found", dp.Spec.Persistence.TargetContainer)
			}
		}
		if target.Resources.Requests == nil {
			target.Resources.Requests = corev1.ResourceList{}
		}
		if target.Resources.Limits == nil {
			target.Resources.Limits = corev1.ResourceList{}
		}
		for name, qty := range b.Resources.Requests {
			target.Resources.Requests[name] = qty
		}
		for name, qty := range b.Resources.Limits {
			target.Resources.Limits[name] = qty
		}
	}
	return nil
}
```

Note: `ApplyBindingOverlay` test expects that a binding with only cpu in
Requests still yields `requests.cpu == 8` — covered by the copy loop
above. Empty containers list cannot happen: preset always builds one
container, and the overlay path is called after the request handler has
validated `spec.pod.spec.containers` non-empty (existing CEL also
enforces ≥1).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webui/ -run TestApply -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/template_apply.go internal/webui/template_apply_test.go
git commit -m "webui: server-side template stamping (preset build + binding overlay)"
```

---

### Task 8: OIDC login, PKCE, auto-provisioning (+ webui envtest suite)

**Files:**
- Modify: `go.mod` (add `github.com/coreos/go-oidc/v3`, `golang.org/x/oauth2`)
- Create: `internal/webui/suite_test.go`
- Create: `internal/webui/oauth.go`
- Test: `internal/webui/oauth_test.go`

**Interfaces:**
- Consumes: `MapUsername` (Task 3), `SessionManager` (Task 4).
- Produces: `type OAuthConfig struct{IssuerURL, ClientID, ClientSecret, RedirectURL, UserPrefix string; Admins []string}`, `func NewOAuth(ctx context.Context, cfg OAuthConfig, c client.Client, sm *SessionManager) (*OAuth, error)`, handlers `(*OAuth) HandleLogin/HandleCallback/HandleLogout(w, r)`. Test suite globals for later tasks: `setupSuite(t *testing.T)`, `k8sClient client.Client`, `k8sManager manager.Manager`.

- [ ] **Step 1: Add dependencies**

Run:
```bash
go get github.com/coreos/go-oidc/v3@v3.11.0 golang.org/x/oauth2@latest && go mod tidy
```
Expected: `go.mod` gains both modules; build stays green (`go build ./...`).

- [ ] **Step 2: Create `internal/webui/suite_test.go`**

Mirrors `internal/controllers/suite_test.go` but registers no reconcilers — the webui is a pure API-server client. The manager provides the cached client + informers the handlers use in production.

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

var (
	envTestEnv *envtest.Environment
	k8sClient  client.Client
	k8sManager manager.Manager
	scheme     *runtime.Scheme
)

func setupSuite(t *testing.T) {
	t.Helper()

	scheme = runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))

	envTestEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := envTestEnv.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}

	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	go func() { _ = k8sManager.Start(mgrCtx) }()
	if !k8sManager.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatal("cache sync")
	}
	k8sClient = k8sManager.GetClient()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns := &corev1.Namespace{}
	ns.Name = "devpods"
	_ = k8sClient.Create(ctx, ns)

	t.Cleanup(func() {
		mgrCancel()
		_ = envTestEnv.Stop()
	})
}
```

- [ ] **Step 3: Write the failing test `internal/webui/oauth_test.go`**

A fake OIDC issuer: discovery + JWKS + token endpoint, RS256-signing
id_tokens with a test RSA key. No browser — `HandleLogin` is inspected
for its redirect + cookies, then `HandleCallback` is invoked directly.

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// fakeIssuer is a minimal OIDC provider good enough for go-oidc:
// discovery, JWKS, and a token endpoint that returns an RS256 id_token
// for a fixed username regardless of the code presented.
type fakeIssuer struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	username string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, username: "alice"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/auth",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		pub := &f.key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test",
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idToken := f.signIDToken(t, "webui-client")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": idToken,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) signIDToken(t *testing.T, aud string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": f.srv.URL, "aud": aud, "sub": "42",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"preferred_username": f.username,
	})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	signing := header + "." + payload
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newOAuthForTest(t *testing.T, f *fakeIssuer, prefix string, admins []string) (*webui.OAuth, *webui.SessionManager) {
	t.Helper()
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	o, err := webui.NewOAuth(context.Background(), webui.OAuthConfig{
		IssuerURL: f.srv.URL, ClientID: "webui-client", ClientSecret: "secret",
		RedirectURL: "https://ui.example.com/auth/callback",
		UserPrefix:  prefix, Admins: admins,
	}, k8sClient, sm)
	if err != nil {
		t.Fatalf("NewOAuth: %v", err)
	}
	return o, sm
}

// loginThenCallback drives HandleLogin, carries its cookies + state
// into HandleCallback, and returns the callback response recorder.
func loginThenCallback(t *testing.T, o *webui.OAuth) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	o.HandleLogin(login, httptest.NewRequest("GET", "/auth/login", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("login status = %d", login.Code)
	}
	loc, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	if state == "" || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("missing state or PKCE in %s", loc)
	}

	cb := httptest.NewRequest("GET", "/auth/callback?code=fake&state="+url.QueryEscape(state), nil)
	for _, c := range login.Result().Cookies() {
		cb.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	o.HandleCallback(rec, cb)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == webui.SessionCookie {
			return c
		}
	}
	return nil
}

func TestOAuth(t *testing.T) {
	setupSuite(t)
	f := newFakeIssuer(t)

	t.Run("callback mints session and provisions User", func(t *testing.T) {
		o, sm := newOAuthForTest(t, f, "gl-", nil)
		rec := loginThenCallback(t, o)
		if rec.Code != http.StatusFound {
			t.Fatalf("callback status = %d body=%s", rec.Code, rec.Body)
		}
		c := sessionCookie(rec)
		if c == nil || !c.HttpOnly {
			t.Fatalf("session cookie missing or not HttpOnly: %+v", c)
		}
		sess, err := sm.Verify(c.Value, time.Now())
		if err != nil || sess.User != "gl-alice" || sess.Admin {
			t.Fatalf("sess=%+v err=%v", sess, err)
		}
		var u devpodv1alpha1.User
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "gl-alice"}, &u); err != nil {
			t.Fatalf("auto-provisioned User missing: %v", err)
		}
		// Second login must not fail on AlreadyExists.
		rec2 := loginThenCallback(t, o)
		if rec2.Code != http.StatusFound {
			t.Fatalf("second login status = %d", rec2.Code)
		}
	})

	t.Run("admin list keys off GitLab username", func(t *testing.T) {
		o, sm := newOAuthForTest(t, f, "gl-", []string{"alice"})
		rec := loginThenCallback(t, o)
		sess, _ := sm.Verify(sessionCookie(rec).Value, time.Now())
		if !sess.Admin {
			t.Fatal("expected admin session")
		}
	})

	t.Run("unmappable username refused", func(t *testing.T) {
		f.username = "Bad.Name"
		defer func() { f.username = "alice" }()
		o, _ := newOAuthForTest(t, f, "gl-", nil)
		rec := loginThenCallback(t, o)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "DNS-1123") {
			t.Fatalf("error page should name the reason: %s", rec.Body)
		}
	})

	t.Run("state mismatch rejected", func(t *testing.T) {
		o, _ := newOAuthForTest(t, f, "gl-", nil)
		login := httptest.NewRecorder()
		o.HandleLogin(login, httptest.NewRequest("GET", "/auth/login", nil))
		cb := httptest.NewRequest("GET", "/auth/callback?code=fake&state=WRONG", nil)
		for _, c := range login.Result().Cookies() {
			cb.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		o.HandleCallback(rec, cb)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}
```

(Drop the `fmt` import — nothing in this file uses it; the `newOAuthForTest`
helper sets `SecureCookies: true` implicitly via the https RedirectURL,
see the implementation below.)

- [ ] **Step 4: Run test to verify it fails**

Run: `bash hack/test.sh -run TestOAuth`
Expected: FAIL — `webui.OAuth` undefined. (Use `hack/test.sh` here — the suite needs envtest binaries.)

- [ ] **Step 5: Create `internal/webui/oauth.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const (
	stateCookie = "devpod_oauth_state"
	pkceCookie  = "devpod_oauth_pkce"
)

// OAuthConfig wires the GitLab OIDC client. Admins lists GitLab
// usernames (pre-prefix) that receive the admin bit.
type OAuthConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	UserPrefix   string
	Admins       []string
}

// OAuth implements the GitLab OIDC authorization-code + PKCE flow,
// maps preferred_username through the configured prefix, and
// idempotently provisions the User CR on first login.
type OAuth struct {
	oauth2Cfg oauth2.Config
	verifier  *oidc.IDTokenVerifier
	prefix    string
	admins    map[string]bool
	sessions  *SessionManager
	client    client.Client
	// secure mirrors the RedirectURL scheme: https deployments set
	// the Secure cookie attribute; plain-http (e2e port-forward, dev)
	// must not, or curl/browsers will refuse to send the cookies.
	secure bool
}

func NewOAuth(ctx context.Context, cfg OAuthConfig, c client.Client, sm *SessionManager) (*OAuth, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery %s: %w", cfg.IssuerURL, err)
	}
	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil {
		return nil, fmt.Errorf("redirect url: %w", err)
	}
	admins := make(map[string]bool, len(cfg.Admins))
	for _, a := range cfg.Admins {
		admins[a] = true
	}
	return &OAuth{
		secure: redirect.Scheme == "https",
		oauth2Cfg: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "profile"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		prefix:   cfg.UserPrefix,
		admins:   admins,
		sessions: sm,
		client:   c,
	}, nil
}

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// flowCookie scopes the short-lived state/PKCE cookies to /auth.
func (o *OAuth) flowCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/auth",
		MaxAge: maxAge, HttpOnly: true, Secure: o.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (o *OAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state := randToken()
	pkce := oauth2.GenerateVerifier()
	http.SetCookie(w, o.flowCookie(stateCookie, state, 600))
	http.SetCookie(w, o.flowCookie(pkceCookie, pkce, 600))
	http.Redirect(w, r, o.oauth2Cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(pkce)), http.StatusFound)
}

func (o *OAuth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	stateC, err1 := r.Cookie(stateCookie)
	pkceC, err2 := r.Cookie(pkceCookie)
	if err1 != nil || err2 != nil || r.URL.Query().Get("state") != stateC.Value {
		http.Error(w, "OAuth state mismatch — restart login", http.StatusBadRequest)
		return
	}
	// Consume the flow cookies.
	http.SetCookie(w, o.flowCookie(stateCookie, "", -1))
	http.SetCookie(w, o.flowCookie(pkceCookie, "", -1))

	token, err := o.oauth2Cfg.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(pkceC.Value))
	if err != nil {
		slog.Error("oauth exchange failed", "err", err)
		http.Error(w, "token exchange with GitLab failed", http.StatusBadGateway)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "GitLab response missing id_token", http.StatusBadGateway)
		return
	}
	idToken, err := o.verifier.Verify(r.Context(), rawID)
	if err != nil {
		slog.Error("id_token verify failed", "err", err)
		http.Error(w, "invalid id_token", http.StatusBadGateway)
		return
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "malformed claims", http.StatusBadGateway)
		return
	}

	mapped, err := MapUsername(o.prefix, claims.PreferredUsername)
	if err != nil {
		// Explicit refusal page naming the reason (spec §3).
		http.Error(w, "login refused: "+err.Error(), http.StatusForbidden)
		return
	}
	if err := o.ensureUser(r.Context(), mapped); err != nil {
		slog.Error("auto-provision failed", "user", mapped, "err", err)
		http.Error(w, "user provisioning failed", http.StatusInternalServerError)
		return
	}

	tok := o.sessions.Mint(mapped, o.admins[claims.PreferredUsername], time.Now())
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: tok, Path: "/",
		MaxAge: int(o.sessions.TTL().Seconds()), HttpOnly: true, Secure: o.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (o *OAuth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: o.secure, SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ensureUser idempotently creates the keyless User CR on first login
// (nil quota = global defaults; pubkeys added later via the UI).
func (o *OAuth) ensureUser(ctx context.Context, name string) error {
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := o.client.Create(ctx, u); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `bash hack/test.sh -run TestOAuth`
Expected: PASS (all four subtests).

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/webui/suite_test.go internal/webui/oauth.go internal/webui/oauth_test.go
git commit -m "webui: GitLab OIDC login with PKCE, prefix mapping, User auto-provision"
```

---

### Task 9: DevPod API handlers (CRUD + stamping + quota + readback)

**Files:**
- Create: `internal/webui/api_devpods.go`
- Test: `internal/webui/api_devpods_test.go`

**Interfaces:**
- Consumes: everything from Tasks 3–8.
- Produces: `type Server struct{Client client.Client; Reader client.Reader; Cache cache.Cache; NS string; Sessions *SessionManager; OAuth *OAuth; DefaultQuota devpodv1alpha1.UserQuota; KoreEnabled bool; Origin string}`; handlers `handleListDevPods`, `handleGetDevPod`, `handleCreateDevPod`, `handlePatchDevPod`, `handleDeleteDevPod`, `handleDevPodEvents` (all `http.HandlerFunc` methods); helpers `sessionFrom(r) (Session, bool)`, `writeErr(w, status int, code, msg string, detail any)`, `writeJSON(w, status int, v any)`; request/response types `CreateRequest`, `PatchRequest{Running *bool}`, `DevPodDetail{DevPod devpodv1alpha1.DevPod; Binding *BindingInfo}`, `BindingInfo{AllocatedCpuset, ReservedNUMA, Pool, PoolSize string}`. Routing (method+path→handler) happens in Task 12; tests call handlers directly with Go 1.22 `SetPathValue`.

- [ ] **Step 1: Create `internal/webui/api_devpods.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Server carries the webui backend's dependencies. Handlers run the
// fixed sequence: session → ownership → (mutations) quota → execute
// under the webui ServiceAccount.
type Server struct {
	Client       client.Client // cached manager client
	Reader       client.Reader // uncached APIReader: events, live Pod readback
	Cache        cache.Cache   // informers for the SSE watch (Task 10)
	NS           string        // devpods namespace
	Sessions     *SessionManager
	OAuth        *OAuth
	DefaultQuota devpodv1alpha1.UserQuota
	KoreEnabled  bool
	Origin       string // allowed Origin for mutating requests (Task 12 middleware)
}

type apiBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, status int, code, msg string, detail any) {
	s.writeJSON(w, status, apiBody{Code: code, Message: msg, Detail: detail})
}

// sessionFrom authenticates the request; on failure it has already
// written the 401.
func (s *Server) sessionFrom(w http.ResponseWriter, r *http.Request) (Session, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		s.writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "not logged in", nil)
		return Session{}, false
	}
	sess, err := s.Sessions.Verify(c.Value, time.Now())
	if err != nil {
		s.writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error(), nil)
		return Session{}, false
	}
	return sess, true
}

// ownedDevPods lists the namespace and filters by owner in-process
// (no field index needed at this scale).
func (s *Server) ownedDevPods(r *http.Request, owner string) ([]devpodv1alpha1.DevPod, error) {
	var list devpodv1alpha1.DevPodList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(s.NS)); err != nil {
		return nil, err
	}
	out := list.Items[:0]
	for _, dp := range list.Items {
		if dp.Spec.Owner == owner {
			out = append(out, dp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// getOwned fetches one DevPod and enforces ownership. Non-owners get
// 404 (not 403) to avoid existence leaks.
func (s *Server) getOwned(w http.ResponseWriter, r *http.Request, sess Session, name string) (*devpodv1alpha1.DevPod, bool) {
	var dp devpodv1alpha1.DevPod
	err := s.Client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.NS}, &dp)
	if apierrors.IsNotFound(err) || (err == nil && dp.Spec.Owner != sess.User && !sess.Admin) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "devpod not found", nil)
		return nil, false
	}
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return nil, false
	}
	return &dp, true
}

// rejectKore enforces the stamping invariant for non-admins: no
// kore.zjusct.io/* annotations anywhere in the submitted object.
func (s *Server) rejectKore(w http.ResponseWriter, sess Session, dp *devpodv1alpha1.DevPod) bool {
	if sess.Admin {
		return true
	}
	offending := KoreAnnotationKeys(dp.Annotations)
	if dp.Spec.Pod != nil {
		offending = append(offending, KoreAnnotationKeys(dp.Spec.Pod.Metadata.Annotations)...)
	}
	if len(offending) > 0 {
		s.writeErr(w, http.StatusForbidden, "KORE_ANNOTATIONS_FORBIDDEN",
			"CPU-binding annotations can only come from a template (pick one via templateRef)", offending)
		return false
	}
	return true
}

// checkQuotaFor runs the quota gate for proposed. exclude names a
// DevPod to leave out of "existing" (the one being updated).
func (s *Server) checkQuotaFor(w http.ResponseWriter, r *http.Request, sess Session, proposed *devpodv1alpha1.DevPod, exclude string) bool {
	if sess.Admin {
		return true
	}
	var u devpodv1alpha1.User
	userPtr := &u
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		userPtr = nil // no User CR: defaults apply
	}
	owned, err := s.ownedDevPods(r, sess.User)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return false
	}
	existing := owned[:0]
	for _, dp := range owned {
		if dp.Name != exclude {
			existing = append(existing, dp)
		}
	}
	if qErr := CheckQuota(EffectiveQuota(userPtr, s.DefaultQuota), existing, proposed); qErr != nil {
		s.writeErr(w, http.StatusConflict, "QUOTA_EXCEEDED", qErr.Error(), qErr.Violations)
		return false
	}
	return true
}

// CreateRequest is the POST /api/devpods body. Either YAML (raw
// DevPod manifest) or the structured fields; TemplateRef composes
// with both (preset templates require no pod, overlays require one).
type CreateRequest struct {
	Name        string                           `json:"name,omitempty"` // suffix; full name = "<owner>-<name>"
	TemplateRef string                           `json:"templateRef,omitempty"`
	YAML        string                           `json:"yaml,omitempty"`
	Image       string                           `json:"image,omitempty"`
	CPU         string                           `json:"cpu,omitempty"`
	Memory      string                           `json:"memory,omitempty"`
	Shell       string                           `json:"shell,omitempty"`
	Persistence *devpodv1alpha1.PersistenceSpec  `json:"persistence,omitempty"`
}

func (s *Server) handleListDevPods(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	items, err := s.ownedDevPods(r, sess.User)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// BindingInfo is the Kore readback shown on the detail page: desired
// (pool/pool-size from the stamped annotations) + actual (allocated
// cpuset / reserved NUMA written back by Kore onto the live Pod).
type BindingInfo struct {
	AllocatedCpuset string `json:"allocatedCpuset,omitempty"`
	ReservedNUMA    string `json:"reservedNuma,omitempty"`
	Pool            string `json:"pool,omitempty"`
	PoolSize        string `json:"poolSize,omitempty"`
}

type DevPodDetail struct {
	DevPod  devpodv1alpha1.DevPod `json:"devpod"`
	Binding *BindingInfo          `json:"binding,omitempty"`
}

func (s *Server) bindingInfo(r *http.Request, dp *devpodv1alpha1.DevPod) *BindingInfo {
	if !s.KoreEnabled || dp.Spec.Pod == nil {
		return nil
	}
	desired := dp.Spec.Pod.Metadata.Annotations
	info := &BindingInfo{
		Pool:     desired[KorePrefix+"pool"],
		PoolSize: desired[KorePrefix+"pool-size"],
	}
	if len(KoreAnnotationKeys(desired)) == 0 {
		return nil // unbound DevPod: no panel
	}
	if ref := dp.Status.WorkloadRef; ref != nil && ref.Kind == "Pod" {
		var pod corev1.Pod
		if err := s.Reader.Get(r.Context(), types.NamespacedName{Name: ref.Name, Namespace: s.NS}, &pod); err == nil {
			info.AllocatedCpuset = pod.Annotations[KorePrefix+"allocated-cpuset"]
			info.ReservedNUMA = pod.Annotations[KorePrefix+"reserved-numa"]
		}
	}
	return info
}

func (s *Server) handleGetDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, DevPodDetail{DevPod: *dp, Binding: s.bindingInfo(r, dp)})
}

func (s *Server) handleCreateDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var req CreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}

	dp, apiErr := s.buildDevPod(sess, req)
	if apiErr != nil {
		s.writeErr(w, apiErr.status, apiErr.Code, apiErr.Message, apiErr.Detail)
		return
	}
	if !s.rejectKore(w, sess, dp) {
		return
	}

	if req.TemplateRef != "" {
		var tpl devpodv1alpha1.DevPodTemplate
		if err := s.Client.Get(r.Context(), types.NamespacedName{Name: req.TemplateRef}, &tpl); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("template %q not found", req.TemplateRef), nil)
			return
		}
		if !s.KoreEnabled && tpl.Spec.Binding != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "binding templates are unavailable: Kore is not installed", nil)
			return
		}
		if err := ApplyTemplate(dp, &tpl); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
	}

	if !sess.Admin {
		if dp.Spec.VM != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "vm workloads cannot be metered for quota; admins only", nil)
			return
		}
		if dp.Spec.Pod == nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "spec.pod is required (or pick a preset template)", nil)
			return
		}
		if err := RequireLimits(&dp.Spec.Pod.Spec); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
	}
	if !s.checkQuotaFor(w, r, sess, dp, "") {
		return
	}

	if err := s.Client.Create(r.Context(), dp); err != nil {
		// k8s Status errors (CEL rejections etc.) pass through verbatim.
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "create rejected", err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, dp)
}

type serverErr struct {
	status  int
	Code    string
	Message string
	Detail  any
}

// buildDevPod turns a CreateRequest into the pre-stamping DevPod.
func (s *Server) buildDevPod(sess Session, req CreateRequest) (*devpodv1alpha1.DevPod, *serverErr) {
	if req.YAML != "" {
		var dp devpodv1alpha1.DevPod
		if err := yaml.UnmarshalStrict([]byte(req.YAML), &dp); err != nil {
			return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "invalid YAML: " + err.Error(), nil}
		}
		dp.Namespace = s.NS
		if dp.Spec.Owner == "" {
			dp.Spec.Owner = sess.User
		}
		if !sess.Admin {
			if dp.Spec.Owner != sess.User {
				return nil, &serverErr{http.StatusForbidden, "FORBIDDEN", "owner must be yourself", nil}
			}
			if !strings.HasPrefix(dp.Name, sess.User+"-") {
				return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST",
					fmt.Sprintf("name must start with %q (budget: %d chars after the prefix)", sess.User+"-", NameBudget(sess.User)), nil}
			}
		}
		return &dp, nil
	}

	if req.Name == "" {
		return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", "name is required", nil}
	}
	name := sess.User + "-" + req.Name
	if len(name) > MaxDevPodNameLen || !dns1123Label.MatchString(name) {
		return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST",
			fmt.Sprintf("name %q invalid or too long (max %d chars for the suffix)", name, NameBudget(sess.User)), nil}
	}
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.NS},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:       sess.User,
			Running:     true,
			Shell:       req.Shell,
			Persistence: req.Persistence,
		},
	}
	if req.Image != "" { // absent for preset-template creates
		limits := corev1.ResourceList{}
		for res, val := range map[corev1.ResourceName]string{corev1.ResourceCPU: req.CPU, corev1.ResourceMemory: req.Memory} {
			if val == "" {
				continue
			}
			q, err := resource.ParseQuantity(val)
			if err != nil {
				return nil, &serverErr{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("invalid %s quantity %q", res, val), nil}
			}
			limits[res] = q
		}
		dp.Spec.Pod = &devpodv1alpha1.PodWorkloadSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:      "dev",
					Image:     req.Image,
					Resources: corev1.ResourceRequirements{Limits: limits},
				}},
			},
		}
	}
	return dp, nil
}

// PatchRequest is the PATCH body. M1 supports only the running flag.
type PatchRequest struct {
	Running *bool `json:"running"`
}

func (s *Server) handlePatchDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var req PatchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	if req.Running == nil || *req.Running == dp.Spec.Running {
		s.writeJSON(w, http.StatusOK, dp) // no-op
		return
	}
	dp.Spec.Running = *req.Running
	if *req.Running { // waking re-enters the compute quota
		if !s.checkQuotaFor(w, r, sess, dp, dp.Name) {
			return
		}
	}
	if err := s.Client.Update(r.Context(), dp); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "update rejected", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, dp)
}

func (s *Server) handleDeleteDevPod(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	if err := s.Client.Delete(r.Context(), dp); err != nil && !apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDevPodEvents(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	dp, ok := s.getOwned(w, r, sess, r.PathValue("name"))
	if !ok {
		return
	}
	var events corev1.EventList
	if err := s.Reader.List(r.Context(), &events,
		client.InNamespace(s.NS),
		client.MatchingFields{"involvedObject.name": dp.Name}); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	sort.Slice(events.Items, func(i, j int) bool {
		return events.Items[i].LastTimestamp.Before(&events.Items[j].LastTimestamp)
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"items": events.Items})
}
```

- [ ] **Step 2: Write the failing test `internal/webui/api_devpods_test.go`**

Helpers shared with Tasks 10–12: `newServer(t)`, `forge(sm, user, admin)`, `doJSON(t, handler, method, path, pathVals, cookie, body) *httptest.ResponseRecorder`.

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func newServer(t *testing.T) (*webui.Server, *webui.SessionManager) {
	t.Helper()
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	return &webui.Server{
		Client:   k8sClient,
		Reader:   k8sManager.GetAPIReader(),
		Cache:    k8sManager.GetCache(),
		NS:       "devpods",
		Sessions: sm,
		DefaultQuota: devpodv1alpha1.UserQuota{
			MaxDevPods: ptr.To(int32(5)),
			Compute: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("8"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
		},
		KoreEnabled: true,
	}, sm
}

func forge(sm *webui.SessionManager, user string, admin bool) *http.Cookie {
	return &http.Cookie{Name: webui.SessionCookie, Value: sm.Mint(user, admin, time.Now())}
}

func doJSON(t *testing.T, h http.HandlerFunc, method, target string, pathVals map[string]string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func cleanupDevPods(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var list devpodv1alpha1.DevPodList
	if err := k8sClient.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		_ = k8sClient.Delete(ctx, &list.Items[i])
	}
}

const createBody = `{"name":"dev1","image":"ubuntu:24.04","cpu":"2","memory":"4Gi"}`

func TestDevPodAPI(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)
	bob := forge(sm, "gl-bob", false)
	admin := forge(sm, "gl-root", true)
	_ = admin

	t.Run("create plain custom", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		var dp devpodv1alpha1.DevPod
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "gl-alice-dev1", Namespace: "devpods"}, &dp); err != nil {
			t.Fatalf("CR not created: %v", err)
		}
		if dp.Spec.Owner != "gl-alice" || !dp.Spec.Running {
			t.Fatalf("spec = %+v", dp.Spec)
		}
	})

	t.Run("kore annotations rejected for non-admin YAML", func(t *testing.T) {
		yamlBody := fmt.Sprintf(`{"yaml":%q}`, `
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: gl-alice-pinned
spec:
  owner: gl-alice
  running: true
  pod:
    metadata:
      annotations:
        kore.zjusct.io/pin: "true"
    spec:
      containers:
      - name: dev
        image: ubuntu:24.04
        resources:
          limits: {cpu: "2", memory: "4Gi"}
`)
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, yamlBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "KORE_ANNOTATIONS_FORBIDDEN") {
			t.Fatalf("wrong code: %s", rec.Body)
		}
	})

	t.Run("overlay template stamps annotations", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		tpl := pinTemplate()
		tpl.Name = "pin8"
		if err := k8sClient.Create(context.Background(), tpl); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tpl) })

		body := `{"name":"pinned","image":"ubuntu:24.04","cpu":"2","memory":"4Gi","templateRef":"pin8"}`
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		var dp devpodv1alpha1.DevPod
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "gl-alice-pinned", Namespace: "devpods"}, &dp); err != nil {
			t.Fatal(err)
		}
		if dp.Spec.Pod.Metadata.Annotations["kore.zjusct.io/pin"] != "true" {
			t.Fatalf("annotations = %v", dp.Spec.Pod.Metadata.Annotations)
		}
		if dp.Spec.Pod.Spec.Containers[0].Resources.Limits.Cpu().String() != "8" {
			t.Fatal("binding resources not applied")
		}
	})

	t.Run("quota exceeded on create", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		big := `{"name":"big","image":"ubuntu:24.04","cpu":"6","memory":"8Gi"}`
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, big); rec.Code != http.StatusCreated {
			t.Fatalf("first create: %d %s", rec.Code, rec.Body)
		}
		big2 := `{"name":"big2","image":"ubuntu:24.04","cpu":"6","memory":"8Gi"}`
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, big2)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		var body struct {
			Code   string                  `json:"code"`
			Detail []webui.QuotaViolation  `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != "QUOTA_EXCEEDED" || len(body.Detail) == 0 || body.Detail[0].Resource != "cpu" {
			t.Fatalf("body = %+v", body)
		}
	})

	t.Run("hibernate then wake over quota", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		mk := func(name, cpu string) {
			b := fmt.Sprintf(`{"name":%q,"image":"ubuntu:24.04","cpu":%q,"memory":"1Gi"}`, name, cpu)
			if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, b); rec.Code != http.StatusCreated {
				t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
			}
		}
		mk("a", "6")
		rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/gl-alice-a", map[string]string{"name": "gl-alice-a"}, alice, `{"running":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("hibernate: %d %s", rec.Code, rec.Body)
		}
		mk("b", "6") // fits because a is hibernated
		rec = doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/gl-alice-a", map[string]string{"name": "gl-alice-a"}, alice, `{"running":true}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("wake should exceed quota: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("ownership isolation", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d", rec.Code)
		}
		rec := doJSON(t, s.HandleGetDevPodForTest(), "GET", "/api/devpods/gl-alice-dev1", map[string]string{"name": "gl-alice-dev1"}, bob, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("bob should get 404, got %d", rec.Code)
		}
		rec = doJSON(t, s.HandleDeleteDevPodForTest(), "DELETE", "/api/devpods/gl-alice-dev1", map[string]string{"name": "gl-alice-dev1"}, bob, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("bob delete should 404, got %d", rec.Code)
		}
		rec = doJSON(t, s.HandleListDevPodsForTest(), "GET", "/api/devpods", nil, bob, "")
		if !strings.Contains(rec.Body.String(), `"items":[]`) && strings.Contains(rec.Body.String(), "gl-alice") {
			t.Fatalf("bob sees alice's pods: %s", rec.Body)
		}
	})

	t.Run("missing limits rejected", func(t *testing.T) {
		body := `{"name":"nolim","image":"ubuntu:24.04"}`
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("vm rejected for non-admin", func(t *testing.T) {
		yamlBody := fmt.Sprintf(`{"yaml":%q}`, `
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: gl-alice-vm
spec:
  owner: gl-alice
  running: true
  vm:
    template: {}
`)
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, yamlBody)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("unauthenticated 401", func(t *testing.T) {
		rec := doJSON(t, s.HandleListDevPodsForTest(), "GET", "/api/devpods", nil, nil, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
```

(The `metav1` import in this file is unused — drop it from the import
block; keep `k8s.io/utils/ptr` for the DefaultQuota literal.)

**Export shims:** the test package is external (`webui_test`), and the
handlers are unexported methods. Add this file as part of THIS step —
`internal/webui/export_test.go` (compiled into the `webui` package only
during tests; the standard Go trick for testing unexported methods from
an external test package):

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import "net/http"

// Handler accessors for the external test package. Test-only file.
func (s *Server) HandleCreateDevPodForTest() http.HandlerFunc { return s.handleCreateDevPod }
func (s *Server) HandleGetDevPodForTest() http.HandlerFunc    { return s.handleGetDevPod }
func (s *Server) HandleListDevPodsForTest() http.HandlerFunc  { return s.handleListDevPods }
func (s *Server) HandlePatchDevPodForTest() http.HandlerFunc  { return s.handlePatchDevPod }
func (s *Server) HandleDeleteDevPodForTest() http.HandlerFunc { return s.handleDeleteDevPod }
func (s *Server) HandleDevPodEventsForTest() http.HandlerFunc { return s.handleDevPodEvents }
```

(Adjust the test calls to the exported spelling: `s.HandleCreateDevPodForTest()` etc.)

- [ ] **Step 3: Run tests**

Run: `bash hack/test.sh -run TestDevPodAPI`
Expected: PASS. If the events field-selector test environment complains
about `involvedObject.name`, events are exercised implicitly only —
`handleDevPodEvents` uses the APIReader, which envtest supports.

- [ ] **Step 4: Full test sweep + commit**

Run: `bash hack/test.sh`
Expected: PASS.

```bash
git add internal/webui/api_devpods.go internal/webui/api_devpods_test.go internal/webui/export_test.go
git commit -m "webui: DevPod CRUD API with ownership, template stamping, quota"
```

---

### Task 10: SSE live watch

**Files:**
- Create: `internal/webui/watch.go`
- Test: `internal/webui/watch_test.go`
- Modify: `internal/webui/export_test.go` (add `HandleWatchDevPodsForTest`)

**Interfaces:**
- Consumes: `Server.Cache` (informers), `sessionFrom`.
- Produces: `handleWatchDevPods` — `GET /api/devpods?watch=true` handler streaming `text/event-stream`; each event is `data: {"type":"ADDED|MODIFIED|DELETED","devpod":{...}}\n\n`, filtered to the session user's DevPods. The client reconnects with backoff and re-lists (spec §8); the server therefore does NOT replay state on connect beyond informer add-events for existing objects.

- [ ] **Step 1: Write the failing test `internal/webui/watch_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWatchStreamsOwnedEvents(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWatchDevPodsForTest()))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("GET", srv.URL+"/api/devpods?watch=true", nil)
	req.AddCookie(forge(sm, "gl-alice", false))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	// Create one DevPod for alice and one for bob; only alice's may arrive.
	rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "gl-alice", false), createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec = doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, forge(sm, "gl-bob", false),
		`{"name":"bobpod","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create bob: %d %s", rec.Code, rec.Body)
	}
	t.Cleanup(func() { cleanupDevPods(t) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if l := scanner.Text(); strings.HasPrefix(l, "data: ") {
				lines <- l
			}
		}
	}()
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, "gl-bob-bobpod") {
				t.Fatalf("leaked bob's event to alice: %s", l)
			}
			if strings.Contains(l, "gl-alice-dev1") {
				return // success
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for alice's event")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `bash hack/test.sh -run TestWatchStreams`
Expected: FAIL — `HandleWatchDevPodsForTest` undefined.

- [ ] **Step 3: Create `internal/webui/watch.go`** (+ export shim)

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	toolscache "k8s.io/client-go/tools/cache"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

type watchEvent struct {
	Type   string                 `json:"type"` // ADDED | MODIFIED | DELETED
	DevPod *devpodv1alpha1.DevPod `json:"devpod"`
}

// handleWatchDevPods streams the session user's DevPod changes as
// Server-Sent Events, fed straight from the shared informer — no
// polling. One informer handler per connection; removed on
// disconnect.
func (s *Server) handleWatchDevPods(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported", nil)
		return
	}

	informer, err := s.Cache.GetInformer(r.Context(), &devpodv1alpha1.DevPod{})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}

	events := make(chan watchEvent, 64)
	push := func(typ string, obj any) {
		dp, ok := obj.(*devpodv1alpha1.DevPod)
		if !ok {
			if tomb, isTomb := obj.(toolscache.DeletedFinalStateUnknown); isTomb {
				dp, ok = tomb.Obj.(*devpodv1alpha1.DevPod)
			}
			if !ok {
				return
			}
		}
		if dp.Namespace != s.NS || dp.Spec.Owner != sess.User {
			return
		}
		select {
		case events <- watchEvent{Type: typ, DevPod: dp}:
		default: // slow consumer: drop; the client re-lists on reconnect
		}
	}
	reg, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { push("ADDED", obj) },
		UpdateFunc: func(_, obj any) { push("MODIFIED", obj) },
		DeleteFunc: func(obj any) { push("DELETED", obj) },
	})
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	defer func() { _ = informer.RemoveEventHandler(reg) }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			raw, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
	}
}
```

Append to `internal/webui/export_test.go`:

```go
func (s *Server) HandleWatchDevPodsForTest() http.HandlerFunc { return s.handleWatchDevPods }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `bash hack/test.sh -run TestWatchStreams`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/watch.go internal/webui/watch_test.go internal/webui/export_test.go
git commit -m "webui: SSE live watch from shared informer"
```

---

### Task 11: /api/me, pubkeys, /api/templates

**Files:**
- Create: `internal/webui/api_users.go`, `internal/webui/api_templates.go`
- Test: `internal/webui/api_users_test.go`
- Modify: `internal/webui/export_test.go` (add shims: `HandleMeForTest`, `HandleGetPubkeysForTest`, `HandlePutPubkeysForTest`, `HandleListTemplatesForTest`)

**Interfaces:**
- Consumes: `EffectiveQuota`, `PodLimits`, `ownedDevPods`, session helpers.
- Produces: `handleMe` (GET /api/me → `{user, admin, nameBudget, quota, usage:{devpods, running, compute, storage}}`), `handleGetPubkeys`/`handlePutPubkeys` (GET/PUT /api/me/pubkeys, body `{"pubkeys":["ssh-ed25519 ..."]}`, each key validated with `golang.org/x/crypto/ssh.ParseAuthorizedKey` — dependency already in go.mod), `handleListTemplates` (GET /api/templates; binding-only templates filtered out when `!KoreEnabled`).

- [ ] **Step 1: Write the failing test `internal/webui/api_users_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testPubkey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx6dU5nZm9vYmFyYmF6cXV4Zm9vYmFyYmF6cXV4Zm9v test@example"

func TestMeAndPubkeys(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)

	// ensure User exists (auto-provision normally does this)
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-alice"}}
	_ = k8sClient.Create(context.Background(), u)

	t.Run("me reports identity, quota and usage", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d", rec.Code)
		}
		rec := doJSON(t, s.HandleMeForTest(), "GET", "/api/me", nil, alice, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		var body struct {
			User       string `json:"user"`
			Admin      bool   `json:"admin"`
			NameBudget int    `json:"nameBudget"`
			Usage      struct {
				DevPods int               `json:"devpods"`
				Compute map[string]string `json:"compute"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.User != "gl-alice" || body.NameBudget != 13 || body.Usage.DevPods != 1 || body.Usage.Compute["cpu"] != "2" {
			t.Fatalf("body = %+v", body)
		}
	})

	t.Run("pubkeys roundtrip", func(t *testing.T) {
		rec := doJSON(t, s.HandlePutPubkeysForTest(), "PUT", "/api/me/pubkeys", nil, alice,
			`{"pubkeys":["`+testPubkey+`"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("put: %d %s", rec.Code, rec.Body)
		}
		var u devpodv1alpha1.User
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "gl-alice"}, &u); err != nil {
			t.Fatal(err)
		}
		if len(u.Spec.Pubkeys) != 1 {
			t.Fatalf("pubkeys = %v", u.Spec.Pubkeys)
		}
		rec = doJSON(t, s.HandleGetPubkeysForTest(), "GET", "/api/me/pubkeys", nil, alice, "")
		if rec.Code != http.StatusOK || !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("get: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid pubkey rejected", func(t *testing.T) {
		rec := doJSON(t, s.HandlePutPubkeysForTest(), "PUT", "/api/me/pubkeys", nil, alice,
			`{"pubkeys":["not a key"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestTemplateList(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)

	binding := pinTemplate()
	binding.Name = "pin8-list"
	preset := &devpodv1alpha1.DevPodTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-preset"},
		Spec: devpodv1alpha1.DevPodTemplateSpec{
			DisplayName: "Plain Ubuntu",
			PodPreset:   &devpodv1alpha1.PodPresetSpec{Image: "ubuntu:24.04"},
		},
	}
	ctx := context.Background()
	for _, tpl := range []*devpodv1alpha1.DevPodTemplate{binding, preset} {
		if err := k8sClient.Create(ctx, tpl); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, tpl) })
	}

	rec := doJSON(t, s.HandleListTemplatesForTest(), "GET", "/api/templates", nil, alice, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !json.Valid(rec.Body.Bytes()) || len(rec.Body.String()) == 0 {
		t.Fatal("invalid body")
	}
	both := rec.Body.String()
	if !strings.Contains(both, "pin8-list") || !strings.Contains(both, "plain-preset") {
		t.Fatalf("missing templates: %s", both)
	}

	s.KoreEnabled = false
	rec = doJSON(t, s.HandleListTemplatesForTest(), "GET", "/api/templates", nil, alice, "")
	filtered := rec.Body.String()
	if strings.Contains(filtered, "pin8-list") || !strings.Contains(filtered, "plain-preset") {
		t.Fatalf("kore-off filtering broken: %s", filtered)
	}
	s.KoreEnabled = true
}
```

(Add `strings` to this test file's imports.)

- [ ] **Step 2: Run test to verify it fails**

Run: `bash hack/test.sh -run 'TestMeAndPubkeys|TestTemplateList'`
Expected: FAIL — handlers undefined.

- [ ] **Step 3: Create `internal/webui/api_users.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

type usageInfo struct {
	DevPods int               `json:"devpods"`
	Running int               `json:"running"`
	Compute map[string]string `json:"compute"`
	Storage string            `json:"storage"`
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var u devpodv1alpha1.User
	userPtr := &u
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		userPtr = nil
	}
	owned, err := s.ownedDevPods(r, sess.User)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}

	usage := usageInfo{DevPods: len(owned), Compute: map[string]string{}}
	compute := corev1.ResourceList{}
	storage := resource.Quantity{}
	for _, dp := range owned {
		if dp.Spec.Persistence != nil {
			storage.Add(dp.Spec.Persistence.Size)
		}
		if !dp.Spec.Running || dp.Spec.Pod == nil {
			continue
		}
		usage.Running++
		for name, qty := range PodLimits(&dp.Spec.Pod.Spec) {
			cur := compute[name]
			cur.Add(qty)
			compute[name] = cur
		}
	}
	for name, qty := range compute {
		usage.Compute[string(name)] = qty.String()
	}
	usage.Storage = storage.String()

	s.writeJSON(w, http.StatusOK, map[string]any{
		"user":       sess.User,
		"admin":      sess.Admin,
		"nameBudget": NameBudget(sess.User),
		"quota":      EffectiveQuota(userPtr, s.DefaultQuota),
		"usage":      usage,
	})
}

func (s *Server) handleGetPubkeys(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var u devpodv1alpha1.User
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil && !apierrors.IsNotFound(err) {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"pubkeys": u.Spec.Pubkeys})
}

func (s *Server) handlePutPubkeys(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		Pubkeys []string `json:"pubkeys"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed request body: "+err.Error(), nil)
		return
	}
	for i, k := range req.Pubkeys {
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k)); err != nil {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST",
				fmt.Sprintf("pubkey #%d is not a valid OpenSSH authorized key", i+1), err.Error())
			return
		}
	}
	var u devpodv1alpha1.User
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	u.Spec.Pubkeys = req.Pubkeys
	if err := s.Client.Update(r.Context(), &u); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "update rejected", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"pubkeys": u.Spec.Pubkeys})
}
```

- [ ] **Step 4: Create `internal/webui/api_templates.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"
	"sort"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// handleListTemplates returns every template, binding details
// included — users may SEE bindings, they just can't author them.
// Binding-carrying templates are hidden when Kore is off.
func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionFrom(w, r); !ok {
		return
	}
	var list devpodv1alpha1.DevPodTemplateList
	if err := s.Client.List(r.Context(), &list); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	items := list.Items[:0]
	for _, tpl := range list.Items {
		if !s.KoreEnabled && tpl.Spec.Binding != nil {
			continue
		}
		items = append(items, tpl)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
```

Append shims to `internal/webui/export_test.go`:

```go
func (s *Server) HandleMeForTest() http.HandlerFunc            { return s.handleMe }
func (s *Server) HandleGetPubkeysForTest() http.HandlerFunc    { return s.handleGetPubkeys }
func (s *Server) HandlePutPubkeysForTest() http.HandlerFunc    { return s.handlePutPubkeys }
func (s *Server) HandleListTemplatesForTest() http.HandlerFunc { return s.handleListTemplates }
```

- [ ] **Step 5: Run tests, then full sweep**

Run: `bash hack/test.sh -run 'TestMeAndPubkeys|TestTemplateList'` then `bash hack/test.sh`
Expected: PASS both.

- [ ] **Step 6: Commit**

```bash
git add internal/webui/api_users.go internal/webui/api_templates.go internal/webui/api_users_test.go internal/webui/export_test.go
git commit -m "webui: /api/me, pubkey self-service, template listing"
```

---

### Task 12: Router, middleware, embedded SPA

**Files:**
- Create: `web/embed.go`, `web/dist/index.html` (committed placeholder)
- Create: `internal/webui/server.go`
- Test: `internal/webui/server_test.go`

**Interfaces:**
- Consumes: all handlers from Tasks 8–11, `web.Dist`.
- Produces: `func (s *Server) Routes() http.Handler` — the complete mux. Route table:
  - `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout` → OAuth
  - `GET /api/me`, `GET|PUT /api/me/pubkeys`, `GET /api/templates`
  - `GET /api/devpods` (dispatches to watch when `?watch=true`), `POST /api/devpods`, `GET|PATCH|DELETE /api/devpods/{name}`, `GET /api/devpods/{name}/events`
  - `GET /healthz` → 200 `ok`
  - everything else → embedded SPA (files by path; SPA fallback to `index.html` for extension-less paths; `/api/*` and `/auth/*` never fall through)
  - Mutating methods (POST/PUT/PATCH/DELETE) require `Origin` header absent OR equal to `s.Origin` (belt-and-braces CSRF on top of SameSite=Lax).

- [ ] **Step 1: Create `web/embed.go` and placeholder dist**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package web embeds the built SPA. web/dist ships a committed
// placeholder so `go build ./...` works without Node; the real bundle
// is produced by hack/build-webui.sh (and inside images/webui).
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
```

`web/dist/index.html`:

```html
<!doctype html>
<title>DevPod</title>
<p>Placeholder bundle — run <code>bash hack/build-webui.sh</code> to build the real UI.</p>
```

- [ ] **Step 2: Write the failing test `internal/webui/server_test.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoutes(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.Origin = "https://ui.example.com"
	h := s.Routes()

	do := func(method, path, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := do("GET", "/api/devpods", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated api = %d", rec.Code)
	}
	alice := forge(sm, "gl-alice", false)
	if rec := do("POST", "/api/devpods", "https://evil.example.com", alice); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation = %d, want 403", rec.Code)
	}
	if rec := do("GET", "/", "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>") {
		t.Fatalf("SPA index: %d %s", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}
	if rec := do("GET", "/devpods/some-name", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("SPA fallback = %d", rec.Code)
	}
	if rec := do("GET", "/api/nonexistent", "", alice); rec.Code == http.StatusOK {
		t.Fatal("api paths must not fall through to SPA")
	}
}
```

- [ ] **Step 3: Create `internal/webui/server.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/mrhaoxx/devpod/web"
)

// originGuard rejects mutating cross-origin requests. Browsers send
// Origin on all cross-site and same-site POST/PUT/PATCH/DELETE; a
// missing header means a non-browser client (curl, tests) which the
// cookie requirement already covers.
func (s *Server) originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if o := r.Header.Get("Origin"); o != "" && s.Origin != "" && o != s.Origin {
				s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "cross-origin request rejected", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Routes assembles the full webui handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	if s.OAuth != nil {
		mux.HandleFunc("GET /auth/login", s.OAuth.HandleLogin)
		mux.HandleFunc("GET /auth/callback", s.OAuth.HandleCallback)
		mux.HandleFunc("POST /auth/logout", s.OAuth.HandleLogout)
	}

	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/me/pubkeys", s.handleGetPubkeys)
	mux.HandleFunc("PUT /api/me/pubkeys", s.handlePutPubkeys)
	mux.HandleFunc("GET /api/templates", s.handleListTemplates)
	mux.HandleFunc("GET /api/devpods", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			s.handleWatchDevPods(w, r)
			return
		}
		s.handleListDevPods(w, r)
	})
	mux.HandleFunc("POST /api/devpods", s.handleCreateDevPod)
	mux.HandleFunc("GET /api/devpods/{name}", s.handleGetDevPod)
	mux.HandleFunc("PATCH /api/devpods/{name}", s.handlePatchDevPod)
	mux.HandleFunc("DELETE /api/devpods/{name}", s.handleDeleteDevPod)
	mux.HandleFunc("GET /api/devpods/{name}/events", s.handleDevPodEvents)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint", nil)
	})

	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err) // embed layout is fixed at build time
	}
	files := http.FileServerFS(dist)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve real files (assets with extensions); SPA-fallback the rest.
		if strings.Contains(r.URL.Path, ".") {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFileFS(w, r, dist, "index.html")
	})

	return s.originGuard(mux)
}
```

- [ ] **Step 4: Run test, full sweep, commit**

Run: `bash hack/test.sh -run TestRoutes` then `bash hack/test.sh`
Expected: PASS.

```bash
git add web/embed.go web/dist/index.html internal/webui/server.go internal/webui/server_test.go
git commit -m "webui: router, origin guard, embedded SPA serving"
```

---

### Task 13: cmd/webui binary

**Files:**
- Create: `cmd/webui/main.go`

**Interfaces:**
- Consumes: `webui.Server`, `webui.NewOAuth`, `webui.NewSessionManager`.
- Produces: the `devpod-webui` binary. Flags (spec §5): `--listen` (default `:8080`), `--gitlab-issuer-url`, `--oauth-client-id`, `--oauth-client-secret-file`, `--redirect-url`, `--user-prefix`, `--admins` (comma-separated GitLab usernames), `--session-key-file` (≥32 bytes), `--default-quota-file` (YAML/JSON `UserQuota`), `--devpod-namespace` (default `devpods`), `--kore` (`auto|on|off`, default `auto`), `--tls-cert`/`--tls-key` (optional pair).

- [ ] **Step 1: Create `cmd/webui/main.go`**

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command devpod-webui serves the DevPod web UI: GitLab-OIDC login,
// template-mediated DevPod self-service, quota enforcement, and the
// embedded SPA.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		listen           string
		issuerURL        string
		clientID         string
		clientSecretFile string
		redirectURL      string
		userPrefix       string
		admins           string
		sessionKeyFile   string
		defaultQuotaFile string
		devpodNamespace  string
		koreMode         string
		tlsCert, tlsKey  string
	)
	flag.StringVar(&listen, "listen", ":8080", "HTTP listen address")
	flag.StringVar(&issuerURL, "gitlab-issuer-url", "", "GitLab OIDC issuer URL (required)")
	flag.StringVar(&clientID, "oauth-client-id", "", "OAuth application client id (required)")
	flag.StringVar(&clientSecretFile, "oauth-client-secret-file", "", "file containing the OAuth client secret (required)")
	flag.StringVar(&redirectURL, "redirect-url", "", "external callback URL, e.g. https://devpod.example.com/auth/callback (required)")
	flag.StringVar(&userPrefix, "user-prefix", "", "prefix mapping GitLab usernames to DevPod users")
	flag.StringVar(&admins, "admins", "", "comma-separated GitLab usernames granted admin")
	flag.StringVar(&sessionKeyFile, "session-key-file", "", "file containing the session HMAC key, >= 32 bytes (required)")
	flag.StringVar(&defaultQuotaFile, "default-quota-file", "", "YAML/JSON UserQuota applied to users without spec.quota")
	flag.StringVar(&devpodNamespace, "devpod-namespace", "devpods", "namespace where DevPod objects live")
	flag.StringVar(&koreMode, "kore", "auto", "Kore integration: auto|on|off")
	flag.StringVar(&tlsCert, "tls-cert", "", "optional TLS certificate (default: TLS at the Ingress)")
	flag.StringVar(&tlsKey, "tls-key", "", "optional TLS key")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	for name, v := range map[string]string{
		"--gitlab-issuer-url": issuerURL, "--oauth-client-id": clientID,
		"--oauth-client-secret-file": clientSecretFile, "--redirect-url": redirectURL,
		"--session-key-file": sessionKeyFile,
	} {
		if v == "" {
			fatal(fmt.Errorf("%s is required", name))
		}
	}

	sessionKey, err := os.ReadFile(sessionKeyFile)
	if err != nil || len(sessionKey) < 32 {
		fatal(fmt.Errorf("session key: need >= 32 bytes from %s (err=%v)", sessionKeyFile, err))
	}
	clientSecret, err := os.ReadFile(clientSecretFile)
	if err != nil {
		fatal(fmt.Errorf("client secret: %w", err))
	}

	defaultQuota := devpodv1alpha1.UserQuota{}
	if defaultQuotaFile != "" {
		raw, err := os.ReadFile(defaultQuotaFile)
		if err != nil {
			fatal(fmt.Errorf("default quota: %w", err))
		}
		if err := yaml.UnmarshalStrict(raw, &defaultQuota); err != nil {
			fatal(fmt.Errorf("default quota: %w", err))
		}
	}

	redirect, err := url.Parse(redirectURL)
	if err != nil {
		fatal(fmt.Errorf("redirect-url: %w", err))
	}

	restCfg := ctrl.GetConfigOrDie()

	koreEnabled := false
	switch koreMode {
	case "on":
		koreEnabled = true
	case "off":
	case "auto":
		dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
		if err == nil {
			if _, err := dc.ServerResourcesForGroupVersion("kore.zjusct.io/v1alpha1"); err == nil {
				koreEnabled = true
			}
		}
	default:
		fatal(fmt.Errorf("--kore must be auto|on|off, got %q", koreMode))
	}
	slog.Info("kore integration", "enabled", koreEnabled, "mode", koreMode)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&devpodv1alpha1.DevPod{}: {Namespaces: map[string]cache.Config{devpodNamespace: {}}},
				&corev1.Pod{}:            {Namespaces: map[string]cache.Config{devpodNamespace: {}}},
			},
		},
	})
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sm := webui.NewSessionManager(sessionKey, 24*time.Hour)
	srv := &webui.Server{
		Client:       mgr.GetClient(),
		Reader:       mgr.GetAPIReader(),
		Cache:        mgr.GetCache(),
		NS:           devpodNamespace,
		Sessions:     sm,
		DefaultQuota: defaultQuota,
		KoreEnabled:  koreEnabled,
		Origin:       redirect.Scheme + "://" + redirect.Host,
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fatal(err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		fatal(fmt.Errorf("cache sync failed"))
	}

	oauth, err := webui.NewOAuth(ctx, webui.OAuthConfig{
		IssuerURL:    issuerURL,
		ClientID:     clientID,
		ClientSecret: strings.TrimSpace(string(clientSecret)),
		RedirectURL:  redirectURL,
		UserPrefix:   userPrefix,
		Admins:       splitNonEmpty(admins),
	}, mgr.GetClient(), sm)
	if err != nil {
		fatal(err)
	}
	srv.OAuth = oauth

	httpSrv := &http.Server{Addr: listen, Handler: srv.Routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("devpod-webui listening", "addr", listen)
	if tlsCert != "" {
		err = httpSrv.ListenAndServeTLS(tlsCert, tlsKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(err error) {
	slog.Error("fatal", "err", err)
	os.Exit(1)
}
```

- [ ] **Step 2: Build + vet**

Run: `go build ./... && go vet ./cmd/webui/`
Expected: exit 0.

- [ ] **Step 3: Commit**

```bash
git add cmd/webui/main.go
git commit -m "webui: devpod-webui binary with flags, kore auto-probe"
```

---

### Task 14: Frontend scaffold + login + list

**Files:**
- Create: `web/package.json`, `web/vite.config.ts`, `web/tsconfig.json`, `web/index.html`, `web/src/main.tsx`, `web/src/index.css`, `web/src/api.ts`, `web/src/pages/Login.tsx`, `web/src/pages/PodList.tsx`
- Create: `hack/build-webui.sh`
- Modify: `.gitignore` (add `web/node_modules/`; keep `web/dist/` COMMITTED as placeholder — add `web/dist/*` with `!web/dist/index.html` only if the build starts polluting status; default: leave dist untracked except the placeholder already committed)

**Interfaces:**
- Consumes: JSON API from Tasks 9–12 (`/api/me`, `/api/devpods`, SSE).
- Produces: `web/dist` bundle (built, not committed beyond the placeholder); `api.ts` exports `me()`, `listDevPods()`, `createDevPod(body)`, `patchDevPod(name, body)`, `deleteDevPod(name)`, `getDevPod(name)`, `getEvents(name)`, `listTemplates()`, `getPubkeys()`, `putPubkeys(keys)`, `watchDevPods(onEvent)`, and the `ApiError` type carrying `{code, message, detail}`.

- [ ] **Step 1: Scaffold config files**

`web/package.json`:

```json
{
  "name": "devpod-webui",
  "private": true,
  "version": "0.1.0",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "test": "vitest run"
  },
  "dependencies": {
    "@tanstack/react-query": "^5.51.0",
    "react": "^18.3.1",
    "react-dom": "^18.3.1",
    "react-router-dom": "^6.26.0"
  },
  "devDependencies": {
    "@tailwindcss/vite": "^4.0.0",
    "@types/react": "^18.3.3",
    "@types/react-dom": "^18.3.0",
    "@vitejs/plugin-react": "^4.3.1",
    "tailwindcss": "^4.0.0",
    "typescript": "^5.5.4",
    "vite": "^5.4.0",
    "vitest": "^2.0.0"
  }
}
```

`web/vite.config.ts`:

```ts
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { outDir: "dist" },
  server: { proxy: { "/api": "http://localhost:8080", "/auth": "http://localhost:8080" } },
});
```

`web/tsconfig.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "module": "ESNext",
    "moduleResolution": "bundler",
    "jsx": "react-jsx",
    "strict": true,
    "noEmit": true,
    "skipLibCheck": true
  },
  "include": ["src"]
}
```

`web/index.html`:

```html
<!doctype html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>DevPod</title>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" src="/src/main.tsx"></script>
  </body>
</html>
```

`web/src/index.css`:

```css
@import "tailwindcss";
```

- [ ] **Step 2: Create `web/src/api.ts`**

```ts
export interface ApiError {
  code: string;
  message: string;
  detail?: unknown;
}

export class ApiFailure extends Error {
  constructor(public status: number, public body: ApiError) {
    super(body.message);
  }
}

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const resp = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401) {
    window.location.href = "/login";
    throw new ApiFailure(401, { code: "UNAUTHORIZED", message: "not logged in" });
  }
  if (!resp.ok) {
    throw new ApiFailure(resp.status, (await resp.json()) as ApiError);
  }
  if (resp.status === 204) return undefined as T;
  return (await resp.json()) as T;
}

export interface Me {
  user: string;
  admin: boolean;
  nameBudget: number;
  quota: { maxDevPods?: number; compute?: Record<string, string>; storage?: string };
  usage: { devpods: number; running: number; compute: Record<string, string>; storage: string };
}

// DevPod objects are passed through as loosely-typed JSON; the UI
// reads a handful of paths and must tolerate schema growth.
export type DevPod = {
  metadata: { name: string };
  spec: { owner: string; running: boolean; shell?: string; persistence?: { size: string } };
  status?: { phase?: string; endpoint?: string; message?: string };
};

export type Template = {
  metadata: { name: string };
  spec: {
    displayName: string;
    description?: string;
    binding?: { annotations: Record<string, string>; resources: unknown };
    podPreset?: { image: string };
  };
};

export const me = () => req<Me>("GET", "/api/me");
export const listDevPods = () => req<{ items: DevPod[] }>("GET", "/api/devpods");
export const getDevPod = (n: string) => req<{ devpod: DevPod; binding?: Record<string, string> }>("GET", `/api/devpods/${n}`);
export const createDevPod = (body: unknown) => req<DevPod>("POST", "/api/devpods", body);
export const patchDevPod = (n: string, running: boolean) => req<DevPod>("PATCH", `/api/devpods/${n}`, { running });
export const deleteDevPod = (n: string) => req<void>("DELETE", `/api/devpods/${n}`);
export const getEvents = (n: string) => req<{ items: unknown[] }>("GET", `/api/devpods/${n}/events`);
export const listTemplates = () => req<{ items: Template[] }>("GET", "/api/templates");
export const getPubkeys = () => req<{ pubkeys: string[] | null }>("GET", "/api/me/pubkeys");
export const putPubkeys = (pubkeys: string[]) => req<{ pubkeys: string[] }>("PUT", "/api/me/pubkeys", { pubkeys });

// watchDevPods opens the SSE stream; reconnects with backoff and
// calls onResync after each (re)connect so the caller re-lists
// (events may have been missed while disconnected).
export function watchDevPods(
  onEvent: (type: string, dp: DevPod) => void,
  onResync: () => void,
): () => void {
  let es: EventSource | null = null;
  let stopped = false;
  let delay = 1000;
  const connect = () => {
    if (stopped) return;
    es = new EventSource("/api/devpods?watch=true");
    es.onopen = () => {
      delay = 1000;
      onResync();
    };
    es.onmessage = (m) => {
      const ev = JSON.parse(m.data) as { type: string; devpod: DevPod };
      onEvent(ev.type, ev.devpod);
    };
    es.onerror = () => {
      es?.close();
      if (!stopped) {
        setTimeout(connect, delay);
        delay = Math.min(delay * 2, 30000);
      }
    };
  };
  connect();
  return () => {
    stopped = true;
    es?.close();
  };
}
```

- [ ] **Step 3: Create `web/src/main.tsx`, `Login.tsx`, `PodList.tsx`**

`web/src/main.tsx`:

```tsx
import React from "react";
import ReactDOM from "react-dom/client";
import { createBrowserRouter, RouterProvider } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "./index.css";
import Login from "./pages/Login";
import PodList from "./pages/PodList";
import PodDetail from "./pages/PodDetail";
import PodCreate from "./pages/PodCreate";
import Pubkeys from "./pages/Pubkeys";

const qc = new QueryClient();
const router = createBrowserRouter([
  { path: "/login", element: <Login /> },
  { path: "/", element: <PodList /> },
  { path: "/devpods/new", element: <PodCreate /> },
  { path: "/devpods/:name", element: <PodDetail /> },
  { path: "/settings/pubkeys", element: <Pubkeys /> },
]);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </React.StrictMode>,
);
```

(Note: `PodDetail`, `PodCreate`, `Pubkeys` land in Task 15 — create
stub files exporting `export default () => null` in THIS task so the
build passes, replaced next task.)

`web/src/pages/Login.tsx`:

```tsx
export default function Login() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-50">
      <div className="rounded-xl border bg-white p-10 text-center shadow-sm">
        <h1 className="mb-2 text-2xl font-semibold">DevPod</h1>
        <p className="mb-6 text-sm text-slate-500">Remote development environments</p>
        <a
          href="/auth/login"
          className="rounded-lg bg-orange-600 px-6 py-2 font-medium text-white hover:bg-orange-700"
        >
          Sign in with GitLab
        </a>
      </div>
    </main>
  );
}
```

`web/src/pages/PodList.tsx`:

```tsx
import { useEffect } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { me, listDevPods, patchDevPod, watchDevPods, DevPod } from "../api";

const phaseColor: Record<string, string> = {
  Running: "bg-green-100 text-green-800",
  Pending: "bg-yellow-100 text-yellow-800",
  Stopped: "bg-slate-100 text-slate-600",
  Failed: "bg-red-100 text-red-800",
};

export default function PodList() {
  const qc = useQueryClient();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const podsQ = useQuery({ queryKey: ["devpods"], queryFn: listDevPods });

  useEffect(
    () =>
      watchDevPods(
        () => qc.invalidateQueries({ queryKey: ["devpods"] }),
        () => qc.invalidateQueries({ queryKey: ["devpods"] }),
      ),
    [qc],
  );

  const toggle = useMutation({
    mutationFn: ({ name, running }: { name: string; running: boolean }) => patchDevPod(name, running),
    onSettled: () => qc.invalidateQueries({ queryKey: ["devpods"] }),
  });

  return (
    <main className="mx-auto max-w-4xl p-8">
      <header className="mb-6 flex items-center justify-between">
        <h1 className="text-xl font-semibold">My DevPods</h1>
        <nav className="flex gap-3 text-sm">
          <Link className="text-blue-600 hover:underline" to="/settings/pubkeys">SSH keys</Link>
          <Link className="rounded bg-blue-600 px-3 py-1.5 text-white" to="/devpods/new">New DevPod</Link>
        </nav>
      </header>
      {meQ.data && (
        <p className="mb-4 text-sm text-slate-500">
          {meQ.data.user} · {meQ.data.usage.devpods} pods ({meQ.data.usage.running} running)
          {meQ.data.usage.compute.cpu && ` · cpu ${meQ.data.usage.compute.cpu}/${meQ.data.quota.compute?.cpu ?? "∞"}`}
        </p>
      )}
      <ul className="divide-y rounded-xl border bg-white">
        {(podsQ.data?.items ?? []).map((dp: DevPod) => (
          <li key={dp.metadata.name} className="flex items-center justify-between p-4">
            <div>
              <Link className="font-medium text-blue-700 hover:underline" to={`/devpods/${dp.metadata.name}`}>
                {dp.metadata.name}
              </Link>
              <span className={`ml-3 rounded-full px-2 py-0.5 text-xs ${phaseColor[dp.status?.phase ?? "Pending"]}`}>
                {dp.status?.phase ?? "Pending"}
              </span>
            </div>
            <button
              className="rounded border px-3 py-1 text-sm hover:bg-slate-50"
              onClick={() => toggle.mutate({ name: dp.metadata.name, running: !dp.spec.running })}
            >
              {dp.spec.running ? "Hibernate" : "Wake"}
            </button>
          </li>
        ))}
        {podsQ.data?.items?.length === 0 && (
          <li className="p-8 text-center text-sm text-slate-400">No DevPods yet — create one.</li>
        )}
      </ul>
    </main>
  );
}
```

- [ ] **Step 4: Create `hack/build-webui.sh`**

```bash
#!/usr/bin/env bash
# Build the SPA into web/dist (embedded by web/embed.go). Requires
# Node >= 20. CI and images/webui/Dockerfile run this; the committed
# web/dist/index.html is only a placeholder for pure-Go builds.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)/web"
npm ci
npm run build
```

- [ ] **Step 5: Build and verify**

Run: `bash hack/build-webui.sh && go build ./...`
Expected: Vite build succeeds into `web/dist/`; Go build embeds it.
Then run `git status` — verify only intended files are new (add `web/node_modules/` to `.gitignore` now; leave built `web/dist` files out of the commit: `git add` files explicitly, never `git add web/`).

- [ ] **Step 6: Commit**

```bash
git add web/package.json web/package-lock.json web/vite.config.ts web/tsconfig.json web/index.html web/src/ hack/build-webui.sh .gitignore
git commit -m "webui: React SPA scaffold, login + live pod list"
```

---

### Task 15: Frontend create / detail / pubkeys pages

**Files:**
- Create (replacing Task 14 stubs): `web/src/pages/PodCreate.tsx`, `web/src/pages/PodDetail.tsx`, `web/src/pages/Pubkeys.tsx`

**Interfaces:**
- Consumes: `api.ts` (Task 14). Create flow implements the spec §7 three-way choice; binding is picked via template cards only — there is NO free-form Kore input anywhere in the UI.

- [ ] **Step 1: Create `web/src/pages/PodCreate.tsx`**

```tsx
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { me, listTemplates, createDevPod, ApiFailure, Template } from "../api";

type Mode = "preset" | "custom" | "yaml";

function TemplateCard({ tpl, selected, onClick }: { tpl: Template; selected: boolean; onClick: () => void }) {
  const b = tpl.spec.binding;
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-lg border p-3 text-left text-sm ${selected ? "border-blue-600 ring-1 ring-blue-600" : "hover:border-slate-400"}`}
    >
      <div className="font-medium">{tpl.spec.displayName}</div>
      {tpl.spec.description && <div className="text-xs text-slate-500">{tpl.spec.description}</div>}
      {b && (
        <div className="mt-1 text-xs text-slate-600">
          {b.annotations["kore.zjusct.io/pin"] === "true"
            ? `pinned · ${b.annotations["kore.zjusct.io/numa-policy"] ?? "single"} NUMA`
            : `pool ${b.annotations["kore.zjusct.io/pool"]} (${b.annotations["kore.zjusct.io/pool-size"]} cores)`}
        </div>
      )}
    </button>
  );
}

export default function PodCreate() {
  const nav = useNavigate();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const tplQ = useQuery({ queryKey: ["templates"], queryFn: listTemplates });
  const [mode, setMode] = useState<Mode>("custom");
  const [name, setName] = useState("");
  const [image, setImage] = useState("ubuntu:24.04");
  const [cpu, setCpu] = useState("2");
  const [memory, setMemory] = useState("4Gi");
  const [shell, setShell] = useState("");
  const [persist, setPersist] = useState("");
  const [tplRef, setTplRef] = useState("");
  const [yamlText, setYamlText] = useState("");
  const [err, setErr] = useState<string | null>(null);

  const templates = tplQ.data?.items ?? [];
  const presets = templates.filter((t) => t.spec.podPreset);
  const overlays = templates.filter((t) => t.spec.binding && !t.spec.podPreset);
  const budget = meQ.data?.nameBudget ?? 21;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr(null);
    try {
      const body: Record<string, unknown> = { name };
      if (mode === "yaml") {
        delete body.name;
        body.yaml = yamlText;
        if (tplRef) body.templateRef = tplRef;
      } else if (mode === "preset") {
        body.templateRef = tplRef;
      } else {
        body.image = image;
        body.cpu = cpu;
        body.memory = memory;
        if (shell) body.shell = shell;
        if (persist) body.persistence = { size: persist, mountPath: "/home/dev" };
        if (tplRef) body.templateRef = tplRef;
      }
      const dp = await createDevPod(body);
      nav(`/devpods/${dp.metadata.name}`);
    } catch (e) {
      setErr(e instanceof ApiFailure ? `${e.body.message}${e.body.detail ? ` — ${JSON.stringify(e.body.detail)}` : ""}` : String(e));
    }
  };

  return (
    <main className="mx-auto max-w-2xl p-8">
      <h1 className="mb-6 text-xl font-semibold">New DevPod</h1>
      <div className="mb-4 flex gap-2 text-sm">
        {(["preset", "custom", "yaml"] as Mode[]).map((m) => (
          <button
            key={m}
            onClick={() => { setMode(m); setTplRef(""); }}
            className={`rounded px-3 py-1 ${mode === m ? "bg-blue-600 text-white" : "border"}`}
          >
            {m === "preset" ? "Preset" : m === "custom" ? "Custom" : "YAML"}
          </button>
        ))}
      </div>

      <form onSubmit={submit} className="space-y-4">
        {mode !== "yaml" && (
          <label className="block text-sm">
            Name suffix ({budget - name.length} chars left)
            <input value={name} onChange={(e) => setName(e.target.value)} maxLength={budget}
              className="mt-1 w-full rounded border px-2 py-1" required />
            <span className="text-xs text-slate-400">{meQ.data?.user}-{name || "…"}</span>
          </label>
        )}

        {mode === "preset" && (
          <div className="grid grid-cols-2 gap-2">
            {presets.map((t) => (
              <TemplateCard key={t.metadata.name} tpl={t} selected={tplRef === t.metadata.name}
                onClick={() => setTplRef(t.metadata.name)} />
            ))}
            {presets.length === 0 && <p className="col-span-2 text-sm text-slate-400">No presets published.</p>}
          </div>
        )}

        {mode === "custom" && (
          <>
            <label className="block text-sm">Image
              <input value={image} onChange={(e) => setImage(e.target.value)} className="mt-1 w-full rounded border px-2 py-1" required />
            </label>
            <div className="grid grid-cols-2 gap-3">
              <label className="block text-sm">CPU limit
                <input value={cpu} onChange={(e) => setCpu(e.target.value)} className="mt-1 w-full rounded border px-2 py-1" required />
              </label>
              <label className="block text-sm">Memory limit
                <input value={memory} onChange={(e) => setMemory(e.target.value)} className="mt-1 w-full rounded border px-2 py-1" required />
              </label>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <label className="block text-sm">Shell (optional)
                <select value={shell} onChange={(e) => setShell(e.target.value)} className="mt-1 w-full rounded border px-2 py-1">
                  <option value="">image default</option>
                  <option>bash</option><option>zsh</option><option>fish</option>
                </select>
              </label>
              <label className="block text-sm">Home volume (optional, e.g. 20Gi)
                <input value={persist} onChange={(e) => setPersist(e.target.value)} className="mt-1 w-full rounded border px-2 py-1" />
              </label>
            </div>
            {overlays.length > 0 && (
              <fieldset className="text-sm">
                <legend className="mb-1">CPU binding (admin-curated)</legend>
                <div className="grid grid-cols-2 gap-2">
                  <button type="button" onClick={() => setTplRef("")}
                    className={`rounded-lg border p-3 text-left ${tplRef === "" ? "border-blue-600 ring-1 ring-blue-600" : ""}`}>
                    <div className="font-medium">None</div>
                    <div className="text-xs text-slate-500">shared cores</div>
                  </button>
                  {overlays.map((t) => (
                    <TemplateCard key={t.metadata.name} tpl={t} selected={tplRef === t.metadata.name}
                      onClick={() => setTplRef(t.metadata.name)} />
                  ))}
                </div>
              </fieldset>
            )}
          </>
        )}

        {mode === "yaml" && (
          <textarea value={yamlText} onChange={(e) => setYamlText(e.target.value)} rows={16}
            className="w-full rounded border p-2 font-mono text-xs" placeholder="apiVersion: devpod.io/v1alpha1&#10;kind: DevPod&#10;..." />
        )}

        {err && <p className="rounded bg-red-50 p-3 text-sm text-red-700">{err}</p>}
        <button className="rounded bg-blue-600 px-4 py-2 text-sm text-white" type="submit"
          disabled={mode === "preset" && !tplRef}>Create</button>
      </form>
    </main>
  );
}
```

- [ ] **Step 2: Create `web/src/pages/PodDetail.tsx`**

```tsx
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { getDevPod, getEvents, patchDevPod, deleteDevPod } from "../api";

export default function PodDetail() {
  const { name = "" } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["devpod", name], queryFn: () => getDevPod(name), refetchInterval: 5000 });
  const ev = useQuery({ queryKey: ["events", name], queryFn: () => getEvents(name), refetchInterval: 10000 });

  const toggle = useMutation({
    mutationFn: (running: boolean) => patchDevPod(name, running),
    onSettled: () => qc.invalidateQueries({ queryKey: ["devpod", name] }),
  });
  const del = useMutation({
    mutationFn: () => deleteDevPod(name),
    onSuccess: () => nav("/"),
  });

  if (!q.data) return <main className="p-8 text-sm text-slate-400">Loading…</main>;
  const dp = q.data.devpod;
  const binding = q.data.binding;

  return (
    <main className="mx-auto max-w-3xl p-8">
      <Link to="/" className="text-sm text-blue-600 hover:underline">← My DevPods</Link>
      <header className="mb-6 mt-2 flex items-center justify-between">
        <h1 className="text-xl font-semibold">{dp.metadata.name}</h1>
        <div className="flex gap-2">
          <button className="rounded border px-3 py-1.5 text-sm" onClick={() => toggle.mutate(!dp.spec.running)}>
            {dp.spec.running ? "Hibernate" : "Wake"}
          </button>
          <button className="rounded border border-red-300 px-3 py-1.5 text-sm text-red-700"
            onClick={() => { if (confirm(`Delete ${dp.metadata.name}? PVC data is lost.`)) del.mutate(); }}>
            Delete
          </button>
        </div>
      </header>

      <dl className="mb-6 grid grid-cols-2 gap-x-8 gap-y-2 rounded-xl border bg-white p-4 text-sm">
        <dt className="text-slate-500">Phase</dt><dd>{dp.status?.phase ?? "Pending"}</dd>
        <dt className="text-slate-500">Endpoint</dt><dd className="font-mono">{dp.status?.endpoint ?? "—"}</dd>
        <dt className="text-slate-500">SSH</dt>
        <dd className="font-mono text-xs">ssh {dp.spec.owner}+{dp.metadata.name.slice(dp.spec.owner.length + 1)}@&lt;gateway&gt;</dd>
        {dp.status?.message && (<><dt className="text-slate-500">Message</dt><dd className="text-red-700">{dp.status.message}</dd></>)}
      </dl>

      {binding && (
        <section className="mb-6 rounded-xl border bg-white p-4 text-sm">
          <h2 className="mb-2 font-medium">CPU binding (Kore)</h2>
          <dl className="grid grid-cols-2 gap-x-8 gap-y-2">
            {binding.allocatedCpuset && (<><dt className="text-slate-500">Allocated cores</dt><dd className="font-mono">{binding.allocatedCpuset}</dd></>)}
            {binding.reservedNuma && (<><dt className="text-slate-500">NUMA zone</dt><dd>{binding.reservedNuma}</dd></>)}
            {binding.pool && (<><dt className="text-slate-500">Pool</dt><dd>{binding.pool} ({binding.poolSize} cores)</dd></>)}
            {!binding.allocatedCpuset && !binding.pool && (<><dt className="text-slate-500">State</dt><dd>binding pending</dd></>)}
          </dl>
        </section>
      )}

      <section className="rounded-xl border bg-white p-4 text-sm">
        <h2 className="mb-2 font-medium">Events</h2>
        <ul className="space-y-1 font-mono text-xs text-slate-600">
          {((ev.data?.items ?? []) as { reason?: string; message?: string }[]).map((e, i) => (
            <li key={i}>{e.reason}: {e.message}</li>
          ))}
          {ev.data?.items?.length === 0 && <li className="text-slate-400">none</li>}
        </ul>
      </section>
    </main>
  );
}
```

- [ ] **Step 3: Create `web/src/pages/Pubkeys.tsx`**

```tsx
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { me, getPubkeys, putPubkeys, ApiFailure } from "../api";

export default function Pubkeys() {
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const [text, setText] = useState("");
  const [msg, setMsg] = useState<string | null>(null);

  useEffect(() => {
    getPubkeys().then((r) => setText((r.pubkeys ?? []).join("\n")));
  }, []);

  const save = async () => {
    setMsg(null);
    try {
      const keys = text.split("\n").map((l) => l.trim()).filter(Boolean);
      await putPubkeys(keys);
      setMsg(`Saved ${keys.length} key(s).`);
    } catch (e) {
      setMsg(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  return (
    <main className="mx-auto max-w-2xl p-8">
      <Link to="/" className="text-sm text-blue-600 hover:underline">← My DevPods</Link>
      <h1 className="mb-2 mt-2 text-xl font-semibold">SSH public keys</h1>
      <p className="mb-4 text-sm text-slate-500">
        One key per line. Then connect with{" "}
        <code className="rounded bg-slate-100 px-1">ssh {meQ.data?.user}+&lt;pod&gt;@&lt;gateway&gt;</code>
      </p>
      <textarea value={text} onChange={(e) => setText(e.target.value)} rows={8}
        className="w-full rounded border p-2 font-mono text-xs" placeholder="ssh-ed25519 AAAA… laptop" />
      <div className="mt-3 flex items-center gap-3">
        <button onClick={save} className="rounded bg-blue-600 px-4 py-2 text-sm text-white">Save</button>
        {msg && <span className="text-sm text-slate-600">{msg}</span>}
      </div>
    </main>
  );
}
```

- [ ] **Step 4: Build**

Run: `bash hack/build-webui.sh && go build ./...`
Expected: clean build, no TS errors.

- [ ] **Step 5: Commit**

```bash
git add web/src/pages/PodCreate.tsx web/src/pages/PodDetail.tsx web/src/pages/Pubkeys.tsx
git commit -m "webui: create (preset/custom/yaml), detail with Kore readback, pubkeys page"
```

---

### Task 16: Image + Helm chart + docs

**Files:**
- Create: `images/webui/Dockerfile`
- Create: `deploy/chart/templates/webui.yaml`, `deploy/chart/templates/webui-rbac.yaml`
- Modify: `deploy/chart/values.yaml`, `README.md`, `QUICKSTART.md`

- [ ] **Step 1: Create `images/webui/Dockerfile`**

```dockerfile
# syntax=docker/dockerfile:1
FROM node:22-alpine AS spa
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=spa /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -o /devpod-webui ./cmd/webui

FROM gcr.io/distroless/static
COPY --from=build /devpod-webui /usr/local/bin/devpod-webui
ENTRYPOINT ["/usr/local/bin/devpod-webui"]
```

- [ ] **Step 2: Add values (`deploy/chart/values.yaml`)**

Append under `image:`:

```yaml
  webui:
    repository: ghcr.io/mrhaoxx/devpod-webui
    tag: dev
    pullPolicy: IfNotPresent
```

Append at top level:

```yaml
webui:
  enabled: false
  replicas: 2
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits:   {cpu: 500m, memory: 512Mi}
  service:
    type: ClusterIP
    port: 80
  # External URL of the UI; the OAuth redirect URL is baseURL + /auth/callback.
  baseURL: ""
  userPrefix: ""
  admins: []                       # GitLab usernames granted admin
  kore: auto                       # auto | on | off
  gitlab:
    issuerURL: ""
    clientID: ""
    # Secret with key "client-secret"; must exist in namespaces.system.
    clientSecretSecret: {name: ""}
  # Secret with key "session-key" (>= 32 random bytes); must pre-exist.
  sessionKeySecret: {name: ""}
  defaultQuota:
    maxDevPods: 3
    compute: {cpu: "8", memory: "16Gi"}
    storage: "50Gi"
```

- [ ] **Step 3: Create `deploy/chart/templates/webui.yaml`**

```yaml
{{- if .Values.webui.enabled }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: devpod-webui-quota
  namespace: {{ .Values.namespaces.system }}
data:
  default-quota.yaml: |
{{ toYaml .Values.webui.defaultQuota | indent 4 }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: devpod-webui
  namespace: {{ .Values.namespaces.system }}
  labels: {app: devpod-webui}
spec:
  replicas: {{ .Values.webui.replicas }}
  selector:
    matchLabels: {app: devpod-webui}
  template:
    metadata:
      labels: {app: devpod-webui}
      annotations:
        checksum/quota: {{ toYaml .Values.webui.defaultQuota | sha256sum }}
    spec:
      serviceAccountName: devpod-webui
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: webui
          image: "{{ .Values.image.webui.repository }}:{{ .Values.image.webui.tag }}"
          imagePullPolicy: {{ .Values.image.webui.pullPolicy }}
          args:
            - --listen=:8080
            - --gitlab-issuer-url={{ required "webui.gitlab.issuerURL is required" .Values.webui.gitlab.issuerURL }}
            - --oauth-client-id={{ required "webui.gitlab.clientID is required" .Values.webui.gitlab.clientID }}
            - --oauth-client-secret-file=/etc/devpod/webui/oauth/client-secret
            - --redirect-url={{ required "webui.baseURL is required" .Values.webui.baseURL }}/auth/callback
            - --user-prefix={{ .Values.webui.userPrefix }}
            - --admins={{ join "," .Values.webui.admins }}
            - --session-key-file=/etc/devpod/webui/session/session-key
            - --default-quota-file=/etc/devpod/webui/default-quota.yaml
            - --devpod-namespace={{ .Values.namespaces.devpods }}
            - --kore={{ .Values.webui.kore }}
          ports: [{containerPort: 8080, name: http}]
          readinessProbe:
            httpGet: {path: /healthz, port: http}
          resources: {{- toYaml .Values.webui.resources | nindent 12 }}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
            readOnlyRootFilesystem: true
          volumeMounts:
            - {name: oauth, mountPath: /etc/devpod/webui/oauth, readOnly: true}
            - {name: session, mountPath: /etc/devpod/webui/session, readOnly: true}
            - {name: quota, mountPath: /etc/devpod/webui/default-quota.yaml, subPath: default-quota.yaml, readOnly: true}
      volumes:
        - name: oauth
          secret: {secretName: {{ required "webui.gitlab.clientSecretSecret.name is required" .Values.webui.gitlab.clientSecretSecret.name }}}
        - name: session
          secret: {secretName: {{ required "webui.sessionKeySecret.name is required" .Values.webui.sessionKeySecret.name }}}
        - name: quota
          configMap: {name: devpod-webui-quota}
---
apiVersion: v1
kind: Service
metadata:
  name: devpod-webui
  namespace: {{ .Values.namespaces.system }}
spec:
  type: {{ .Values.webui.service.type }}
  selector: {app: devpod-webui}
  ports:
    - {name: http, port: {{ .Values.webui.service.port }}, targetPort: http}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: devpod-webui
  namespace: {{ .Values.namespaces.system }}
spec:
  podSelector:
    matchLabels: {app: devpod-webui}
  policyTypes: [Ingress]
  ingress:
    - ports: [{port: 8080}]
{{- end }}
```

- [ ] **Step 4: Create `deploy/chart/templates/webui-rbac.yaml`**

```yaml
{{- if .Values.webui.enabled }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: devpod-webui
  namespace: {{ .Values.namespaces.system }}
---
# Namespaced rights in the devpods namespace only.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: devpod-webui
  namespace: {{ .Values.namespaces.devpods }}
rules:
  - apiGroups: ["devpod.io"]
    resources: ["devpods", "devpodsnapshots"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["pods", "events"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: devpod-webui
  namespace: {{ .Values.namespaces.devpods }}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: devpod-webui}
subjects:
  - {kind: ServiceAccount, name: devpod-webui, namespace: {{ .Values.namespaces.system }}}
---
# Users and DevPodTemplates are cluster-scoped: unavoidable ClusterRole.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: devpod-webui
rules:
  - apiGroups: ["devpod.io"]
    resources: ["users", "devpodtemplates"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
{{- if ne .Values.webui.kore "off" }}
  - apiGroups: ["kore.zjusct.io"]
    resources: ["korenodetopologies"]
    verbs: ["get", "list", "watch"]
{{- end }}
  # discovery for --kore=auto CRD probing is served by the apiserver
  # without extra RBAC.
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: devpod-webui
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: devpod-webui}
subjects:
  - {kind: ServiceAccount, name: devpod-webui, namespace: {{ .Values.namespaces.system }}}
{{- end }}
```

- [ ] **Step 5: Lint chart + docs**

Run: `helm template deploy/chart --set webui.enabled=true --set webui.baseURL=https://ui.example.com --set webui.gitlab.issuerURL=https://gitlab.example.com --set webui.gitlab.clientID=x --set webui.gitlab.clientSecretSecret.name=s1 --set webui.sessionKeySecret.name=s2 >/dev/null`
Expected: renders without error (also run once WITHOUT webui values — must render because everything is behind `webui.enabled`).

Append to `README.md` Commands section:

```markdown
    # Build the web UI bundle (requires Node >= 20)
    bash hack/build-webui.sh

    # Web UI end-to-end (kind required)
    bash hack/e2e-webui.sh
```

Add a `## Web UI` section to `QUICKSTART.md` (after the LDAP section) covering: create the two Secrets (`client-secret`, `session-key` via `openssl rand -out - 32`), GitLab OAuth application setup (redirect URL `<baseURL>/auth/callback`, scopes `openid profile`), helm upgrade with `webui.enabled=true`, and a note that templates are seeded via `kubectl apply` in M1 with one example DevPodTemplate manifest (copy the `pin8` shape from Task 9's test).

- [ ] **Step 6: Commit**

```bash
git add images/webui/Dockerfile deploy/chart/templates/webui.yaml deploy/chart/templates/webui-rbac.yaml deploy/chart/values.yaml README.md QUICKSTART.md
git commit -m "webui: image, chart deployment + RBAC, docs"
```

---

### Task 17: Fake IdP + e2e

**Files:**
- Create: `test/fakeidp/main.go`
- Create: `hack/e2e-webui.sh`

**Interfaces:**
- Consumes: the deployed chart (Task 16), `hack/e2e-up.sh` conventions.
- Produces: an in-cluster minimal OIDC issuer (`fakeidp`) whose `/auth` auto-approves (302 straight back with a code) so the whole login flow works with curl; the e2e script drives login → create → wait Running → hibernate → delete through the public API.

- [ ] **Step 1: Create `test/fakeidp/main.go`**

Reuse the shapes from Task 8's test issuer, as a standalone binary:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command fakeidp is a minimal OIDC issuer for e2e: discovery, JWKS,
// auto-approving /auth, and a /token endpoint returning an RS256
// id_token with a fixed preferred_username. NEVER use outside tests.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"time"
)

func main() {
	var (
		listen   = flag.String("listen", ":9998", "listen address")
		issuer   = flag.String("issuer", "http://fakeidp.devpod-system.svc:9998", "issuer URL as seen by the webui")
		username = flag.String("username", "alice", "preferred_username to assert")
		clientID = flag.String("client-id", "webui-client", "audience")
	)
	flag.Parse()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}

	sign := func() string {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"e2e"}`))
		claims, _ := json.Marshal(map[string]any{
			"iss": *issuer, "aud": *clientID, "sub": "1",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"preferred_username": *username,
		})
		payload := base64.RawURLEncoding.EncodeToString(claims)
		h := sha256.Sum256([]byte(header + "." + payload))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		if err != nil {
			log.Fatal(err)
		}
		return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": *issuer, "authorization_endpoint": *issuer + "/auth",
			"token_endpoint": *issuer + "/token", "jwks_uri": *issuer + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		pub := &key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "e2e",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		v := redirect.Query()
		v.Set("code", "e2e-code")
		v.Set("state", q.Get("state"))
		redirect.RawQuery = v.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": sign(),
		})
	})

	fmt.Println("fakeidp listening on", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux)) //nolint:gosec // test-only binary
}
```

- [ ] **Step 2: Create `hack/e2e-webui.sh`**

```bash
#!/usr/bin/env bash
# Web UI e2e on kind: fake IdP + webui, then curl-driven
# login → create → hibernate → wake-refused-by-quota? → delete.
# Assumes hack/e2e-up.sh has already stood up the cluster + base chart.
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"
NS=devpod-system
CLUSTER="${KIND_CLUSTER:-devpod-e2e}"

# --- images ---------------------------------------------------------------
docker build -t devpod-webui:e2e -f images/webui/Dockerfile .
docker build -t devpod-fakeidp:e2e -f - . <<'EOF'
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /fakeidp ./test/fakeidp
FROM gcr.io/distroless/static
COPY --from=build /fakeidp /usr/local/bin/fakeidp
ENTRYPOINT ["/usr/local/bin/fakeidp"]
EOF
kind load docker-image devpod-webui:e2e devpod-fakeidp:e2e --name "$CLUSTER"

# --- fake IdP -------------------------------------------------------------
kubectl -n "$NS" apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: fakeidp, labels: {app: fakeidp}}
spec:
  replicas: 1
  selector: {matchLabels: {app: fakeidp}}
  template:
    metadata: {labels: {app: fakeidp}}
    spec:
      containers:
        - name: fakeidp
          image: devpod-fakeidp:e2e
          args: ["--issuer=http://fakeidp.devpod-system.svc:9998"]
          ports: [{containerPort: 9998}]
---
apiVersion: v1
kind: Service
metadata: {name: fakeidp}
spec:
  selector: {app: fakeidp}
  ports: [{port: 9998}]
EOF
kubectl -n "$NS" rollout status deploy/fakeidp --timeout=120s

# --- secrets + chart ------------------------------------------------------
kubectl -n "$NS" delete secret webui-oauth webui-session --ignore-not-found
kubectl -n "$NS" create secret generic webui-oauth --from-literal=client-secret=e2e-secret
kubectl -n "$NS" create secret generic webui-session --from-literal=session-key="$(head -c 48 /dev/urandom | base64)"

helm upgrade devpod ./deploy/chart --reuse-values \
  --set image.webui.repository=devpod-webui --set image.webui.tag=e2e \
  --set webui.enabled=true \
  --set webui.replicas=1 \
  --set webui.baseURL=http://127.0.0.1:18080 \
  --set webui.userPrefix=gl- \
  --set webui.gitlab.issuerURL=http://fakeidp.devpod-system.svc:9998 \
  --set webui.gitlab.clientID=webui-client \
  --set webui.gitlab.clientSecretSecret.name=webui-oauth \
  --set webui.sessionKeySecret.name=webui-session \
  --set-json 'webui.defaultQuota={"maxDevPods":2,"compute":{"cpu":"4","memory":"8Gi"},"storage":"10Gi"}'
kubectl -n "$NS" rollout status deploy/devpod-webui --timeout=180s

kubectl -n "$NS" port-forward svc/devpod-webui 18080:80 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 2

# --- login (cookie jar carries state/PKCE/session) -------------------------
JAR="$(mktemp)"
# /auth/login 302→ fakeidp /auth 302→ /auth/callback 302→ /
# curl -L follows all three; fakeidp is reachable from the host only via
# the webui's redirect, so rewrite its host to the port-forward? No:
# fakeidp's /auth URL is cluster-internal. Follow manually instead.
LOGIN_LOC=$(curl -s -o /dev/null -w '%{redirect_url}' -c "$JAR" http://127.0.0.1:18080/auth/login)
# Extract state from the IdP URL and call the callback directly — the
# fake IdP always issues code "e2e-code".
STATE=$(python3 -c "import sys,urllib.parse as u;print(u.parse_qs(u.urlparse(sys.argv[1]).query)['state'][0])" "$LOGIN_LOC")
curl -s -o /dev/null -b "$JAR" -c "$JAR" \
  "http://127.0.0.1:18080/auth/callback?code=e2e-code&state=$STATE"
grep -q devpod_session "$JAR" || { echo "FAIL: no session cookie"; exit 1; }

api() { curl -s -b "$JAR" -H 'Content-Type: application/json' "$@"; }

# --- exercise -------------------------------------------------------------
api -X POST http://127.0.0.1:18080/api/devpods \
  -d '{"name":"e2e","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}' | tee /tmp/create.json
grep -q '"gl-alice-e2e"' /tmp/create.json || { echo "FAIL: create"; exit 1; }

kubectl -n devpods wait --for=jsonpath='{.status.phase}'=Running devpod/gl-alice-e2e --timeout=180s

api -X PATCH http://127.0.0.1:18080/api/devpods/gl-alice-e2e -d '{"running":false}' >/dev/null
kubectl -n devpods wait --for=jsonpath='{.status.phase}'=Stopped devpod/gl-alice-e2e --timeout=120s

# quota: cpu limit is 4 → a 6-cpu pod must be refused with 409
CODE=$(api -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:18080/api/devpods \
  -d '{"name":"big","image":"ubuntu:24.04","cpu":"6","memory":"1Gi"}')
[ "$CODE" = "409" ] || { echo "FAIL: quota expected 409, got $CODE"; exit 1; }

# kore stamping invariant: raw annotation must be rejected
CODE=$(api -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:18080/api/devpods \
  -d '{"yaml":"apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: gl-alice-pin\nspec:\n  owner: gl-alice\n  running: true\n  pod:\n    metadata:\n      annotations:\n        kore.zjusct.io/pin: \"true\"\n    spec:\n      containers:\n      - name: dev\n        image: ubuntu:24.04\n        resources:\n          limits: {cpu: \"1\", memory: \"1Gi\"}\n"}')
[ "$CODE" = "403" ] || { echo "FAIL: kore rejection expected 403, got $CODE"; exit 1; }

api -X DELETE -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/api/devpods/gl-alice-e2e | grep -q 204 \
  || { echo "FAIL: delete"; exit 1; }

echo "e2e-webui: PASS"
```

- [ ] **Step 3: Run it**

Run: `bash hack/e2e-up.sh && bash hack/e2e-webui.sh`
Expected: final line `e2e-webui: PASS`. Debug loop: `kubectl -n devpod-system logs deploy/devpod-webui`.

- [ ] **Step 4: Commit**

```bash
git add test/fakeidp/main.go hack/e2e-webui.sh
git commit -m "webui: fake OIDC issuer + end-to-end script"
```

---

## Post-plan checklist (M1 exit)

- `bash hack/test.sh` green, `bash hack/e2e-webui.sh` green.
- `helm template` renders with and without `webui.enabled`.
- Spec §11 M1 acceptance: fresh GitLab user → login → pubkey upload → create (plain AND via a seeded pin template) → `ssh gl-xxx+pod@gateway` — verify manually on the e2e cluster.
- Update `FOLLOWUPS.md` with anything deferred during implementation.




