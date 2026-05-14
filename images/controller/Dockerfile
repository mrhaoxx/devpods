# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/devpod-controller ./cmd/controller

FROM gcr.io/distroless/static
COPY --from=build /out/devpod-controller /usr/local/bin/devpod-controller
ENTRYPOINT ["/usr/local/bin/devpod-controller"]
