# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM node:22-alpine AS spa
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=spa /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /devpod-webui ./cmd/webui

FROM gcr.io/distroless/static
COPY --from=build /devpod-webui /usr/local/bin/devpod-webui
ENTRYPOINT ["/usr/local/bin/devpod-webui"]
