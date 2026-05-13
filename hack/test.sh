#!/usr/bin/env bash
# Run unit tests with envtest binaries on PATH for the controller tests.
set -euo pipefail

ENVTEST_K8S_VERSION="${ENVTEST_K8S_VERSION:-1.31.0}"
BIN_DIR="${BIN_DIR:-$PWD/bin}"
mkdir -p "$BIN_DIR"

KUBEBUILDER_ASSETS="$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 \
  use "$ENVTEST_K8S_VERSION" --bin-dir "$BIN_DIR" -p path)"
export KUBEBUILDER_ASSETS

exec go test ./... -count=1 -race -coverprofile=cover.out "$@"
