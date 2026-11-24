// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func grant(name, match string, ps ...v1.GrantPermission) v1.AccessGrant {
	return v1.AccessGrant{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.AccessGrantSpec{Match: match, Permissions: ps}}
}

func TestGrantsHas(t *testing.T) {
	g := CompileGrants([]v1.AccessGrant{
		grant("st2-ops", "group = ops and cluster = st2", v1.PermSetReservable),
		grant("alice-audit", "user = alice", v1.PermViewAudit),
	}, known)
	ops := map[string]bool{"ops": true}
	g1 := &Node{Name: "g1"}
	if !g.Has(v1.PermSetReservable, "bob", ops, "st2", g1) {
		t.Error("ops in st2")
	}
	if g.Has(v1.PermSetReservable, "bob", ops, "yc", g1) {
		t.Error("ops in yc")
	}
	if g.Has(v1.PermSetReservable, "carol", nil, "st2", g1) {
		t.Error("not in ops")
	}
	// a union: each grant gives its own, and nothing else
	if !g.Has(v1.PermViewAudit, "alice", nil, "yc", g1) || g.Has(v1.PermSetReservable, "alice", nil, "st2", g1) {
		t.Error("alice")
	}
	if g.Has(v1.PermDisconnectSessions, "bob", ops, "st2", g1) {
		t.Error("a permission given by none")
	}
}

// A machine not known (a session that did not record it, a node gone):
// only a grant that holds whatever the machine counts.
func TestGrantsMachineUnknown(t *testing.T) {
	g := CompileGrants([]v1.AccessGrant{
		grant("one", "user = a and machine = g1", v1.PermViewAudit),
		grant("all", "user = b and cluster = st", v1.PermViewAudit),
	}, known)
	if g.Has(v1.PermViewAudit, "a", nil, "st", nil) {
		t.Error("a machine grant, machine not known")
	}
	if !g.Has(v1.PermViewAudit, "b", nil, "st", nil) {
		t.Error("a whole-cluster grant, machine not known")
	}
}

// A grant that cannot be read, holds an item that can match nothing,
// names a group not there, or has no condition, gives nothing — and is said.
func TestGrantsBroken(t *testing.T) {
	for match, says := range map[string]string{
		"cluster in (":           "第 13 个字符",
		"group = gone":           "点名了不存在的组 gone",
		`user = "["`:             "[",
		"":                       "条件是空的",
		"not group = gone":       "点名了不存在的组 gone",
		"everyone and user = a[": "a[",
	} {
		g := CompileGrants([]v1.AccessGrant{grant("bad", match, v1.PermViewAudit, v1.PermSetReservable)}, known)
		if g.Has(v1.PermViewAudit, "anyone", nil, "st", &Node{Name: "g1"}) || g.Has(v1.PermSetReservable, "anyone", nil, "st", nil) {
			t.Errorf("%q gives", match)
		}
		if pr := g.Problems(); len(pr) != 1 || pr[0].Name != "bad" || !strings.Contains(pr[0].Message, says) {
			t.Errorf("%q: problems %+v", match, pr)
		}
	}
	if pr := CompileGrants([]v1.AccessGrant{grant("glob", "group = gone*", v1.PermViewAudit)}, known).Problems(); len(pr) != 0 {
		t.Errorf("a glob of groups: %+v", pr)
	}
}

// Whether someone may hold a permission somewhere (what the page shows).
func TestGrantsPossible(t *testing.T) {
	g := CompileGrants([]v1.AccessGrant{grant("a", "user = a and machine = g1", v1.PermViewAudit)}, known)
	if !g.Possible(v1.PermViewAudit, "a", nil) {
		t.Error("a may view somewhere")
	}
	if g.Possible(v1.PermSetReservable, "a", nil) {
		t.Error("a may not set reservable")
	}
	if g.Possible(v1.PermViewAudit, "b", nil) {
		t.Error("b may view nowhere")
	}
}

// PossibleIn: some grant of p could hold somewhere in the cluster (the
// machine not known) — what decides whether a cluster is shown at all.
func TestGrantSetPossibleIn(t *testing.T) {
	g := CompileGrants([]v1.AccessGrant{
		grant("keeper", "user = alice and cluster = st and machine = g1", v1.PermManageReservations),
		grant("audit", "user = alice and cluster = st2", v1.PermViewAudit),
	}, known)
	for _, c := range []struct {
		user, cluster string
		want          bool
	}{
		{"alice", "st", true},
		{"alice", "st2", false}, // a grant there, of another permission
		{"bob", "st", false},
	} {
		if got := g.PossibleIn(v1.PermManageReservations, c.user, nil, c.cluster); got != c.want {
			t.Errorf("%s in %s: %v", c.user, c.cluster, got)
		}
	}
}
