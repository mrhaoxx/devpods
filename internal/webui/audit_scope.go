// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"log/slog"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// The audit a viewer may see (management grants design 2026-11-20 §6):
// their own, and what grants of viewAudit give — whole clusters, and
// machines of the clusters reachable now. A machine not known (none
// recorded, gone from its cluster, its cluster away) counts only under a
// grant that holds whatever the machine.

// auditCluster is a cluster as grants judge it (name: "" in a console of
// one) and as the audit records it (label), and its clients while it can
// be reached (nil: away).
type auditCluster struct {
	name, label string
	cc          *clusters.ClusterClients
}

func (s *Server) auditClusters(ctx context.Context) []auditCluster {
	if s.Clusters == nil {
		cc := s.local()
		return []auditCluster{{name: cc.Name, label: s.clusterLabel(cc), cc: cc}}
	}
	live := map[string]*clusters.ClusterClients{}
	s.eachCluster("", func(cc *clusters.ClusterClients) { live[cc.Name] = cc })
	var out []auditCluster
	for _, n := range s.clusterNames(ctx) {
		out = append(out, auditCluster{name: n, label: n, cc: live[n]})
	}
	return out
}

// auditScope: nil (everything) for an admin; else the viewer's own (own)
// and what their viewAudit grants give.
func (s *Server) auditScope(ctx context.Context, sess Session, own bool) (*store.Scope, error) {
	if sess.Admin {
		return nil, nil
	}
	sc := &store.Scope{}
	if own {
		sc.User = sess.User
	}
	gs, groups, err := s.grantsFor(ctx, sess.User)
	if err != nil {
		// grants that cannot be read take nothing away and give nothing
		slog.Warn("audit_grants_unread", "user", sess.User, "err", err)
		return sc, nil
	}
	if !gs.Possible(devpodv1alpha1.PermViewAudit, sess.User, groups) {
		return sc, nil
	}
	for _, c := range s.auditClusters(ctx) {
		if gs.Has(devpodv1alpha1.PermViewAudit, sess.User, groups, c.name, nil) {
			sc.Clusters = append(sc.Clusters, c.label)
			continue
		}
		if c.cc == nil {
			continue // away: its machines cannot be told
		}
		var nodes corev1.NodeList
		if err := c.cc.Client.List(ctx, &nodes); err != nil {
			continue
		}
		for i := range nodes.Items {
			n := &nodes.Items[i]
			if gs.Has(devpodv1alpha1.PermViewAudit, sess.User, groups, c.name, &access.Node{Name: n.Name, Labels: n.Labels}) {
				sc.Machines = append(sc.Machines, store.ClusterNode{Cluster: c.label, Node: n.Name})
			}
		}
	}
	return sc, nil
}

// mayOnSession: an admin, or one a grant gives p on the session's machine.
func (s *Server) mayOnSession(ctx context.Context, sess Session, p devpodv1alpha1.GrantPermission, d *store.SessionDetail) bool {
	if sess.Admin {
		return true
	}
	gs, groups, err := s.grantsFor(ctx, sess.User)
	if err != nil {
		return false
	}
	for _, c := range s.auditClusters(ctx) {
		if c.label != d.Session.Cluster {
			continue
		}
		var node *access.Node
		if d.Session.Node != "" && c.cc != nil {
			var n corev1.Node
			if c.cc.Client.Get(ctx, types.NamespacedName{Name: d.Session.Node}, &n) == nil {
				node = &access.Node{Name: n.Name, Labels: n.Labels}
			}
		}
		return gs.Has(p, sess.User, groups, c.name, node)
	}
	return false // a session of no cluster (the console's CLI)
}

// mayOnFile: a grant of viewAudit on the machine of the file's session;
// a file of no session counts its machine unknown.
func (s *Server) mayOnFile(ctx context.Context, sess Session, fe *store.FileEventRow) bool {
	d := &store.SessionDetail{Session: store.SessionRow{Cluster: fe.Cluster}}
	if fe.SessionID != "" {
		var err error
		if d, err = s.Audit.Store.GetSession(ctx, fe.SessionID); err != nil {
			return false
		}
	}
	return s.mayOnSession(ctx, sess, devpodv1alpha1.PermViewAudit, d)
}

// auditListScope: what an audit list may cover — the viewer's scope — and its
// user= filter, which only narrows it.
func (s *Server) auditListScope(w http.ResponseWriter, r *http.Request, sess Session) (*store.Scope, string, bool) {
	sc, err := s.auditScope(r.Context(), sess, true)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return nil, "", false
	}
	return sc, r.URL.Query().Get("user"), true
}
