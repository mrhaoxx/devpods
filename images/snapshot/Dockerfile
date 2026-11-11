# syntax=docker/dockerfile:1.7
# Image for DevPod snapshot Jobs: one static binary, devpod-snapshot, that
# commits a container and pushes the image through the node's container
# runtime API (containerd or Docker). It is its own Go module so the
# containerd / nerdctl libraries stay out of the main build.
# Build context: the repository root.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY images/snapshot/go.mod images/snapshot/go.sum ./
COPY images/snapshot/*.go ./
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/devpod-snapshot .

# static: CA certificates for the registry push, no shell. The Job runs as
# root because the runtime sockets are root-only on the nodes.
FROM gcr.io/distroless/static
COPY --from=build /out/devpod-snapshot /devpod-snapshot
USER 0
ENTRYPOINT ["/devpod-snapshot"]
