// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"
	"net/url"

	"github.com/mrhaoxx/devpod/internal/audit"
)

// handleAuditDisconnect serves POST /api/audit/sessions/{id}/disconnect:
// an admin cuts an open SSH session (audit spec §17). The webui asks the
// session's gateway over the internal live endpoint; the access log row
// is written first, so an attempt is on record even if the cut fails.
func (s *Server) handleAuditDisconnect(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditAdmin(w, r)
	if !ok {
		return
	}
	ls := s.LiveSessions
	if ls == nil {
		s.writeErr(w, http.StatusNotFound, "LIVE_DISABLED", "the gateway control endpoint is not enabled", nil)
		return
	}
	d, ok := s.visibleSession(w, r, sess)
	if !ok {
		return
	}
	if d.Session.ClosedAt != nil {
		s.writeErr(w, http.StatusConflict, "SESSION_CLOSED", "the session has already ended", nil)
		return
	}
	if !s.logAccess(w, r, sess.User, "disconnect", d.Session.SessionID, "") {
		return
	}
	addr, err := ls.resolve(r.Context(), d.Session.GatewayPod)
	if err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "LIVE_UNAVAILABLE", "the session's gateway cannot be reached: "+err.Error(), nil)
		return
	}
	q := url.Values{"session": {d.Session.SessionID}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://"+addr+audit.KillPath+"?"+q.Encode(), nil)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	req.Header.Set("Authorization", "Bearer "+ls.key)
	resp, err := ls.client.Do(req)
	if err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "LIVE_UNAVAILABLE", "the session's gateway cannot be reached", nil)
		return
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		w.WriteHeader(http.StatusNoContent)
	case http.StatusNotFound:
		s.writeErr(w, http.StatusConflict, "SESSION_CLOSED", "the session has already ended", nil)
	default:
		s.writeErr(w, http.StatusBadGateway, "LIVE_UNAVAILABLE", "gateway: "+resp.Status, nil)
	}
}
