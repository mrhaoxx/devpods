# syntax=docker/dockerfile:1.7

# --- Stage 1: static-pie OpenSSH from source (musl) -----------------------
FROM alpine:3.20 AS sshd-build
ARG OPENSSH_VERSION=10.3p1
RUN apk add --no-cache build-base linux-headers \
    openssl-dev openssl-libs-static \
    zlib-dev zlib-static \
    autoconf wget
WORKDIR /src
RUN wget -qO openssh.tar.gz \
    "https://cdn.openbsd.org/pub/OpenBSD/OpenSSH/portable/openssh-${OPENSSH_VERSION}.tar.gz" && \
    tar xzf openssh.tar.gz
WORKDIR /src/openssh-${OPENSSH_VERSION}
RUN LDFLAGS="-static" \
    CFLAGS="-O2" \
    ./configure \
        --prefix=/opt/devpod \
        --sbindir=/opt/devpod/sbin \
        --libexecdir=/opt/devpod/libexec \
        --sysconfdir=/opt/devpod/etc \
        --without-PAM \
        --without-selinux \
        --without-ldns \
        --with-privsep-user=root \
        --disable-strip
RUN make -j sshd sshd-session sshd-auth sftp-server
RUN mkdir -p /out/sbin /out/libexec && \
    cp sshd /out/sbin/sshd && \
    cp sftp-server /out/libexec/sftp-server && \
    if [ -f sshd-session ]; then cp sshd-session /out/libexec/sshd-session; fi && \
    if [ -f sshd-auth    ]; then cp sshd-auth    /out/libexec/sshd-auth;    fi && \
    strip /out/sbin/* /out/libexec/* 2>/dev/null || true

# --- Stage 2: build supervisor (static Go) ------------------------------
FROM golang:1.25-alpine AS supervisor-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/devpod-supervisor ./cmd/supervisor

# --- Stage 3: bash-static ----------------------------------------------
# Debian ships `bash-static`, a fully-static bash binary linked against glibc
# at /bin/bash-static. Alpine has no equivalent prebuilt package (its `bash`
# is dynamically linked to musl), so we pull from Debian here.
FROM debian:bookworm-slim AS bash-build
RUN apt-get update && \
    apt-get install -y --no-install-recommends bash-static binutils && \
    rm -rf /var/lib/apt/lists/*
RUN mkdir -p /out && cp /bin/bash-static /out/bash && strip /out/bash

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
        --prefix=/opt/devpod \
        --disable-dynamic --enable-static \
        --disable-cap --disable-pcre \
        --without-tcsetpgrp && \
    for mod in \
      zsh/regex zsh/system zsh/param/private \
      zsh/zutil zsh/computil zsh/complist zsh/complete \
      zsh/zle zsh/zleparameter \
      zsh/datetime zsh/parameter zsh/langinfo zsh/terminfo zsh/mathfunc; do \
      sed -i "/^name=${mod} /s/link=no/link=static/" config.modules; \
    done && \
    make -j && \
    make install DESTDIR=/zsh-install && \
    mkdir -p /out && cp Src/zsh /out/zsh && strip /out/zsh

# --- Stage 5: fish 4.x (upstream prebuilt static-pie binary) -----------
# fish 4.x release tarballs ship a Cargo/musl static-pie binary that runs
# in scratch. Rebuilding from source would need CMake + Rust + ncurses-
# static + pcre2-dev (~10 min cold cross-build via qemu); upstream's binary
# is what we'd produce, so we download + verify SHA256 instead.
FROM alpine:3.20 AS fish-build
ARG FISH_VERSION=4.7.1
ARG FISH_SHA256_x86_64=345388add316b94a847b08cef01f1b46e85b98215328271ee22a21555a3204df
ARG FISH_SHA256_aarch64=72957265b8629fbb3cdb215260c302f6154403dbaf35d38103aac6af689fb7f6
ARG TARGETARCH
RUN apk add --no-cache wget xz binutils
WORKDIR /src
RUN case "$TARGETARCH" in \
        amd64) FISH_ARCH=x86_64;  FISH_SHA="$FISH_SHA256_x86_64" ;; \
        arm64) FISH_ARCH=aarch64; FISH_SHA="$FISH_SHA256_aarch64" ;; \
        *) echo "fish-build: unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac && \
    wget -qO fish.tar.xz \
        "https://github.com/fish-shell/fish-shell/releases/download/${FISH_VERSION}/fish-${FISH_VERSION}-linux-${FISH_ARCH}.tar.xz" && \
    echo "${FISH_SHA}  fish.tar.xz" | sha256sum -c - && \
    tar xJf fish.tar.xz && \
    mkdir -p /out && cp fish /out/fish && strip /out/fish

# --- Stage 6: GNU coreutils (static musl from source) + busybox --------
# GNU coreutils statically linked against musl. Each utility (ls, cat,
# cp, ...) is its own binary; stripped, the set is ~20 MB. busybox is
# bundled in /out/busybox-bin/ for everything coreutils doesn't ship
# (top, ps, find, awk, sed, grep, vi, ...). In the Final stage the
# busybox bundle gets COPYed FIRST, then GNU coreutils OVERWRITES the
# overlapping symlinks so GNU wins on its own ground.
FROM alpine:3.20 AS coreutils-build
ARG COREUTILS_VERSION=9.7
RUN apk add --no-cache build-base wget xz busybox-static

# GNU coreutils source build (static musl)
WORKDIR /src
RUN wget -qO coreutils.tar.xz \
    "https://ftp.gnu.org/gnu/coreutils/coreutils-${COREUTILS_VERSION}.tar.xz" && \
    tar xJf coreutils.tar.xz
WORKDIR /src/coreutils-${COREUTILS_VERSION}
RUN FORCE_UNSAFE_CONFIGURE=1 LDFLAGS="-static" CFLAGS="-O2" \
    ./configure --prefix=/out/gnu \
                --enable-no-install-program=stdbuf
RUN make -j2
RUN make install-exec
RUN strip /out/gnu/bin/*

# busybox bundle: copy the static binary + create applet symlinks.
# Skip names that would collide with our real binaries from earlier stages
# (bash/zsh/fish) or with sshd's executables. coreutils overlap is handled
# at COPY-merge time in the Final scratch stage.
RUN mkdir -p /out/busybox-bin && cp /bin/busybox.static /out/busybox-bin/busybox && \
    cd /out/busybox-bin && \
    for a in $(./busybox --list); do \
        case "$a" in \
            bash|zsh|fish|sshd|sftp-server) continue ;; \
            init|login|passwd|su|nologin|poweroff|reboot|halt|getty) continue ;; \
            mount|umount|switch_root|pivot_root|chroot) continue ;; \
            crond|syslogd|klogd|mdev|udhcpc|udhcpc6|ifup|ifdown) continue ;; \
            chpasswd|adduser|addgroup|deluser|delgroup|cryptpw|mkpasswd) continue ;; \
        esac; \
        ln -sf busybox "$a"; \
    done

# --- Stage 7: terminfo database ----------------------------------------
FROM alpine:3.20 AS terminfo-build
RUN apk add --no-cache ncurses-terminfo
RUN mkdir -p /out && cp -r /usr/share/terminfo /out/terminfo

# --- Final stage: scratch image for the initContainer -----------------
FROM scratch
COPY --from=sshd-build       /out/sbin/                  /opt/devpod/sbin/
COPY --from=sshd-build       /out/libexec/               /opt/devpod/libexec/
COPY --from=supervisor-build /out/devpod-supervisor      /opt/devpod/devpod-supervisor
COPY --from=bash-build       /out/bash                   /opt/devpod/bin/bash
COPY --from=zsh-build        /out/zsh                    /opt/devpod/bin/zsh
COPY --from=zsh-build        /zsh-install/opt/devpod/share/zsh/ /opt/devpod/share/zsh/
COPY --from=fish-build       /out/fish                   /opt/devpod/bin/fish
# Order matters: busybox first, GNU coreutils second — GNU OVERWRITES the
# busybox symlinks for overlapping applet names (ls, cat, cp, ...).
COPY --from=coreutils-build  /out/busybox-bin/           /opt/devpod/bin/
COPY --from=coreutils-build  /out/gnu/bin/               /opt/devpod/bin/
COPY --from=terminfo-build   /out/terminfo               /opt/devpod/share/terminfo
COPY images/supervisor/sshd_config                       /opt/devpod/etc/sshd_config
