// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"reflect"
	"testing"
)

func TestScopeCond(t *testing.T) {
	m := []ClusterNode{{"st", "g1"}, {"yc", "n2"}}
	for _, c := range []struct {
		name string
		s    *Scope
		k    scopeKind
		sql  string
		args []any
	}{
		{"everything", nil, scopeSessions, "", nil},
		{"nothing", &Scope{}, scopeSessions, "0", nil},
		{"own", &Scope{User: "a"}, scopeSessions, "(user = ?)", []any{"a"}},
		{"clusters", &Scope{Clusters: []string{"st"}}, scopeSessions, "(cluster IN ?)", []any{[]string{"st"}}},
		{"machines", &Scope{Machines: m}, scopeSessions, "(concat(cluster, '/', node) IN ?)", []any{[]string{"st/g1", "yc/n2"}}},
		{"all three", &Scope{User: "a", Clusters: []string{"st"}, Machines: m[:1]}, scopeSessions,
			"(user = ? OR cluster IN ? OR concat(cluster, '/', node) IN ?)", []any{"a", []string{"st"}, []string{"st/g1"}}},
		{"commands: machines by their session", &Scope{User: "a", Machines: m[:1]}, scopeBySession,
			"(user = ? OR session_id IN (SELECT session_id FROM db.sessions WHERE concat(cluster, '/', node) IN ?))", []any{"a", []string{"st/g1"}}},
		{"alerts: no user", &Scope{User: "a", Clusters: []string{"st"}}, scopeAlerts, "(cluster IN ?)", []any{[]string{"st"}}},
		{"events: one's own is what one owns or did", &Scope{User: "a"}, scopeEvents, "(owner = ? OR actor = ?)", []any{"a", "a"}},
		{"events: machines by their own node, and what else was done to what was on them", &Scope{User: "a", Machines: m[:1]}, scopeEvents,
			"(owner = ? OR actor = ? OR concat(cluster, '/', node) IN ? OR concat(cluster, '/', kind, '/', object, '/', object_uid) IN " +
				"(SELECT concat(cluster, '/', kind, '/', object, '/', object_uid) FROM db.events WHERE concat(cluster, '/', node) IN ?))",
			[]any{"a", "a", []string{"st/g1"}, []string{"st/g1"}}},
		{"alerts: only own is nothing", &Scope{User: "a"}, scopeAlerts, "0", nil},
	} {
		sql, args := scopeCond(c.s, c.k, "db")
		if sql != c.sql || !reflect.DeepEqual(args, c.args) {
			t.Errorf("%s:\n got  %q %#v\n want %q %#v", c.name, sql, args, c.sql, c.args)
		}
	}
}
