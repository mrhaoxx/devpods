// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"strings"
)

// ClusterNode is a machine of a cluster.
type ClusterNode struct{ Cluster, Node string }

// Scope narrows rows to what a viewer may see (management grants design
// 2026-11-20 §6.2): their own (User), any of a whole cluster (Clusters),
// any on one of Machines. A nil Scope is everything (an admin); one with
// nothing in it, nothing.
type Scope struct {
	User     string
	Clusters []string
	Machines []ClusterNode
}

type scopeKind uint8

const (
	// scopeSessions: rows with user, cluster and node (sessions, HTTP requests)
	scopeSessions scopeKind = iota
	// scopeBySession: rows with user and cluster, on a machine through
	// their session (commands, file events)
	scopeBySession
	// scopeAlerts: rows with a cluster, on a machine through their
	// session, and no user — never one's own
	scopeAlerts
)

// scopeCond: s as one condition with its args; "" for no condition.
func scopeCond(s *Scope, k scopeKind, db string) (string, []any) {
	if s == nil {
		return "", nil
	}
	var parts []string
	var args []any
	if s.User != "" && k != scopeAlerts {
		parts, args = append(parts, "user = ?"), append(args, s.User)
	}
	if len(s.Clusters) > 0 {
		parts, args = append(parts, "cluster IN ?"), append(args, s.Clusters)
	}
	if len(s.Machines) > 0 {
		keys := make([]string, len(s.Machines))
		for i, m := range s.Machines {
			keys[i] = m.Cluster + "/" + m.Node
		}
		on := "concat(cluster, '/', node) IN ?"
		if k != scopeSessions {
			on = fmt.Sprintf("session_id IN (SELECT session_id FROM %s.sessions WHERE %s)", db, on)
		}
		parts, args = append(parts, on), append(args, keys)
	}
	if len(parts) == 0 {
		return "0", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// addScope narrows w to s.
func (w *where) addScope(s *Scope, k scopeKind, db string) {
	if cond, args := scopeCond(s, k, db); cond != "" {
		w.add(cond, args...)
	}
}
