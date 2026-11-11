# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM alpine:3.22 AS download
ARG NERDCTL_VERSION=2.2.1
ARG TARGETARCH
RUN apk add --no-cache ca-certificates curl
WORKDIR /download
RUN set -eu; \
    archive="nerdctl-${NERDCTL_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    release="https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}"; \
    curl -fsSL "$release/$archive" -o "$archive"; \
    curl -fsSL "$release/SHA256SUMS" -o SHA256SUMS; \
    grep " ${archive}\$" SHA256SUMS | sha256sum -c -; \
    mkdir /out; \
    tar -xzf "$archive" -C /out nerdctl

# No target-platform RUN is needed for an amd64+arm64 build.
FROM alpine:3.22
COPY --from=download /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=download /out/nerdctl /usr/local/bin/nerdctl
CMD ["nerdctl", "--help"]
