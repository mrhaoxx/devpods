// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"net/http"

	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// AuditStore is the read side of the audit store the webui needs
// (satisfied by *store.CH).
type AuditStore interface {
	ListSessions(context.Context, store.SessionFilter) ([]store.SessionSummary, string, error)
	GetSession(context.Context, string) (*store.SessionDetail, error)
	SearchCommands(context.Context, store.CommandFilter) ([]store.CommandRow, string, error)
	SearchFiles(context.Context, store.FileFilter) ([]store.FileEventRow, string, error)
	SearchHTTP(context.Context, store.HTTPFilter) ([]store.HTTPRequestRow, string, error)
	GetFileEvent(context.Context, string) (*store.FileEventRow, error)
	ListAlerts(context.Context, store.TimeFilter) ([]store.AlertRow, string, error)
	ListAccessLog(context.Context, store.TimeFilter) ([]store.AccessLogRow, string, error)
	InsertAccessLog(context.Context, store.AccessLogRow) error
	Presence(context.Context, []string) (map[string][]string, error)
}

// AuditDeps wires the audit backends; nil on Server means audit is off.
type AuditDeps struct {
	Store   AuditStore
	Objects store.ObjectStore
}

// freshSession is sessionFrom with the admin bit re-resolved from the
// --admins allowlist and the cached User — never trusted from the
// cookie, so revoking admin takes effect immediately.
func (s *Server) freshSession(w http.ResponseWriter, r *http.Request) (Session, bool) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return sess, false
	}
	var u devpodv1alpha1.User
	up := &u
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: sess.User}, &u); err != nil {
		up = nil
	}
	sess.Admin = s.adminFor(sess.User, up)
	return sess, true
}
