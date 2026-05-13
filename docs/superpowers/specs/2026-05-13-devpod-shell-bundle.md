# DevPod shell bundle — bash/zsh/fish + busybox + coreutils in supervisor image

**Status:** Approved
**Date:** 2026-05-13
**Depends on:** v2 in-container sshd ([2026-05-13-devpod-v2-incontainer-sshd.md](2026-05-13-devpod-v2-incontainer-sshd.md)) merged.

---

## 1. Goal

Make every DevPod usable as an interactive shell environment regardless of
what's in the user's container image — including distroless images that
ship neither a shell nor coreutils.

Concretely: the supervisor init container delivers a self-contained bundle
of `/opt/devpod/bin/{bash,zsh,fish,busybox,ls,cat,...}` plus a full
terminfo database, mounted read-only into the user container. The user
picks the login shell via a new `DevPod.spec.shell` field; if unset, the
supervisor falls back to the user container's `/etc/passwd` shell when
that file exists and is executable, otherwise to bash from the bundle.

Non-goals:

- Bundling editors (vim, nano), language toolchains, or git. The bundle
  is shell + textual coreutils only. Users add tools in their own image
  or via persistence.
- Bundling dotfiles. Shells use their stock defaults; users drop
  `.bashrc`/`.zshrc`/`config.fish` into their persistence-mounted `$HOME`
  if they want customization.
- Per-session shell selection (`ssh user+dp@gw -- fish`). The shell is a
  per-DevPod choice; switching ad-hoc is still possible by invoking the
  binary directly (`ssh ... -- /opt/devpod/bin/fish`), no field needed.

## 2. Architecture

### 2.1 Bundle contents (`/opt/devpod/` inside the supervisor image)

```
/opt/devpod/
  devpod-supervisor               PID 1 wrapper (Go)
  sbin/sshd                       static OpenSSH (existing)
  libexec/sftp-server             (existing)
  etc/sshd_config                 template (read-only)
  etc/profile.devpod              env defaults sourced by login shells
  bin/
    bash                          alpine bash-static
    zsh                           source-built, musl static, ncurses-static
    fish                          fish 4.x, Rust musl static
    busybox                       alpine busybox-static (fallback)
    {ls,cat,cp,mv,rm,grep,sed,    uutils/coreutils, musl static (preferred,
     awk,find,sort,head,tail,      first in PATH)
     wc,cut,tr,echo,env,...}
  share/terminfo/                 ncurses-terminfo full database (~5 MB)
```

Layout invariants:

- `/opt/devpod/bin/` is on `PATH` *first* in login shells (see §2.4).
- uutils binaries shadow busybox applets when both exist. busybox covers
  the long tail (`ps`, `top`, `xxd`, etc.) uutils doesn't ship.
- All paths under `/opt/devpod` are read-only from the user container's
  perspective (the emptyDir is writable on the node but the supervisor
  doesn't write to it after bootstrap; nothing inside the user container
  is expected to either).

### 2.2 Image size budget

Target: **≤ 40 MB** compressed for the supervisor image, up from the v2
target of ≤ 15 MB. Approximate per-component contribution (uncompressed):

| Component              | Size   |
|------------------------|--------|
| sshd + sftp-server     | ~3 MB  |
| devpod-supervisor      | ~3 MB  |
| bash-static            | ~1.5 MB|
| zsh static             | ~4 MB  |
| fish 4.x static        | ~8 MB  |
| busybox-static         | ~1 MB  |
| uutils coreutils       | ~6 MB  |
| terminfo database      | ~5 MB  |
| sshd_config + glue     | <0.1 MB|
| **Total uncompressed** | **~32 MB** |

Compressed image roughly 25–30 MB. emptyDir `sizeLimit` lifts from
50 Mi (v2) to **100 Mi** to leave headroom.

### 2.3 CRD addition

`api/v1alpha1/devpod_types.go`:

```go
type DevPodSpec struct {
    // ... existing fields ...

    // Shell picks the interactive shell sshd presents at login. If
    // empty, the controller falls back to the user container's
    // /etc/passwd shell when executable, otherwise to bash from the
    // DevPod bundle. The named shells are provided by the supervisor
    // bundle at /opt/devpod/bin/.
    //
    // +optional
    // +kubebuilder:validation:Enum=bash;zsh;fish
    Shell string `json:"shell,omitempty"`
}
```

No default. Empty string means "auto" (supervisor probes). The enum is
intentionally closed — exotic shells (xonsh, nushell, etc.) are not in
scope and would expand the image budget.

### 2.4 Supervisor behavior

`cmd/supervisor/main.go` gains a `prepareShell()` step that computes
runtime sshd overrides before exec'ing sshd. No temp file is written;
overrides are passed as `-o` flags so the baked `sshd_config` stays
read-only and untouched.

1. Read `DEVPOD_SHELL` env var (injected by the controller into the
   target container — see §2.5). One of `bash`, `zsh`, `fish`, or empty.
2. Decide whether to inject a `ForceCommand`:
   - If `DEVPOD_SHELL` set: force `/opt/devpod/bin/<shell> -l`.
   - Else: parse `/etc/passwd` for the **root** entry (UID 0 — the only
     identity sshd accepts in DevPod's auth model; see v2 §2.5
     `PermitRootLogin prohibit-password`). If the file is missing,
     malformed, or root's listed shell is not executable, force
     `/opt/devpod/bin/bash -l`. Otherwise omit ForceCommand and let
     sshd's default flow run root's `/etc/passwd` shell.
3. Exec sshd with the env / ForceCommand overrides as repeated `-o`
   flags:
   ```
   sshd -D -e \
     -f /opt/devpod/etc/sshd_config \
     -h /etc/devpod/ssh_host_ed25519_key \
     -o "SetEnv=PATH=/opt/devpod/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
     -o "SetEnv=TERMINFO=/opt/devpod/share/terminfo" \
     -o "SetEnv=TERMINFO_DIRS=/opt/devpod/share/terminfo:/usr/share/terminfo:/etc/terminfo:/lib/terminfo" \
     -o "SetEnv=DEVPOD_ACTIVE_SHELL=<chosen>" \
     [-o "ForceCommand=/opt/devpod/bin/<shell> -l"]
   ```
   `DEVPOD_ACTIVE_SHELL` is for diagnostics (and e2e assertions); it
   carries the actual shell name supervisor resolved, including the
   auto-fallback result.

The supervisor binary keeps its existing PID-1 duties unchanged (spawn
sshd + optional user-cmd, zombie reap, signal forward).

### 2.5 Controller / render changes

`internal/render/pod.go`:

- `injectSupervisor` extension: if `dp.Spec.Shell != ""`, append an
  `EnvVar{Name: "DEVPOD_SHELL", Value: dp.Spec.Shell}` to the target
  container's `Env`.
- Volume mount path unchanged (`/opt/devpod` read-only).
- emptyDir for `devpod-bin` gains `SizeLimit: resource.MustParse("100Mi")`.

`status.endpoint` and gateway dialing logic: unchanged.

`render.HostKeySecret`: unchanged — same Secret, same mount path
`/etc/devpod/`.

### 2.6 sshd_config baseline (`/opt/devpod/etc/sshd_config`)

Unchanged from v2 except the `Port` and `HostKey` lines. The SetEnv /
ForceCommand additions are written at runtime per §2.4 step 3 so the
baked config stays generic across DevPods.

### 2.7 No GatewayConfig changes

The bundle is part of the supervisor image. Operators that want to ship
a leaner image override `GatewayConfig.spec.supervisorImage` to a custom
build. No per-cluster "include shell bundle" toggle is added.

## 3. Image build pipeline

`images/supervisor/Dockerfile` grows from 3 stages to 7. Existing stages
(sshd-build, supervisor-build, scratch assembly) keep their structure;
new stages are appended.

```dockerfile
# syntax=docker/dockerfile:1.7

# --- Stage 1: static OpenSSH (existing) ---
FROM alpine:3.20 AS sshd-build
# ... unchanged ...

# --- Stage 2: supervisor Go binary (existing) ---
FROM golang:1.22-alpine AS supervisor-build
# ... unchanged ...

# --- Stage 3: bash-static ---
FROM alpine:3.20 AS bash-build
RUN apk add --no-cache bash-static
RUN mkdir -p /out && cp /bin/bash.static /out/bash && strip /out/bash

# --- Stage 4: zsh static (musl + ncurses-static) ---
FROM alpine:3.20 AS zsh-build
ARG ZSH_VERSION=5.9
RUN apk add --no-cache build-base ncurses-static ncurses-dev autoconf
WORKDIR /src
RUN wget -qO- https://www.zsh.org/pub/zsh-${ZSH_VERSION}.tar.xz | tar xJ
WORKDIR /src/zsh-${ZSH_VERSION}
RUN CFLAGS="-O2" LDFLAGS="-static" \
    ./configure --disable-dynamic --enable-static \
                --disable-cap --disable-pcre \
                --without-tcsetpgrp && \
    make -j && \
    mkdir -p /out && cp Src/zsh /out/zsh && strip /out/zsh

# --- Stage 5: fish 4.x static (Rust + musl) ---
FROM rust:1.82-alpine AS fish-build
ARG FISH_VERSION=4.0.0
RUN apk add --no-cache build-base cmake ncurses-static pcre2-dev gettext-dev
WORKDIR /src
RUN wget -qO- https://github.com/fish-shell/fish-shell/releases/download/${FISH_VERSION}/fish-${FISH_VERSION}.tar.xz | tar xJ
WORKDIR /src/fish-${FISH_VERSION}
RUN rustup target add $(uname -m)-unknown-linux-musl
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_DOCS=OFF \
          -DCMAKE_EXE_LINKER_FLAGS="-static" \
          -DRust_CARGO_TARGET=$(uname -m)-unknown-linux-musl && \
    cmake --build build -j --target fish && \
    mkdir -p /out && cp build/fish /out/fish && strip /out/fish

# --- Stage 6: busybox + uutils coreutils ---
FROM rust:1.82-alpine AS coreutils-build
RUN apk add --no-cache busybox-static build-base
RUN mkdir -p /out && cp /bin/busybox.static /out/busybox
RUN rustup target add $(uname -m)-unknown-linux-musl
ARG UUTILS_VERSION=0.0.30
RUN cargo install --root /uu --version ${UUTILS_VERSION} \
                  --target $(uname -m)-unknown-linux-musl \
                  --features unix \
                  coreutils
# uutils ships one multi-call binary. Enumerate applets via
# `coreutils --list` (exact subcommand name verified at build time;
# upstream has historically used --list / --help — confirm and pin in
# the implementation plan). For each applet, create a symlink that
# busybox-style dispatches into the multi-call binary.
RUN mkdir -p /out/bin && cp /uu/bin/coreutils /out/bin/coreutils && \
    strip /out/bin/coreutils && \
    cd /out/bin && \
    for a in $(/uu/bin/coreutils --list); do ln -s coreutils "$a"; done

# --- Stage 7: terminfo ---
FROM alpine:3.20 AS terminfo-build
RUN apk add --no-cache ncurses-terminfo
RUN mkdir -p /out && cp -r /usr/share/terminfo /out/terminfo

# --- Stage 8: scratch assembly ---
FROM scratch
COPY --from=sshd-build       /out/sbin/sshd                 /opt/devpod/sbin/sshd
COPY --from=sshd-build       /out/libexec/sftp-server       /opt/devpod/libexec/sftp-server
COPY --from=supervisor-build /out/devpod-supervisor         /opt/devpod/devpod-supervisor
COPY --from=bash-build       /out/bash                      /opt/devpod/bin/bash
COPY --from=zsh-build        /out/zsh                       /opt/devpod/bin/zsh
COPY --from=fish-build       /out/fish                      /opt/devpod/bin/fish
COPY --from=coreutils-build  /out/busybox                   /opt/devpod/bin/busybox
COPY --from=coreutils-build  /out/bin/                      /opt/devpod/bin/
COPY --from=terminfo-build   /out/terminfo                  /opt/devpod/share/terminfo
COPY images/supervisor/sshd_config                          /opt/devpod/etc/sshd_config
COPY images/supervisor/profile.devpod                       /opt/devpod/etc/profile.devpod
```

Build complexity:

- buildx with `linux/amd64,linux/arm64`. Both targets must build cleanly;
  any regression fails CI.
- Build time grows from ~2 min to ~8 min (zsh + fish dominate). Use
  GitHub Actions cache (`type=gha,scope=supervisor-<stage>`) per stage
  to keep incremental builds under a minute.
- Pin every version via ARG. Bumps are explicit commits.

## 4. Code changes (sketch)

```
api/v1alpha1/devpod_types.go              + Shell field, kubebuilder enum
api/v1alpha1/zz_generated.deepcopy.go     regen
config/crd/bases/devpod.io_devpods.yaml   regen
deploy/chart/templates/crds/...           regen via hack/sync-crd-chart

cmd/supervisor/main.go                    + prepareShell() before sshd exec
cmd/supervisor/main_test.go               + probe / fallback / config render tests

internal/render/pod.go                    DEVPOD_SHELL env, emptyDir sizeLimit
internal/render/pod_test.go               assert env + sizeLimit

internal/webhook/...                      n/a (CEL enum on Shell field is enough)

images/supervisor/Dockerfile              add 5 stages
images/supervisor/profile.devpod          NEW (PATH + TERMINFO defaults; sourceable)
images/supervisor/sshd_config             unchanged

hack/e2e-v2-shells.sh                     NEW
```

## 5. Test plan

### 5.1 Unit

- `cmd/supervisor/main_test.go`:
  - `DEVPOD_SHELL=zsh` → rendered sshd_config contains
    `ForceCommand /opt/devpod/bin/zsh -l`.
  - `DEVPOD_SHELL` unset, no `/etc/passwd` → renders bash ForceCommand.
  - `DEVPOD_SHELL` unset, `/etc/passwd` lists `/bin/sh`, file exists and
    is executable → no ForceCommand line emitted.
  - `DEVPOD_SHELL` unset, `/etc/passwd` lists `/bin/sh`, file missing →
    bash fallback ForceCommand.
  - Invalid `DEVPOD_SHELL=tcsh` → supervisor logs and falls back to
    bash (defense-in-depth; CRD enum should already reject this).

- `internal/render/pod_test.go`:
  - `dp.Spec.Shell = "fish"` → target container env contains
    `{Name:"DEVPOD_SHELL", Value:"fish"}`.
  - `dp.Spec.Shell = ""` → no `DEVPOD_SHELL` env entry.
  - emptyDir `devpod-bin` has `SizeLimit == 100Mi`.

### 5.2 envtest

`internal/controllers/devpod_controller_test.go`: add one case where
`spec.shell` round-trips through reconcile and lands on the rendered Pod.

### 5.3 e2e (`hack/e2e-v2-shells.sh` — new)

For each shell in `(bash, zsh, fish)`:

1. Apply a DevPod using `gcr.io/distroless/static-debian12` as the
   container image with `spec.shell: <shell>`.
2. Wait for Running.
3. `ssh alice+shells@gw -- env` → output contains
   `DEVPOD_ACTIVE_SHELL=<shell>` (supervisor's chosen shell is the one
   sshd actually exec's).
4. `ssh alice+shells@gw -- ls /opt/devpod/bin` → succeeds, output
   non-empty (proves coreutils + PATH wiring).
5. `ssh alice+shells@gw -- tput colors` → returns `256` (proves terminfo
   database is found via `TERMINFO_DIRS`).
6. Interactive smoke: `ssh -t alice+shells@gw 'echo hello && exit'` →
   exits cleanly with `hello` in stdout.

Plus one auto-fallback case:

7. DevPod with the same distroless image, `spec.shell` unset → ssh login
   succeeds, `env` shows `DEVPOD_ACTIVE_SHELL=bash` (fallback path).

`hack/e2e-v2.sh` and `hack/e2e-m2.sh` keep passing without modification
(they use full debian images where the in-container shell is unchanged).

## 6. Risks

- **fish 4.x static-cmake**: fish's CMake glue is the most fragile build
  step. Falling back to fish 3.7 is an option but loses the Rust core
  rewrite. Plan B: drop fish from the bundle if upstream static-link
  recipe regresses, surface a follow-up to add it back.
- **uutils completeness**: uutils ships most of GNU coreutils but a few
  exotic flags differ. busybox covers the gap for daily use. Spec § 6
  documents which applets uutils provides versus busybox-only.
- **Image registry pull on cold nodes**: 30 MB compressed pull is ~3 s
  on 100 Mbit. Documented; not a blocker.
- **emptyDir sizeLimit eviction**: 100 Mi is generous (bundle is ~32 MB
  uncompressed). If a node is under ephemeral-storage pressure kubelet
  evicts; same risk as today.
- **terminfo path**: TERMINFO and TERMINFO_DIRS are honored by
  ncurses-linked binaries (zsh, fish, busybox, uutils). If the *user's*
  container ships its own ncurses binary that ignores TERMINFO_DIRS,
  that binary may fail to render — out of scope; the user controls their
  image.
- **Build time on PR CI**: GHA cache makes incremental cheap; cold
  rebuilds (cache miss) are ~8 min. Acceptable for an image that ships
  on a release cadence rather than per-PR.

## 7. Open questions resolved during brainstorming

- Q: Per-session shell switch (`ssh ... -- fish`)? **A:** Already works
  by invoking the binary directly. No new mechanism.
- Q: Coreutils flavor? **A:** uutils (Rust, easy static) preferred,
  busybox covers gaps.
- Q: GatewayConfig toggle for bundle inclusion? **A:** No. Operators
  override `supervisorImage` for a leaner build.
- Q: Ship default rcfiles for prompt / autosuggest? **A:** No. Use shell
  stock defaults; users drop their own dotfiles into persistence.
- Q: emptyDir size? **A:** 100 Mi (bundle ~32 MB + headroom).
- Q: Exotic shells (xonsh, nushell)? **A:** Out of scope. CRD enum
  rejects them.
