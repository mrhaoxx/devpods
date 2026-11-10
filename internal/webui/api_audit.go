// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

const defaultAuditRange = 7 * 24 * time.Hour

func (s *Server) registerAuditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/audit/sessions", s.handleAuditSessions)
	mux.HandleFunc("GET /api/audit/sessions/{id}", s.handleAuditSession)
	mux.HandleFunc("GET /api/audit/sessions/{id}/channels/{ch}/cast", s.handleAuditCast)
	mux.HandleFunc("GET /api/audit/sessions/{id}/channels/{ch}/live", s.handleAuditLive)
	mux.HandleFunc("GET /api/audit/sessions/{id}/channels/{ch}/raw", s.handleAuditRaw)
	mux.HandleFunc("POST /api/audit/sessions/{id}/disconnect", s.handleAuditDisconnect)
	mux.HandleFunc("GET /api/audit/files/{event}/content", s.handleAuditFileContent)
	mux.HandleFunc("GET /api/audit/commands", s.handleAuditCommands)
	mux.HandleFunc("GET /api/audit/files", s.handleAuditFiles)
	mux.HandleFunc("GET /api/audit/alerts", s.handleAuditAlerts)
	mux.HandleFunc("GET /api/audit/access-log", s.handleAuditAccessLog)
	mux.HandleFunc("GET /api/audit/presence", s.handleAuditPresence)
	mux.HandleFunc("GET /api/admin/users/{name}/audit-policy", s.handleGetAuditPolicy)
	mux.HandleFunc("PUT /api/admin/users/{name}/audit-policy", s.handlePutAuditPolicy)
	mux.HandleFunc("GET /api/admin/audit/policy-default", s.handleGetAuditPolicyDefault)
}

// auditSession gates every audit endpoint: audit on + a fresh session.
func (s *Server) auditSession(w http.ResponseWriter, r *http.Request) (Session, bool) {
	if s.Audit == nil {
		s.writeErr(w, http.StatusNotFound, "AUDIT_DISABLED", "session audit is not enabled", nil)
		return Session{}, false
	}
	return s.freshSession(w, r)
}

func (s *Server) auditAdmin(w http.ResponseWriter, r *http.Request) (Session, bool) {
	sess, ok := s.auditSession(w, r)
	if ok && !sess.Admin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "admin only", nil)
		return sess, false
	}
	return sess, ok
}

// timeRange parses from/to (RFC 3339), defaulting to the last 7 days.
func timeRange(r *http.Request) (from, to time.Time, err error) {
	to = time.Now().UTC()
	if v := r.URL.Query().Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, fmt.Errorf("invalid to: %w", err)
		}
	}
	from = to.Add(-defaultAuditRange)
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, fmt.Errorf("invalid from: %w", err)
		}
	}
	return from, to, nil
}

func queryLimit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

// scopeUser is the user a list query may cover: non-admins are always
// pinned to themselves.
func scopeUser(sess Session, r *http.Request) string {
	if !sess.Admin {
		return sess.User
	}
	return r.URL.Query().Get("user")
}

// devpodUID resolves a DevPod name filter; ok=false means "no such
// DevPod", which callers answer with an empty page.
func (s *Server) devpodUID(r *http.Request) (uid string, ok bool) {
	name := r.URL.Query().Get("devpod")
	if name == "" {
		return "", true
	}
	var dp devpodv1alpha1.DevPod
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: s.NS, Name: name}, &dp); err != nil {
		return "", false
	}
	return string(dp.UID), true
}

func (s *Server) auditStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found", nil)
	case errors.Is(err, store.ErrRangeRequired):
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
	default:
		slog.Error("audit store", "err", err)
		s.writeErr(w, http.StatusServiceUnavailable, "INTERNAL", "audit store unavailable", nil)
	}
}

func (s *Server) insertAccess(r *http.Request, viewer, action, sessionID, target string) error {
	err := s.Audit.Store.InsertAccessLog(r.Context(), store.AccessLogRow{
		TS: time.Now().UTC(), Viewer: viewer, Action: action, TargetSession: sessionID, TargetFile: target, ClientIP: r.RemoteAddr,
	})
	if err != nil {
		slog.Error("audit access log", "err", err, "viewer", viewer, "action", action)
	}
	return err
}

// logAccess records a content access before the content is served
// (§9.1: access_log must not miss one). On failure it answers 503 and
// returns false; the caller then serves nothing.
func (s *Server) logAccess(w http.ResponseWriter, r *http.Request, viewer, action, sessionID, target string) bool {
	if err := s.insertAccess(r, viewer, action, sessionID, target); err != nil {
		s.writeErr(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "the access could not be recorded; try again later", nil)
		return false
	}
	return true
}

type page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next"`
}

func (s *Server) handleAuditSessions(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditSession(w, r)
	if !ok {
		return
	}
	from, to, err := timeRange(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	uid, found := s.devpodUID(r)
	if !found {
		s.writeJSON(w, http.StatusOK, page[store.SessionSummary]{Items: []store.SessionSummary{}})
		return
	}
	q := r.URL.Query()
	items, next, err := s.Audit.Store.ListSessions(r.Context(), store.SessionFilter{
		User: scopeUser(sess, r), DevPodUID: uid, From: from, To: to,
		HasFiles: q.Get("hasFiles") == "true", HasAlerts: q.Get("hasAlerts") == "true",
		Cursor: q.Get("cursor"), Limit: queryLimit(r),
	})
	if err != nil {
		s.auditStoreErr(w, err)
		return
	}
	if items == nil {
		items = []store.SessionSummary{}
	}
	s.writeJSON(w, http.StatusOK, page[store.SessionSummary]{Items: items, Next: next})
}

// visibleSession loads a session the caller may see; non-admins get 404
// for other users' sessions (no existence leak).
func (s *Server) visibleSession(w http.ResponseWriter, r *http.Request, sess Session) (*store.SessionDetail, bool) {
	d, err := s.Audit.Store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		s.auditStoreErr(w, err)
		return nil, false
	}
	if !sess.Admin && d.Session.User != sess.User {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found", nil)
		return nil, false
	}
	return d, true
}

func (s *Server) handleAuditSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditSession(w, r)
	if !ok {
		return
	}
	d, ok := s.visibleSession(w, r, sess)
	if !ok {
		return
	}
	if d.Session.User != sess.User && !s.logAccess(w, r, sess.User, "view_session", d.Session.SessionID, "") {
		return
	}
	s.writeJSON(w, http.StatusOK, d)
}

// segmentIterator walks a session's raw segments in order.
func (s *Server) segmentIterator(r *http.Request, d *store.SessionDetail) rawlog.Iterator {
	segs := append([]store.RawSegmentRow(nil), d.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].Seq < segs[j].Seq })
	return rawlog.Concat(len(segs), func(i int) (io.ReadCloser, error) {
		return s.Audit.Objects.Get(r.Context(), segs[i].S3Key)
	})
}

func channelParam(r *http.Request) (uint32, error) {
	n, err := strconv.ParseUint(r.PathValue("ch"), 10, 32)
	return uint32(n), err
}

func (s *Server) handleAuditCast(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditSession(w, r)
	if !ok {
		return
	}
	d, ok := s.visibleSession(w, r, sess)
	if !ok {
		return
	}
	ch, err := channelParam(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "invalid channel", nil)
		return
	}
	if !s.logAccess(w, r, sess.User, "replay", d.Session.SessionID, fmt.Sprintf("channel/%d", ch)) {
		return
	}
	w.Header().Set("Content-Type", "application/x-asciicast")
	if err := rawlog.WriteCast(w, s.segmentIterator(r, d), ch); err != nil {
		slog.Error("audit cast", "session", d.Session.SessionID, "err", err)
	}
}

func (s *Server) handleAuditRaw(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditAdmin(w, r)
	if !ok {
		return
	}
	d, ok := s.visibleSession(w, r, sess)
	if !ok {
		return
	}
	ch, err := channelParam(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "invalid channel", nil)
		return
	}
	kind := map[string]rawlog.Kind{"in": rawlog.KindDataIn, "out": rawlog.KindDataOut, "err": rawlog.KindDataErr}[r.URL.Query().Get("stream")]
	if kind == 0 {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "stream must be in, out or err", nil)
		return
	}
	if !s.logAccess(w, r, sess.User, "download_raw", d.Session.SessionID, fmt.Sprintf("channel/%d/%s", ch, r.URL.Query().Get("stream"))) {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-ch%d-%s.bin"`, d.Session.SessionID, ch, r.URL.Query().Get("stream")))
	it := s.segmentIterator(r, d)
	for {
		rec, err := it.Next()
		if err != nil {
			if err != io.EOF {
				slog.Error("audit raw", "session", d.Session.SessionID, "err", err)
			}
			return
		}
		if rec.Channel == ch && rec.Kind == kind {
			if _, err := w.Write(rec.Payload); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleAuditFileContent(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditSession(w, r)
	if !ok {
		return
	}
	fe, err := s.Audit.Store.GetFileEvent(r.Context(), r.PathValue("event"))
	if err != nil {
		s.auditStoreErr(w, err)
		return
	}
	if (!sess.Admin && fe.User != sess.User) || fe.S3Key == "" {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found", nil)
		return
	}
	body, err := s.Audit.Objects.Get(r.Context(), fe.S3Key)
	if err != nil {
		s.auditStoreErr(w, err)
		return
	}
	defer body.Close()
	if !s.logAccess(w, r, sess.User, "download_file", fe.SessionID, fe.EventID) {
		return
	}
	name := strings.NewReplacer(`"`, "", `\`, "", "\r", "", "\n", "").Replace(path.Base(fe.Path))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	_, _ = io.Copy(w, body)
}

func (s *Server) handleAuditCommands(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditSession(w, r)
	if !ok {
		return
	}
	from, to, err := timeRange(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	uid, found := s.devpodUID(r)
	if !found {
		s.writeJSON(w, http.StatusOK, page[store.CommandRow]{Items: []store.CommandRow{}})
		return
	}
	q := r.URL.Query()
	items, next, err := s.Audit.Store.SearchCommands(r.Context(), store.CommandFilter{
		User: scopeUser(sess, r), DevPodUID: uid, Q: q.Get("q"), From: from, To: to, Cursor: q.Get("cursor"), Limit: queryLimit(r),
	})
	if err != nil {
		s.auditStoreErr(w, err)
		return
	}
	if items == nil {
		items = []store.CommandRow{}
	}
	s.writeJSON(w, http.StatusOK, page[store.CommandRow]{Items: items, Next: next})
}

func (s *Server) handleAuditFiles(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditSession(w, r)
	if !ok {
		return
	}
	from, to, err := timeRange(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	uid, found := s.devpodUID(r)
	if !found {
		s.writeJSON(w, http.StatusOK, page[store.FileEventRow]{Items: []store.FileEventRow{}})
		return
	}
	q := r.URL.Query()
	items, next, err := s.Audit.Store.SearchFiles(r.Context(), store.FileFilter{
		User: scopeUser(sess, r), DevPodUID: uid, Path: q.Get("path"), SHA256: q.Get("sha256"),
		From: from, To: to, Cursor: q.Get("cursor"), Limit: queryLimit(r),
	})
	if err != nil {
		s.auditStoreErr(w, err)
		return
	}
	if items == nil {
		items = []store.FileEventRow{}
	}
	s.writeJSON(w, http.StatusOK, page[store.FileEventRow]{Items: items, Next: next})
}

func (s *Server) handleAuditAlerts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auditAdmin(w, r); !ok {
		return
	}
	from, to, err := timeRange(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	items, next, err := s.Audit.Store.ListAlerts(r.Context(), store.TimeFilter{From: from, To: to, Cursor: r.URL.Query().Get("cursor"), Limit: queryLimit(r)})
	if err != nil {
		s.auditStoreErr(w, err)
		return
	}
	if items == nil {
		items = []store.AlertRow{}
	}
	s.writeJSON(w, http.StatusOK, page[store.AlertRow]{Items: items, Next: next})
}

func (s *Server) handleAuditAccessLog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auditAdmin(w, r); !ok {
		return
	}
	from, to, err := timeRange(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	items, next, err := s.Audit.Store.ListAccessLog(r.Context(), store.TimeFilter{From: from, To: to, Cursor: r.URL.Query().Get("cursor"), Limit: queryLimit(r)})
	if err != nil {
		s.auditStoreErr(w, err)
		return
	}
	if items == nil {
		items = []store.AccessLogRow{}
	}
	s.writeJSON(w, http.StatusOK, page[store.AccessLogRow]{Items: items, Next: next})
}

type presence struct {
	Count int      `json:"count"`
	Users []string `json:"users"`
}

// handleAuditPresence reports who is connected to the caller's DevPods
// (admins: every DevPod with ?scope=all) — the equivalent of `who`.
func (s *Server) handleAuditPresence(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditSession(w, r)
	if !ok {
		return
	}
	var dps []devpodv1alpha1.DevPod
	if sess.Admin && r.URL.Query().Get("scope") == "all" {
		var list devpodv1alpha1.DevPodList
		if err := s.Client.List(r.Context(), &list, client.InNamespace(s.NS)); err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		dps = list.Items
	} else {
		owned, err := s.ownedDevPods(r, sess.User)
		if err != nil {
			s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
			return
		}
		dps = owned
	}
	byUID := map[string]string{}
	uids := make([]string, 0, len(dps))
	for _, dp := range dps {
		byUID[string(dp.UID)] = dp.Name
		uids = append(uids, string(dp.UID))
	}
	got, err := s.Audit.Store.Presence(r.Context(), uids)
	if err != nil {
		s.auditStoreErr(w, err)
		return
	}
	items := map[string]presence{}
	for uid, users := range got {
		if name, ok := byUID[uid]; ok {
			items[name] = presence{Count: len(users), Users: users}
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) defaultAuditPolicy(r *http.Request) devpodv1alpha1.AuditPolicy {
	var gc devpodv1alpha1.GatewayConfig
	name := s.GatewayConfigName
	if name == "" {
		name = "default"
	}
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &gc); err != nil || gc.Spec.Audit == nil {
		return devpodv1alpha1.AuditPolicy{}
	}
	return gc.Spec.Audit.Policy
}

func (s *Server) handleGetAuditPolicyDefault(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auditAdmin(w, r); !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, s.defaultAuditPolicy(r))
}

func (s *Server) handleGetAuditPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auditAdmin(w, r); !ok {
		return
	}
	var u devpodv1alpha1.User
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: r.PathValue("name")}, &u); err != nil {
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "user not found", nil)
		return
	}
	def := s.defaultAuditPolicy(r)
	eff := policy.Resolve(&def, u.Spec.AuditPolicy)
	warnings := eff.Warnings()
	if warnings == nil {
		warnings = []string{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"default": def, "user": u.Spec.AuditPolicy, "effective": eff, "warnings": warnings})
}

func (s *Server) handlePutAuditPolicy(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auditAdmin(w, r)
	if !ok {
		return
	}
	var pol *devpodv1alpha1.AuditPolicy
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pol); err != nil {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "malformed policy: "+err.Error(), nil)
		return
	}
	name := r.PathValue("name")
	if err := s.setAuditPolicy(r.Context(), name, pol); err != nil {
		if apierrors.IsNotFound(err) {
			s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "user not found (LDAP-only users follow the cluster default)", nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "update rejected", err.Error())
		return
	}
	_ = s.insertAccess(r, sess.User, "edit_policy", "", "user/"+name) // the edit already happened
	s.writeJSON(w, http.StatusOK, pol)
}

// setAuditPolicy sets (nil: clears) a user's policy override.
func (s *Server) setAuditPolicy(ctx context.Context, name string, pol *devpodv1alpha1.AuditPolicy) error {
	return s.updateUser(ctx, name, func(u *devpodv1alpha1.User) error {
		u.Spec.AuditPolicy = pol
		return nil
	})
}
