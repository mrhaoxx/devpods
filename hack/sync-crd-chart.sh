#!/usr/bin/env bash
# Mirror controller-gen-generated CRD YAMLs from config/crd/bases/ into
# the Helm chart so the two locations cannot drift. Run by go:generate
# in api/v1alpha1/doc.go.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
SRC="$ROOT/config/crd/bases"
DST="$ROOT/deploy/chart/crds"

mkdir -p "$DST"
cp "$SRC"/devpod.io_*.yaml "$DST"/
