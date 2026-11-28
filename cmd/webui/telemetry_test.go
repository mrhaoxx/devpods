// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrhaoxx/devpod/internal/webui"
)

// dp reaches the console two ways — the ingest listener (DevPods of the
// console's own cluster) and an agent's link (a joined cluster's) — and
// both answer all of what dp calls.
func TestDPRoutes(t *testing.T) {
	m := http.NewServeMux()
	dpRoutes(m, &webui.Server{})
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/dp/v1/cli"},
		{http.MethodGet, "/dp/v1/attach?probe=1"},
	} {
		if _, pattern := m.Handler(httptest.NewRequest(c.method, c.path, nil)); pattern == "" {
			t.Errorf("%s %s: not served", c.method, c.path)
		}
	}
}
