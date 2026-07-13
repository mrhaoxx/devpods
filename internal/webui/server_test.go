// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoutes(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.Origin = "https://ui.example.com"
	h := s.Routes()

	do := func(method, path, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := do("GET", "/api/devpods", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated api = %d", rec.Code)
	}
	alice := forge(sm, "gl-alice", false)
	if rec := do("POST", "/api/devpods", "https://evil.example.com", alice); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation = %d, want 403", rec.Code)
	}
	if rec := do("GET", "/", "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>") {
		t.Fatalf("SPA index: %d %s", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}
	if rec := do("GET", "/devpods/some-name", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("SPA fallback = %d", rec.Code)
	}
	if rec := do("GET", "/api/nonexistent", "", alice); rec.Code == http.StatusOK {
		t.Fatal("api paths must not fall through to SPA")
	}
}
