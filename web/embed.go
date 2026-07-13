// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package web embeds the built SPA. web/dist ships a committed
// placeholder so `go build ./...` works without Node; the real bundle
// is produced by hack/build-webui.sh (and inside images/webui).
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
