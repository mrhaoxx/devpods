# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
# VERSION: what it says it is (the 集群 page shows it); the image's tag
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/devpod-agent ./cmd/agent

FROM gcr.io/distroless/static
COPY --from=build /out/devpod-agent /usr/local/bin/devpod-agent
ENTRYPOINT ["/usr/local/bin/devpod-agent"]
