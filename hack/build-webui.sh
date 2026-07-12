#!/usr/bin/env bash
# Build the SPA into web/dist (embedded by web/embed.go). Requires
# Node >= 20. CI and images/webui/Dockerfile run this; the committed
# web/dist/index.html is only a placeholder for pure-Go builds.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)/web"
npm ci
npm run build
