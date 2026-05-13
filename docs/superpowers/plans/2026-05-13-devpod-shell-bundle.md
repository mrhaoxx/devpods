# Shell-bundle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the supervisor shell bundle (`bash`, `zsh`, `fish`, `busybox`, uutils coreutils, terminfo) inside the `devpod-supervisor` scratch image and let users pick the interactive shell via a new `DevPod.spec.shell` enum field, with auto-fallback to bundled bash when the user container's `/etc/passwd` shell is missing, non-executable, or on the no-login blacklist.

**Architecture:** One Dockerfile (`images/supervisor/Dockerfile`) grows from 3 stages to 8. The `cmd/supervisor` binary gains a pure helper that computes the `-o ForceCommand=...` and `-o SetEnv=...` flags to append to its existing `sshd` exec; the helper is unit-tested with fake env/passwd inputs and called from `main`. The controller (`internal/render/pod.go`) wires `dp.Spec.Shell` through as a `DEVPOD_SHELL` env var on the target container and bumps the supervisor emptyDir's `sizeLimit` to 100Mi. CRD regen and chart sync go through the existing `go generate ./api/v1alpha1/...` flow.

**Tech Stack:** Go 1.25, controller-runtime v0.20, kubebuilder validation markers, alpine 3.20 build images (musl + ncurses-static + busybox-static), Rust 1.82 for fish and uutils, docker buildx with `linux/amd64,linux/arm64`.

**Spec reference:** `docs/superpowers/specs/2026-05-13-devpod-shell-bundle.md`.

---

## File map

**Create**
- `cmd/supervisor/shell.go` — pure `prepareShellArgs()` helper + tiny `/etc/passwd` parser
- `cmd/supervisor/shell_test.go` — unit tests for the helper
- `hack/e2e-v2-shells.sh` — e2e: distroless + each shell + fallback case

**Modify**
- `api/v1alpha1/devpod_types.go` — add `Shell` field on `DevPodSpec`
- `api/v1alpha1/zz_generated.deepcopy.go` — regen
- `config/crd/bases/devpod.io_devpods.yaml` — regen
- `deploy/chart/templates/crds/devpod.io_devpods.yaml` — sync (regen does this)
- `internal/render/pod.go` — inject `DEVPOD_SHELL` env + emptyDir sizeLimit
- `internal/render/pod_test.go` — cover env + sizeLimit
- `cmd/supervisor/main.go` — call `prepareShellArgs()` and append to sshd args
- `cmd/supervisor/main_test.go` — assertion that overrides are forwarded
- `images/supervisor/Dockerfile` — add 5 build stages + assembly COPYs
- `hack/e2e-up.sh` — no change needed (Dockerfile path unchanged); the GHA cache key bump if any is out of scope

---

## Task 1: CRD `Shell` field + regen

**Files:**
- Modify: `api/v1alpha1/devpod_types.go` (within `DevPodSpec`, around line 146 — after `ExitOnUserCommandExit`)
- Modify (regen): `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/devpod.io_devpods.yaml`, `deploy/chart/templates/crds/devpod.io_devpods.yaml`

- [ ] **Step 1: Add the `Shell` field**

Edit `api/v1alpha1/devpod_types.go`, insert immediately after the `ExitOnUserCommandExit bool` block (currently ends with `ExitOnUserCommandExit bool ...`, around line 145):

```go
	// Shell selects the interactive login shell sshd exec's inside the
	// user container. When empty, the supervisor falls back to the user
	// image's /etc/passwd shell if it exists and is executable; if not
	// (typical for distroless images), it falls back to /opt/devpod/bin/bash.
	// The named shells are provided by the supervisor bundle at
	// /opt/devpod/bin/.
	//
	// +optional
	// +kubebuilder:validation:Enum=bash;zsh;fish
	Shell string `json:"shell,omitempty"`
```

- [ ] **Step 2: Regenerate deepcopy + CRDs + chart sync**

Run:
```bash
go generate ./api/v1alpha1/...
```
Expected: three files change — `api/v1alpha1/zz_generated.deepcopy.go` (unchanged for plain-string field), `config/crd/bases/devpod.io_devpods.yaml` (adds `shell` property with enum), `deploy/chart/templates/crds/devpod.io_devpods.yaml` (mirrors the same).

- [ ] **Step 3: Verify enum landed in CRD**

Run:
```bash
grep -A2 'shell:' config/crd/bases/devpod.io_devpods.yaml | head -20
```
Expected output contains:
```
                shell:
                  description: ...
                  enum:
                  - bash
                  - zsh
                  - fish
                  type: string
```

- [ ] **Step 4: Compile-check**

Run:
```bash
go build ./...
```
Expected: no output, exit 0.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/devpod_types.go \
        api/v1alpha1/zz_generated.deepcopy.go \
        config/crd/bases/devpod.io_devpods.yaml \
        deploy/chart/templates/crds/devpod.io_devpods.yaml
git commit -m "api/v1alpha1: DevPodSpec.Shell — bash|zsh|fish enum for in-container login shell"
```

---

## Task 2: Render — wire `DEVPOD_SHELL` env + bump emptyDir sizeLimit (TDD)

**Files:**
- Modify: `internal/render/pod.go:83-124` (`wrapTargetWithSupervisor`), `internal/render/pod.go:141-146` (`supervisorBinVolume`)
- Modify: `internal/render/pod_test.go`

- [ ] **Step 1: Write failing test for `DEVPOD_SHELL` env injection**

Append to `internal/render/pod_test.go`:

```go
func TestRenderPod_InjectsDevpodShellEnvWhenSet(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Shell = "fish"
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	target := pod.Spec.Containers[0]
	var seen string
	for _, e := range target.Env {
		if e.Name == "DEVPOD_SHELL" {
			seen = e.Value
		}
	}
	if seen != "fish" {
		t.Errorf("DEVPOD_SHELL env = %q, want %q", seen, "fish")
	}
}

func TestRenderPod_OmitsDevpodShellEnvWhenUnset(t *testing.T) {
	dp := minimalDevPod() // Shell defaults to ""
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	target := pod.Spec.Containers[0]
	for _, e := range target.Env {
		if e.Name == "DEVPOD_SHELL" {
			t.Errorf("DEVPOD_SHELL env unexpectedly set to %q", e.Value)
		}
	}
}
```

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/render/ -run TestRenderPod_InjectsDevpodShellEnvWhenSet -v
```
Expected: FAIL — `DEVPOD_SHELL env = "", want "fish"`.

- [ ] **Step 3: Implement env injection in `wrapTargetWithSupervisor`**

Edit `internal/render/pod.go`. Within `wrapTargetWithSupervisor`, in the `for i := range spec.Containers` block immediately after the `if dp.Spec.ExitOnUserCommandExit { ... }` block (around line 120, before `return nil`), insert:

```go
		if dp.Spec.Shell != "" {
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "DEVPOD_SHELL", Value: dp.Spec.Shell},
			)
		}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/render/ -run 'TestRenderPod_(InjectsDevpodShellEnvWhenSet|OmitsDevpodShellEnvWhenUnset)' -v
```
Expected: PASS for both.

- [ ] **Step 5: Write failing test for emptyDir sizeLimit**

Append to `internal/render/pod_test.go`:

```go
func TestRenderPod_SupervisorBinVolumeHasSizeLimit(t *testing.T) {
	pod, err := render.Pod(minimalDevPod(), cfg())
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name != render.VolumeSupervisorBin {
			continue
		}
		if v.EmptyDir == nil {
			t.Fatalf("volume %q has no EmptyDir source", v.Name)
		}
		if v.EmptyDir.SizeLimit == nil {
			t.Errorf("emptyDir.sizeLimit unset; want 100Mi")
			return
		}
		want := resource.MustParse("100Mi")
		if v.EmptyDir.SizeLimit.Cmp(want) != 0 {
			t.Errorf("emptyDir.sizeLimit = %s, want %s",
				v.EmptyDir.SizeLimit.String(), want.String())
		}
		return
	}
	t.Fatalf("volume %q not found", render.VolumeSupervisorBin)
}
```

- [ ] **Step 6: Run, verify failure**

```bash
go test ./internal/render/ -run TestRenderPod_SupervisorBinVolumeHasSizeLimit -v
```
Expected: FAIL — `emptyDir.sizeLimit unset; want 100Mi`.

- [ ] **Step 7: Set sizeLimit in `supervisorBinVolume`**

Edit `internal/render/pod.go`. Replace `supervisorBinVolume` (lines 141–146):

```go
func supervisorBinVolume() corev1.Volume {
	limit := resource.MustParse("100Mi")
	return corev1.Volume{
		Name: VolumeSupervisorBin,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit},
		},
	}
}
```

Add the import for `k8s.io/apimachinery/pkg/api/resource` at the top of the file if not already present (it isn't):

```go
import (
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)
```

- [ ] **Step 8: Run, verify pass + full render package green**

```bash
go test ./internal/render/ -v
```
Expected: all tests PASS (including pre-existing ones — sizeLimit is additive and DEVPOD_SHELL only fires when explicitly set).

- [ ] **Step 9: Commit**

```bash
git add internal/render/pod.go internal/render/pod_test.go
git commit -m "internal/render: DEVPOD_SHELL env + supervisor emptyDir sizeLimit=100Mi"
```

---

## Task 3: Supervisor `prepareShellArgs` helper (TDD, pure logic)

**Files:**
- Create: `cmd/supervisor/shell.go`
- Create: `cmd/supervisor/shell_test.go`

The helper is split from `main.go` for testability: it is pure (no I/O dependencies except injected readers) and returns the slice of extra `-o` flags to append to the `sshd` invocation.

- [ ] **Step 1: Sketch the contract via tests first**

Create `cmd/supervisor/shell_test.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeStat returns a stub `os.Stat` for the supervisor's shell probe.
// paths is a map of path -> mode (use 0 to mean "missing").
func fakeStat(paths map[string]os.FileMode) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		m, ok := paths[p]
		if !ok || m == 0 {
			return nil, os.ErrNotExist
		}
		return fakeFileInfo{mode: m}, nil
	}
}

type fakeFileInfo struct{ mode os.FileMode }

func (f fakeFileInfo) Name() string       { return "" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func mustContain(t *testing.T, args []string, want string) {
	t.Helper()
	for _, a := range args {
		if a == want {
			return
		}
	}
	t.Errorf("args missing %q\ngot: %v", want, args)
}

func mustNotContainSubstr(t *testing.T, args []string, substr string) {
	t.Helper()
	for _, a := range args {
		if strings.Contains(a, substr) {
			t.Errorf("args unexpectedly contain %q via %q\nargs: %v", substr, a, args)
			return
		}
	}
}

func TestPrepareShellArgs_ExplicitShell_AddsForceCommand(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "fish" }, // DEVPOD_SHELL=fish
		nil,                                   // passwd reader not consulted
		fakeStat(map[string]os.FileMode{}),
	)
	mustContain(t, args, "-o")
	mustContain(t, args, "ForceCommand=/opt/devpod/bin/fish -l")
	mustContain(t, args, "SetEnv=DEVPOD_ACTIVE_SHELL=fish")
	mustContain(t, args, "SetEnv=PATH=/opt/devpod/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	mustContain(t, args, "SetEnv=TERMINFO=/opt/devpod/share/terminfo")
}

func TestPrepareShellArgs_InvalidShell_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "tcsh" }, // not in our bundle
		nil,
		fakeStat(map[string]os.FileMode{}),
	)
	// Defense-in-depth: CRD enum should reject tcsh, but if it slips
	// through the supervisor still picks a sane default.
	mustContain(t, args, "ForceCommand=/opt/devpod/bin/bash -l")
	mustContain(t, args, "SetEnv=DEVPOD_ACTIVE_SHELL=bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdMissing_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) { return nil, os.ErrNotExist },
		fakeStat(map[string]os.FileMode{}),
	)
	mustContain(t, args, "ForceCommand=/opt/devpod/bin/bash -l")
	mustContain(t, args, "SetEnv=DEVPOD_ACTIVE_SHELL=bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdRootShellExecutable_NoForceCommand(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534::/:/bin/false\n"), nil
		},
		fakeStat(map[string]os.FileMode{"/bin/bash": 0o755}),
	)
	mustNotContainSubstr(t, args, "ForceCommand=")
	// The shell we "would have used" is reported for diagnostics.
	mustContain(t, args, "SetEnv=DEVPOD_ACTIVE_SHELL=/bin/bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdRootShellMissing_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/bin/zsh\n"), nil
		},
		fakeStat(map[string]os.FileMode{}), // /bin/zsh not present
	)
	mustContain(t, args, "ForceCommand=/opt/devpod/bin/bash -l")
	mustContain(t, args, "SetEnv=DEVPOD_ACTIVE_SHELL=bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdRootShellNotExecutable_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/bin/bash\n"), nil
		},
		fakeStat(map[string]os.FileMode{"/bin/bash": 0o644}), // no x bit
	)
	mustContain(t, args, "ForceCommand=/opt/devpod/bin/bash -l")
}

func TestPrepareShellArgs_UnsetEnv_PasswdRootShellNologinBlacklist_FallsBackToBash(t *testing.T) {
	for _, shell := range []string{"/sbin/nologin", "/usr/sbin/nologin", "/bin/false", "/usr/bin/false"} {
		args := prepareShellArgs(
			func(string) string { return "" },
			func() ([]byte, error) {
				return []byte("root:x:0:0:root:/root:" + shell + "\n"), nil
			},
			fakeStat(map[string]os.FileMode{shell: 0o755}),
		)
		mustContain(t, args, "ForceCommand=/opt/devpod/bin/bash -l")
		if t.Failed() {
			t.Logf("blacklist case failed for shell=%q", shell)
			return
		}
	}
}

func TestPrepareShellArgs_UnsetEnv_PasswdNoRootLine_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) {
			return []byte("nobody:x:65534:65534::/:/bin/false\n"), nil
		},
		fakeStat(map[string]os.FileMode{"/bin/false": 0o755}),
	)
	mustContain(t, args, "ForceCommand=/opt/devpod/bin/bash -l")
}
```

- [ ] **Step 2: Run, verify all tests fail with "undefined: prepareShellArgs"**

```bash
go test ./cmd/supervisor/ -run 'TestPrepareShellArgs' -v
```
Expected: FAIL — `undefined: prepareShellArgs` (compilation error).

- [ ] **Step 3: Implement `prepareShellArgs`**

Create `cmd/supervisor/shell.go`:

```go
// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"os"
	"strings"
)

// allowedShells are the shells provided by the supervisor bundle.
// CRD enum on DevPod.spec.shell normally enforces this; the supervisor
// re-validates for defense in depth.
var allowedShells = map[string]string{
	"bash": "/opt/devpod/bin/bash",
	"zsh":  "/opt/devpod/bin/zsh",
	"fish": "/opt/devpod/bin/fish",
}

// nologinShells are common "no interactive login" shells that should be
// treated as "no usable shell" by the auto-fallback path.
var nologinShells = map[string]struct{}{
	"/sbin/nologin":     {},
	"/usr/sbin/nologin": {},
	"/bin/false":        {},
	"/usr/bin/false":    {},
}

// prepareShellArgs returns the extra `-o` flags to append to the sshd
// invocation. It honors DEVPOD_SHELL when set; otherwise it probes the
// user container's /etc/passwd root entry and falls back to bundled
// bash when that shell is missing, non-executable, or on the no-login
// blacklist.
//
// getenv, passwd, and stat are injected for testability. In production,
// callers pass os.Getenv, defaultPasswdReader, os.Stat.
func prepareShellArgs(
	getenv func(string) string,
	passwd func() ([]byte, error),
	stat func(string) (fs.FileInfo, error),
) []string {
	chosen, force := resolveShell(getenv, passwd, stat)
	out := []string{
		"-o", "SetEnv=PATH=/opt/devpod/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"-o", "SetEnv=TERMINFO=/opt/devpod/share/terminfo",
		"-o", "SetEnv=TERMINFO_DIRS=/opt/devpod/share/terminfo:/usr/share/terminfo:/etc/terminfo:/lib/terminfo",
		"-o", "SetEnv=DEVPOD_ACTIVE_SHELL=" + chosen,
	}
	if force != "" {
		out = append(out, "-o", "ForceCommand="+force+" -l")
	}
	return out
}

// resolveShell decides what to advertise as the active shell, and
// whether sshd needs a ForceCommand override.
//
// Returns (chosen, forcePath):
//   - chosen is the value to expose via DEVPOD_ACTIVE_SHELL — either a
//     short name (bash|zsh|fish) when we forced the bundle, or the
//     /etc/passwd shell path when we leave sshd's default flow alone.
//   - forcePath is non-empty iff we want sshd to bypass /etc/passwd
//     and exec that binary as the login program.
func resolveShell(
	getenv func(string) string,
	passwd func() ([]byte, error),
	stat func(string) (fs.FileInfo, error),
) (chosen string, forcePath string) {
	if want := getenv("DEVPOD_SHELL"); want != "" {
		if p, ok := allowedShells[want]; ok {
			return want, p
		}
		return "bash", allowedShells["bash"]
	}
	if passwd != nil {
		if data, err := passwd(); err == nil {
			if rootShell := rootShellFromPasswd(data); rootShell != "" {
				if _, blacklisted := nologinShells[rootShell]; !blacklisted {
					if fi, err := stat(rootShell); err == nil && fi.Mode()&0o111 != 0 {
						return rootShell, ""
					}
				}
			}
		}
	}
	return "bash", allowedShells["bash"]
}

// rootShellFromPasswd extracts the shell field of the root entry.
// Returns "" when not found or malformed.
func rootShellFromPasswd(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "root:") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			return ""
		}
		return fields[6]
	}
	return ""
}

// defaultPasswdReader is the production passwd source.
func defaultPasswdReader() ([]byte, error) {
	return os.ReadFile("/etc/passwd")
}
```

- [ ] **Step 4: Run tests, verify all pass**

```bash
go test ./cmd/supervisor/ -run 'TestPrepareShellArgs' -v
```
Expected: PASS for all 8 cases.

- [ ] **Step 5: Commit**

```bash
git add cmd/supervisor/shell.go cmd/supervisor/shell_test.go
git commit -m "cmd/supervisor: prepareShellArgs — DEVPOD_SHELL + /etc/passwd fallback"
```

---

## Task 4: Wire `prepareShellArgs` into supervisor `main` (TDD)

**Files:**
- Modify: `cmd/supervisor/main.go:60-77` (sshd cmd construction)
- Modify: `cmd/supervisor/main_test.go`

- [ ] **Step 1: Write failing test that asserts overrides reach sshd**

Append to `cmd/supervisor/main_test.go`:

```go
// TestSupervisor_ForwardsShellOverridesToSshd uses a fake sshd
// (printenv-style) to assert the supervisor invokes sshd with the
// -o SetEnv / ForceCommand flags computed from DEVPOD_SHELL.
func TestSupervisor_ForwardsShellOverridesToSshd(t *testing.T) {
	sup := buildSupervisor(t)
	// Fake sshd: print all our args to stdout and exit 0.
	cmd := exec.Command(sup) // no user args
	cmd.Env = append(cmd.Environ(),
		"DEVPOD_SHELL=zsh",
		// Echo argv via /bin/sh; the supervisor will append -o flags
		// after the configured sshd args. We use sh -c "echo $@" as
		// the fake sshd binary so its stdout reveals what sshd would
		// have received.
		"SUPERVISOR_SSHD_PATH=/bin/sh",
		`SUPERVISOR_SSHD_ARGS=-c;printf '%s\n' "$@";--`,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("supervisor: %v: %s", err, out)
	}
	s := string(out)
	for _, want := range []string{
		"-o",
		"ForceCommand=/opt/devpod/bin/zsh -l",
		"SetEnv=DEVPOD_ACTIVE_SHELL=zsh",
		"SetEnv=PATH=/opt/devpod/bin:",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("stdout missing %q\nfull:\n%s", want, s)
		}
	}
}
```

- [ ] **Step 2: Run, verify failure**

```bash
go test ./cmd/supervisor/ -run TestSupervisor_ForwardsShellOverridesToSshd -v
```
Expected: FAIL — stdout missing the ForceCommand line.

- [ ] **Step 3: Wire the helper into `main`**

Edit `cmd/supervisor/main.go`. In `main`, replace the block (around line 67-69):

```go
	if port := os.Getenv("SUPERVISOR_SSHD_PORT"); port != "" {
		sshdArgs = append(sshdArgs, "-p", port)
	}
	sshdCmd := exec.Command(sshdPath, sshdArgs...)
```

with:

```go
	if port := os.Getenv("SUPERVISOR_SSHD_PORT"); port != "" {
		sshdArgs = append(sshdArgs, "-p", port)
	}
	sshdArgs = append(sshdArgs,
		prepareShellArgs(os.Getenv, defaultPasswdReader, os.Stat)...,
	)
	sshdCmd := exec.Command(sshdPath, sshdArgs...)
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./cmd/supervisor/ -run TestSupervisor_ForwardsShellOverridesToSshd -v
```
Expected: PASS.

- [ ] **Step 5: Run full supervisor test suite to verify no regressions**

```bash
go test ./cmd/supervisor/ -v
```
Expected: all existing tests still PASS (the new overrides append to sshd args but the existing tests use stub sshd commands that ignore extra args).

- [ ] **Step 6: Run full project tests**

```bash
bash hack/test.sh
```
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/supervisor/main.go cmd/supervisor/main_test.go
git commit -m "cmd/supervisor: append shell overrides (PATH/TERMINFO/ForceCommand) to sshd args"
```

---

## Task 5: Dockerfile — bash-static stage

**Files:**
- Modify: `images/supervisor/Dockerfile`

- [ ] **Step 1: Add the `bash-build` stage**

Edit `images/supervisor/Dockerfile`. Insert a new stage block immediately after the `supervisor-build` stage (after line 44, before the `# --- Stage 3: scratch image ...` comment):

```dockerfile
# --- Stage 3: bash-static ----------------------------------------------
FROM alpine:3.20 AS bash-build
RUN apk add --no-cache bash-static
RUN mkdir -p /out && cp /bin/bash.static /out/bash && strip /out/bash
```

- [ ] **Step 2: COPY bash into the scratch assembly**

Edit the scratch stage at the end of the file. Below the existing `COPY --from=supervisor-build ...` line, append:

```dockerfile
COPY --from=bash-build       /out/bash                   /opt/devpod/bin/bash
```

Also renumber the scratch-stage comment to reflect that stages have shifted (scratch is no longer Stage 3). Rename `# --- Stage 3: scratch image ...` to `# --- Final stage: scratch image for the initContainer -----------------`.

- [ ] **Step 3: Build the image locally to verify**

```bash
docker buildx build --platform=linux/amd64 \
  -t devpod-supervisor:plan-task5 \
  -f images/supervisor/Dockerfile --load .
```
Expected: build succeeds. Approximate time: 2–3 min cold (mostly the existing sshd-build stage), under 30 s if buildx cache is warm.

- [ ] **Step 4: Smoke check the bundled binary exists**

```bash
docker run --rm --entrypoint /opt/devpod/bin/bash devpod-supervisor:plan-task5 -c 'echo $BASH_VERSION'
```
Expected: bash version string on stdout (e.g., `5.2.26(1)-release`), exit 0.

- [ ] **Step 5: Commit**

```bash
git add images/supervisor/Dockerfile
git commit -m "images/supervisor: ship static bash at /opt/devpod/bin/bash"
```

---

## Task 6: Dockerfile — zsh-static stage

**Files:**
- Modify: `images/supervisor/Dockerfile`

- [ ] **Step 1: Add the `zsh-build` stage**

Edit `images/supervisor/Dockerfile`. Insert immediately after the `bash-build` stage:

```dockerfile
# --- Stage 4: zsh static (musl + ncurses-static) -----------------------
FROM alpine:3.20 AS zsh-build
ARG ZSH_VERSION=5.9
RUN apk add --no-cache build-base ncurses-static ncurses-dev autoconf wget xz
WORKDIR /src
RUN wget -qO zsh.tar.xz \
    "https://www.zsh.org/pub/zsh-${ZSH_VERSION}.tar.xz" && \
    tar xJf zsh.tar.xz
WORKDIR /src/zsh-${ZSH_VERSION}
RUN CFLAGS="-O2" LDFLAGS="-static" \
    ./configure \
        --disable-dynamic --enable-static \
        --disable-cap --disable-pcre \
        --without-tcsetpgrp && \
    make -j && \
    mkdir -p /out && cp Src/zsh /out/zsh && strip /out/zsh
```

- [ ] **Step 2: COPY zsh into the scratch assembly**

In the final scratch stage, append after the bash COPY:

```dockerfile
COPY --from=zsh-build        /out/zsh                    /opt/devpod/bin/zsh
```

- [ ] **Step 3: Build and verify**

```bash
docker buildx build --platform=linux/amd64 \
  -t devpod-supervisor:plan-task6 \
  -f images/supervisor/Dockerfile --load .
docker run --rm --entrypoint /opt/devpod/bin/zsh devpod-supervisor:plan-task6 -c 'echo $ZSH_VERSION'
```
Expected: build succeeds (zsh stage adds ~3–5 min cold); `5.9` on stdout.

- [ ] **Step 4: Commit**

```bash
git add images/supervisor/Dockerfile
git commit -m "images/supervisor: ship static zsh at /opt/devpod/bin/zsh"
```

---

## Task 7: Dockerfile — fish-static stage

**Files:**
- Modify: `images/supervisor/Dockerfile`

fish 4.x is the first release with a Rust core; static-link against musl
works through CMake's `-DCMAKE_EXE_LINKER_FLAGS` plus a musl Rust target.
If upstream regresses, this task may need to fall back to fish 3.7 source
(C++); document any deviation in the commit message.

- [ ] **Step 1: Add the `fish-build` stage**

Edit `images/supervisor/Dockerfile`. Insert after the `zsh-build` stage:

```dockerfile
# --- Stage 5: fish 4.x static (Rust + musl) ----------------------------
FROM rust:1.82-alpine AS fish-build
ARG FISH_VERSION=4.0.0
RUN apk add --no-cache build-base cmake ncurses-static pcre2-dev gettext-dev wget xz
WORKDIR /src
RUN wget -qO fish.tar.xz \
    "https://github.com/fish-shell/fish-shell/releases/download/${FISH_VERSION}/fish-${FISH_VERSION}.tar.xz" && \
    tar xJf fish.tar.xz
WORKDIR /src/fish-${FISH_VERSION}
RUN rustup target add $(uname -m)-unknown-linux-musl
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_DOCS=OFF \
          -DCMAKE_EXE_LINKER_FLAGS="-static" \
          -DRust_CARGO_TARGET=$(uname -m)-unknown-linux-musl && \
    cmake --build build -j --target fish
RUN mkdir -p /out && cp build/fish /out/fish && strip /out/fish
```

- [ ] **Step 2: COPY fish into the scratch assembly**

In the final scratch stage, append after the zsh COPY:

```dockerfile
COPY --from=fish-build       /out/fish                   /opt/devpod/bin/fish
```

- [ ] **Step 3: Build and verify**

```bash
docker buildx build --platform=linux/amd64 \
  -t devpod-supervisor:plan-task7 \
  -f images/supervisor/Dockerfile --load .
docker run --rm --entrypoint /opt/devpod/bin/fish devpod-supervisor:plan-task7 -c 'echo $version'
```
Expected: build succeeds (fish stage adds ~3–4 min cold); `4.0.0` on stdout.

If the build fails due to a fish 4.x upstream issue, retry with
`ARG FISH_VERSION=3.7.1` and adjust the configure invocation to omit
Rust target flags; commit the fallback and add a TODO comment in the
Dockerfile referencing the upstream issue.

- [ ] **Step 4: Commit**

```bash
git add images/supervisor/Dockerfile
git commit -m "images/supervisor: ship static fish at /opt/devpod/bin/fish"
```

---

## Task 8: Dockerfile — busybox + uutils coreutils stage

**Files:**
- Modify: `images/supervisor/Dockerfile`

- [ ] **Step 1: Add the `coreutils-build` stage**

Edit `images/supervisor/Dockerfile`. Insert after the `fish-build` stage:

```dockerfile
# --- Stage 6: busybox + uutils coreutils -------------------------------
FROM rust:1.82-alpine AS coreutils-build
ARG UUTILS_VERSION=0.0.30
RUN apk add --no-cache busybox-static build-base
RUN mkdir -p /out && cp /bin/busybox.static /out/busybox
RUN rustup target add $(uname -m)-unknown-linux-musl
RUN cargo install --root /uu --version ${UUTILS_VERSION} \
                  --target $(uname -m)-unknown-linux-musl \
                  --features unix \
                  coreutils
# uutils ships one multi-call binary. Enumerate applets via
# `coreutils --list`. (Upstream alternates between --list and other
# forms across versions; if pinned ${UUTILS_VERSION} doesn't have
# --list, swap for `coreutils help | awk '/^[a-z]/{print $1}'`.)
RUN mkdir -p /out/bin && cp /uu/bin/coreutils /out/bin/coreutils && \
    strip /out/bin/coreutils && \
    cd /out/bin && \
    for a in $(/uu/bin/coreutils --list); do ln -sf coreutils "$a"; done
```

- [ ] **Step 2: COPY busybox + uutils into scratch assembly**

In the final scratch stage, append after the fish COPY:

```dockerfile
COPY --from=coreutils-build  /out/busybox                /opt/devpod/bin/busybox
COPY --from=coreutils-build  /out/bin/                   /opt/devpod/bin/
```

- [ ] **Step 3: Build and verify**

```bash
docker buildx build --platform=linux/amd64 \
  -t devpod-supervisor:plan-task8 \
  -f images/supervisor/Dockerfile --load .
docker run --rm --entrypoint /opt/devpod/bin/busybox devpod-supervisor:plan-task8 --help 2>&1 | head -3
docker run --rm --entrypoint /opt/devpod/bin/ls devpod-supervisor:plan-task8 /opt/devpod/bin | head -20
```
Expected: busybox banner on stdout; `ls` output lists at least bash, zsh, fish, busybox, coreutils, and several uutils symlinks (cat, cp, mv, rm, etc.).

- [ ] **Step 4: If `coreutils --list` is unavailable in pinned version**

Replace the symlink loop:
```dockerfile
    for a in $(/uu/bin/coreutils --list); do ln -sf coreutils "$a"; done
```
with:
```dockerfile
    for a in $(/uu/bin/coreutils help 2>&1 | awk '/^[ ]*[a-z]+/{print $1}' | sort -u); do \
        [ -n "$a" ] && ln -sf coreutils "$a" || true; \
    done
```

- [ ] **Step 5: Commit**

```bash
git add images/supervisor/Dockerfile
git commit -m "images/supervisor: ship busybox-static + uutils coreutils as /opt/devpod/bin/*"
```

---

## Task 9: Dockerfile — terminfo stage

**Files:**
- Modify: `images/supervisor/Dockerfile`

- [ ] **Step 1: Add the `terminfo-build` stage**

Edit `images/supervisor/Dockerfile`. Insert after the `coreutils-build` stage:

```dockerfile
# --- Stage 7: terminfo database ----------------------------------------
FROM alpine:3.20 AS terminfo-build
RUN apk add --no-cache ncurses-terminfo
RUN mkdir -p /out && cp -r /usr/share/terminfo /out/terminfo
```

- [ ] **Step 2: COPY terminfo into scratch assembly**

In the final scratch stage, append after the busybox/uutils COPYs:

```dockerfile
COPY --from=terminfo-build   /out/terminfo               /opt/devpod/share/terminfo
```

- [ ] **Step 3: Build and verify**

```bash
docker buildx build --platform=linux/amd64 \
  -t devpod-supervisor:plan-task9 \
  -f images/supervisor/Dockerfile --load .
docker run --rm --entrypoint /opt/devpod/bin/busybox devpod-supervisor:plan-task9 ls /opt/devpod/share/terminfo/x | head -3
```
Expected: lists `xterm`, `xterm-256color`, and friends (each a small file under the `x/` shard).

- [ ] **Step 4: Verify final image size budget**

```bash
docker images devpod-supervisor:plan-task9 --format '{{.Size}}'
```
Expected: ≤ 40 MB. If significantly larger, capture the per-stage contribution via `docker image history devpod-supervisor:plan-task9` and note in the commit message.

- [ ] **Step 5: Commit**

```bash
git add images/supervisor/Dockerfile
git commit -m "images/supervisor: ship full terminfo database at /opt/devpod/share/terminfo"
```

---

## Task 10: e2e — distroless image + each shell + fallback

**Files:**
- Create: `hack/e2e-v2-shells.sh`

- [ ] **Step 1: Create the new e2e script**

Write `hack/e2e-v2-shells.sh`:

```bash
#!/usr/bin/env bash
# Prove the shell bundle: a distroless user image with no shell of its
# own still yields working ssh sessions, and spec.shell selects the
# bundle shell. Verifies bash / zsh / fish + fallback path.
#
# Prereqs: hack/e2e-up.sh already run (cluster up, supervisor image
# loaded, gateway service exposed).

set -euo pipefail

NS=devpods
OWNER=alice
GW_PORT=2222
KEY="$(cat /tmp/devpod-test-key-path)"

cleanup() {
    pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
    for n in s-bash s-zsh s-fish s-fallback; do
        kubectl -n "$NS" delete devpod "$n" --ignore-not-found --wait=false 2>/dev/null || true
    done
}
trap cleanup EXIT

start_port_forward() {
    pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
    sleep 1
    kubectl -n devpod-system port-forward svc/devpod-gateway "$GW_PORT:22" >/dev/null 2>&1 &
    local deadline=$((SECONDS + 30))
    until nc -z 127.0.0.1 "$GW_PORT" 2>/dev/null; do
        [[ $SECONDS -lt $deadline ]] || { echo "FAIL: port-forward never came up"; exit 1; }
        sleep 1
    done
}

ssh_run() {
    local name="$1"; shift
    ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -p "$GW_PORT" -i "$KEY" "$OWNER+$name@127.0.0.1" -- "$@"
}

apply_devpod() {
    local name="$1"; local shell="$2"
    local shell_field=""
    [[ -n "$shell" ]] && shell_field="  shell: $shell"
    cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: $name, namespace: $NS}
spec:
  owner: $OWNER
  running: true
${shell_field}
  pod:
    spec:
      containers:
      - name: dev
        image: gcr.io/distroless/static-debian12
        # No command — supervisor runs only sshd; the user container
        # has no /bin/sh of its own (that's the point).
EOF
    kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$name" --timeout=180s
}

verify_shell() {
    local name="$1"; local expected="$2"
    local out
    out=$(ssh_run "$name" 'env')
    if ! grep -q "^DEVPOD_ACTIVE_SHELL=$expected$" <<<"$out"; then
        echo "FAIL: $name expected DEVPOD_ACTIVE_SHELL=$expected, got:"
        grep DEVPOD_ACTIVE_SHELL <<<"$out" || echo "  (no DEVPOD_ACTIVE_SHELL in env)"
        return 1
    fi
    # Coreutils + PATH wiring: ls of bundle dir must succeed and show
    # several known binaries.
    out=$(ssh_run "$name" 'ls /opt/devpod/bin')
    for binary in bash zsh fish busybox; do
        if ! grep -q "^$binary$" <<<"$out"; then
            echo "FAIL: $name missing /opt/devpod/bin/$binary"
            return 1
        fi
    done
    # Terminfo: tput colors should report >= 8 (terminfo db is reachable
    # via TERMINFO/TERMINFO_DIRS). xterm is the default TERM with the
    # SSH client we use here.
    out=$(ssh_run "$name" 'TERM=xterm-256color /opt/devpod/bin/busybox tput colors')
    if [[ "$out" != "256" ]]; then
        echo "FAIL: $name tput colors = $out, want 256"
        return 1
    fi
    echo "OK: $name DEVPOD_ACTIVE_SHELL=$expected, /opt/devpod/bin populated, tput colors=256"
}

start_port_forward

echo "[1/4] DevPod with spec.shell=bash"
apply_devpod s-bash bash
verify_shell s-bash bash

echo "[2/4] DevPod with spec.shell=zsh"
apply_devpod s-zsh zsh
verify_shell s-zsh zsh

echo "[3/4] DevPod with spec.shell=fish"
apply_devpod s-fish fish
verify_shell s-fish fish

echo "[4/4] DevPod with spec.shell unset — auto-fallback to bash"
apply_devpod s-fallback ""
verify_shell s-fallback bash

echo
echo "OK: shell bundle demo passed — distroless image + bash/zsh/fish/fallback."
```

- [ ] **Step 2: Make the script executable and run it**

```bash
chmod +x hack/e2e-v2-shells.sh
bash hack/e2e-up.sh         # ensures fresh supervisor image with the bundle is loaded
bash hack/e2e-v2-shells.sh
```

Expected: all four `OK:` lines, exit 0.

- [ ] **Step 3: Make sure the existing v2 demo still passes (regression check)**

```bash
bash hack/e2e-v2.sh
```
Expected: existing test passes — the bundle is additive; debian image path is unchanged.

- [ ] **Step 4: Commit**

```bash
git add hack/e2e-v2-shells.sh
git commit -m "hack/e2e-v2-shells: distroless + bash/zsh/fish/fallback round-trip"
```

---

## Task 11: Final integration check + summary

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite**

```bash
bash hack/test.sh
```
Expected: all PASS.

- [ ] **Step 2: Re-run both e2e scripts back to back**

```bash
bash hack/e2e-v2.sh && bash hack/e2e-v2-shells.sh
```
Expected: both pass cleanly.

- [ ] **Step 3: Verify supervisor image size**

```bash
docker images devpod-supervisor:e2e --format '{{.Size}}'
```
Expected: ≤ 40 MB compressed. If over, capture per-stage size via
`docker image history` and note in a follow-up commit.

- [ ] **Step 4: Smoke check `kubectl explain` shows the new field**

```bash
kubectl explain devpod.spec.shell
```
Expected: shows the `Shell` description plus the enum hint.

- [ ] **Step 5: No commit; this task is a gate.**

---

## Self-review summary

- **Spec coverage:** spec §2.1 layout → Tasks 5–9; §2.2 size budget → Task 9 step 4; §2.3 CRD field → Task 1; §2.4 supervisor logic → Tasks 3–4; §2.5 controller wiring → Task 2; §3 Dockerfile → Tasks 5–9; §5.1 unit tests → Tasks 2 + 3; §5.3 e2e → Task 10.
- **Placeholder scan:** no TBDs or vague "handle edge cases" steps remain.
- **Type consistency:** `prepareShellArgs` signature (Task 3) matches its callsite in main (Task 4).
- **Spec deviations resolved inline:** Task 3 adds a no-login blacklist (nologin/false) that the spec §2.4 implies via "not executable" but doesn't enumerate — captured in `nologinShells`. The CRD field name `shell` and env var `DEVPOD_SHELL` / `DEVPOD_ACTIVE_SHELL` are consistent across api/render/supervisor/e2e.
