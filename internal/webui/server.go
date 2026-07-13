// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/mrhaoxx/devpod/web"
)

// originGuard rejects mutating cross-origin requests. Browsers send
// Origin on all cross-site and same-site POST/PUT/PATCH/DELETE; a
// missing header means a non-browser client (curl, tests) which the
// cookie requirement already covers.
func (s *Server) originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if o := r.Header.Get("Origin"); o != "" && s.Origin != "" && o != s.Origin {
				s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "cross-origin request rejected", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Routes assembles the full webui handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	if s.OAuth != nil {
		mux.HandleFunc("GET /auth/login", s.OAuth.HandleLogin)
		mux.HandleFunc("GET /auth/callback", s.OAuth.HandleCallback)
	}
	// logout works for any login path; password login + config are
	// always registered (they self-gate on s.PasswordAuth).
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/config", s.handleAuthConfig)
	mux.HandleFunc("POST /api/auth/password", s.handlePasswordLogin)

	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("PUT /api/me/password", s.handleChangePassword)
	mux.HandleFunc("GET /api/me/pubkeys", s.handleGetPubkeys)
	mux.HandleFunc("PUT /api/me/pubkeys", s.handlePutPubkeys)
	mux.HandleFunc("GET /api/admin/users", s.handleListUsers)
	mux.HandleFunc("POST /api/admin/users", s.handleCreateUser)
	mux.HandleFunc("PATCH /api/admin/users/{name}", s.handlePatchUser)
	mux.HandleFunc("DELETE /api/admin/users/{name}", s.handleDeleteUser)
	mux.HandleFunc("GET /api/templates", s.handleListTemplates)
	mux.HandleFunc("GET /api/devpods", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			s.handleWatchDevPods(w, r)
			return
		}
		s.handleListDevPods(w, r)
	})
	mux.HandleFunc("POST /api/devpods", s.handleCreateDevPod)
	mux.HandleFunc("GET /api/devpods/{name}", s.handleGetDevPod)
	mux.HandleFunc("PATCH /api/devpods/{name}", s.handlePatchDevPod)
	mux.HandleFunc("DELETE /api/devpods/{name}", s.handleDeleteDevPod)
	mux.HandleFunc("GET /api/devpods/{name}/events", s.handleDevPodEvents)
	// Single SSE connection carrying both DevPod status and its events.
	mux.HandleFunc("GET /api/devpods/{name}/stream", s.handleDevPodStream)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint", nil)
	})

	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err) // embed layout is fixed at build time
	}
	files := http.FileServerFS(dist)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve real files (assets with extensions); SPA-fallback the rest.
		if strings.Contains(r.URL.Path, ".") {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFileFS(w, r, dist, "index.html")
	})

	return s.originGuard(mux)
}
