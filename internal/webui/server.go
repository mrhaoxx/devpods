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
			// a bearer request carries no ambient credential to abuse
			if o := r.Header.Get("Origin"); o != "" && s.Origin != "" && o != s.Origin && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
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
	mux.HandleFunc("POST /api/admin/users:batch", s.handleUsersBatch)
	mux.HandleFunc("POST /api/admin/users:import", s.handleImportUsers)
	mux.HandleFunc("GET /api/admin/devpods", s.handleAdminListDevPods)
	mux.HandleFunc("GET /api/kore/topology", s.handleKoreTopology)
	mux.HandleFunc("GET /api/templates", s.handleListTemplates)
	mux.HandleFunc("GET /api/devpods", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			// the watch stream is not filtered per DevPod
			if sess, ok := s.sessionFrom(w, r); !ok {
				return
			} else if sess.Token != nil && len(sess.Token.DevPods) > 0 {
				s.writeErr(w, http.StatusForbidden, "SCOPE", "this token is limited to specific DevPods; list without watch", nil)
				return
			}
			s.handleWatchDevPods(w, r)
			return
		}
		s.handleListDevPods(w, r)
	})
	mux.HandleFunc("POST /api/devpods", s.handleCreateDevPod)
	mux.HandleFunc("GET /api/devpods/{name}", s.handleGetDevPod)
	mux.HandleFunc("PATCH /api/devpods/{name}", s.handlePatchDevPod)
	mux.HandleFunc("DELETE /api/devpods/{name}", s.handleDeleteDevPod)
	mux.HandleFunc("POST /api/devpods/{name}/redeploy", s.handleRedeployDevPod)
	mux.HandleFunc("GET /api/devpods/{name}/http", s.handleGetHTTP)
	mux.HandleFunc("PUT /api/devpods/{name}/http", s.handlePutHTTP)
	mux.HandleFunc("POST /api/http/ticket", s.handleHTTPTicket)
	mux.HandleFunc("POST /api/devpods/{name}/snapshots", s.handleCreateSnapshot)
	mux.HandleFunc("GET /api/snapshots", s.handleListSnapshots)
	mux.HandleFunc("GET /api/snapshots/{id}", s.handleGetSnapshot)
	mux.HandleFunc("GET /api/snapshots/{id}/progress", s.handleSnapshotProgress)
	mux.HandleFunc("DELETE /api/snapshots/{id}", s.handleDeleteSnapshot)
	mux.HandleFunc("GET /api/me/tokens", s.handleListTokens)
	mux.HandleFunc("POST /api/me/tokens", s.handleCreateToken)
	mux.HandleFunc("DELETE /api/me/tokens/{id}", s.handleDeleteToken)
	mux.HandleFunc("GET /api/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /api/me/registries", s.handleListRegistries)
	mux.HandleFunc("PUT /api/me/registries/{host}", s.handlePutRegistry)
	mux.HandleFunc("DELETE /api/me/registries/{host}", s.handleDeleteRegistry)
	mux.HandleFunc("GET /api/nodes", s.handleNodes)
	mux.HandleFunc("GET /api/reservations", s.handleListReservations)
	mux.HandleFunc("GET /api/reservations/rules", s.handleReservationRules)
	mux.HandleFunc("POST /api/reservations", s.handleCreateReservation)
	mux.HandleFunc("PATCH /api/reservations/{id}", s.handlePatchReservation)
	mux.HandleFunc("DELETE /api/reservations/{id}", s.handleDeleteReservation)
	mux.HandleFunc("PUT /api/admin/nodes/{name}/reservable", s.handleSetReservable)
	mux.HandleFunc("GET /api/devpods/{name}/shell", s.handleDevPodShell)
	mux.HandleFunc("GET /api/devpods/{name}/events", s.handleDevPodEvents)
	// Single SSE connection carrying both DevPod status and its events.
	mux.HandleFunc("GET /api/devpods/{name}/stream", s.handleDevPodStream)
	mux.HandleFunc("GET /api/devpods/{name}/topology", s.handleDevPodTopology)
	s.registerAuditRoutes(mux)
	mux.HandleFunc("GET /api/live/devpods", s.handleLiveDevPods)
	mux.HandleFunc("GET /api/live/devpods/recent", s.handleLiveRecent)
	mux.HandleFunc("GET /api/devpods/{name}/metrics", s.handleDevPodMetrics)
	mux.HandleFunc("GET /api/devpods/{name}/info", s.handleDevPodInfo)
	mux.HandleFunc("GET /api/devpods/{name}/processes", s.handleDevPodProcesses)
	mux.HandleFunc("GET /api/admin/nodes", s.handleAdminNodes)
	mux.HandleFunc("GET /api/admin/nodes/recent", s.handleNodesRecent)
	mux.HandleFunc("GET /api/admin/nodes/{name}", s.handleAdminNode)
	mux.HandleFunc("GET /api/admin/live/overview", s.handleLiveOverview)
	mux.HandleFunc("GET /api/search", s.handleSearch)
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
