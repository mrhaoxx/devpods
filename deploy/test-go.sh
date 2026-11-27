#!/usr/bin/env bash
# Copyright 2026 The DevPod Authors.
#
# SPDX-License-Identifier: Apache-2.0
#
# test-go.sh runs every Go test. The longest packages (internal/webui,
# internal/controllers) are each split into SHARDS processes, each with its
# own envtest apiserver, running beside the other packages. Arguments go to
# every go test (e.g. -race, -count=1, -timeout 30m).
#
#   bash deploy/test-go.sh -count=1
#   SHARDS=2 bash deploy/test-go.sh -race -count=1
set -euo pipefail
cd "$(dirname "$0")/.."

SHARDS=${SHARDS:-4}
big=(internal/webui internal/controllers)
mod=$(go list -m)
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

skip=$(printf '%s\n' "${big[@]/#/$mod/}")
mapfile -t others < <(go list ./... | grep -vxF "$skip")

pids=()
go test "$@" "${others[@]}" >"$out/0-others.log" 2>&1 &
pids+=($!)
for pkg in "${big[@]}"; do
  mapfile -t tests < <(go test -list '^Test' "./$pkg" | grep '^Test')
  for ((i = 0; i < SHARDS; i++)); do
    names=()
    for ((j = i; j < ${#tests[@]}; j += SHARDS)); do names+=("${tests[j]}"); done
    [ ${#names[@]} -gt 0 ] || continue
    re="^($(IFS='|'; echo "${names[*]}"))\$"
    go test "$@" -run "$re" "./$pkg" >"$out/${pkg//\//-}-$i.log" 2>&1 &
    pids+=($!)
  done
done

failed=0
for p in "${pids[@]}"; do
  wait "$p" || failed=1
done
for f in "$out"/*.log; do
  echo "== $(basename "$f" .log)"
  cat "$f"
done
exit $failed
