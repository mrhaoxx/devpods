// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"testing"

	v1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func allow(privileged, reservation bool) v1.AccessEffect {
	return v1.AccessEffect{Allow: true, Privileged: privileged, NeedsReservation: reservation}
}

// The first rule whose resource and subject match decides; else the default.
func TestDecideOrder(t *testing.T) {
	p := &v1.AccessPolicySpec{
		Rules: []v1.AccessRule{
			{Cluster: "yc", Machines: []string{"c500-8-65-*"}, Subject: v1.AccessSubject{Kind: "some", Groups: []string{"infer"}}, Effect: allow(true, false)},
			{Cluster: "yc", Machines: []string{"c500-8-65-*"}, Subject: v1.AccessSubject{Kind: "everyone"}, Effect: v1.AccessEffect{}},
			{Cluster: "yc", Subject: v1.AccessSubject{Kind: "everyone"}, Effect: allow(false, true)},
			{Machines: []string{"gpu=B300"}, Subject: v1.AccessSubject{Kind: "some", Users: []string{"wyc"}}, Effect: allow(true, false)},
		},
		Default: v1.DefaultAccessEffect(),
	}
	inInfer := map[string]bool{"infer": true}
	n33 := Node{Name: "c500-8-65-33"}
	if d := Decide(p, "wyc", inInfer, "yc", n33); d.Rule != 0 || !d.Allow || !d.Privileged {
		t.Fatalf("group member on c500-8-65-33: %+v", d)
	}
	if d := Decide(p, "bob", nil, "yc", n33); d.Rule != 1 || d.Allow {
		t.Fatalf("others on c500-8-65-33: %+v", d)
	}
	if d := Decide(p, "bob", nil, "yc", Node{Name: "c500-16-23"}); d.Rule != 2 || !d.Allow || !d.NeedsReservation || d.Privileged {
		t.Fatalf("others on c500-16-23: %+v", d)
	}
	if d := Decide(p, "wyc", nil, "st", Node{Name: "g1", Labels: map[string]string{"gpu": "B300"}}); d.Rule != 3 || !d.Allow {
		t.Fatalf("wyc on a B300 of st: %+v", d)
	}
	if d := Decide(p, "bob", nil, "st", Node{Name: "g1"}); d.Rule != -1 || !d.Allow || !d.Privileged {
		t.Fatalf("no rule for st: the default: %+v", d)
	}
}

func TestMachineMatches(t *testing.T) {
	b300 := Node{Name: "g206", Labels: map[string]string{"gpu": "B300"}}
	plain := Node{Name: "c500-16-23"}
	for _, c := range []struct {
		sel  []string
		n    Node
		want bool
	}{
		{nil, plain, true},
		{[]string{"c500-*"}, plain, true},
		{[]string{"c500-8-*"}, plain, false},
		{[]string{"gpu=B300"}, b300, true},
		{[]string{"gpu=B300"}, plain, false}, // no such label: no match, no error
		{[]string{"a", "gpu=B300"}, b300, true},
		{[]string{"g206"}, b300, true},
	} {
		if got := MachineMatches(c.sel, c.n); got != c.want {
			t.Errorf("MachineMatches(%v, %s) = %v", c.sel, c.n.Name, got)
		}
	}
}

func TestUnrestricted(t *testing.T) {
	everyone := v1.AccessSubject{Kind: "everyone"}
	allow := v1.AccessEffect{Allow: true}
	open := v1.DefaultAccessEffect()
	priv := Needs{Privileged: true}
	none := Needs{}
	cases := []struct {
		name  string
		p     v1.AccessPolicySpec
		needs Needs
		want  bool
	}{
		{"no rules, default open", v1.AccessPolicySpec{Default: open}, priv, true},
		{"default deny", v1.AccessPolicySpec{Default: v1.AccessEffect{}}, none, false},
		{"a deny for some machines, even none there yet", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Machines: []string{"c500-*"}, Subject: everyone}}, Default: open}, none, false},
		{"a deny for other users", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: v1.AccessSubject{Kind: "some", Users: []string{"bob", "carol"}}}}, Default: open}, none, true},
		{"a deny for a group not mine", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: v1.AccessSubject{Kind: "some", Groups: []string{"ops"}}}}, Default: open}, none, true},
		{"a deny for my group among others", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: v1.AccessSubject{Kind: "some", Users: []string{"bob"}, Groups: []string{"ops", "lab"}}}}, Default: open}, none, false},
		{"a deny in another cluster", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "yc", Subject: everyone}}, Default: open}, none, true},
		{"an allow of every machine decides before a deny", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: everyone, Effect: open}, {Machines: []string{"m1"}, Subject: everyone}}, Default: open}, none, true},
		{"allowed without privileges, wanting them", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Machines: []string{"m*"}, Subject: everyone, Effect: allow}}, Default: open}, priv, false},
		{"allowed without privileges, not wanting them", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Machines: []string{"m*"}, Subject: everyone, Effect: allow}}, Default: open}, none, true},
		{"needs a reservation somewhere", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Machines: []string{"g1"}, Subject: everyone, Effect: v1.AccessEffect{Allow: true, NeedsReservation: true}}}, Default: open}, none, false},
	}
	for _, c := range cases {
		if got := Unrestricted(&c.p, "alice", map[string]bool{"lab": true}, "zjusct", c.needs); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// A rule names any number of users and groups: it is about each of them.
func TestSubjectSome(t *testing.T) {
	p := &v1.AccessPolicySpec{
		Rules:   []v1.AccessRule{{Subject: v1.AccessSubject{Kind: "some", Users: []string{"alice", "bob"}, Groups: []string{"lab", "ops"}}}},
		Default: v1.DefaultAccessEffect(),
	}
	n := Node{Name: "g1"}
	for user, groups := range map[string]map[string]bool{"alice": nil, "bob": nil, "carol": {"ops": true}, "dave": {"lab": true, "x": true}} {
		if d := Decide(p, user, groups, "st", n); d.Rule != 0 || d.Allow {
			t.Errorf("%s: %+v", user, d)
		}
	}
	if d := Decide(p, "eve", map[string]bool{"x": true}, "st", n); d.Rule != -1 {
		t.Errorf("eve, in neither: %+v", d)
	}
}

// Whether a cluster is shown to a user: some machine of it, there now or
// to come, the rules let them use.
func TestClusterOpen(t *testing.T) {
	everyone := v1.AccessSubject{Kind: "everyone"}
	deny, allow := v1.AccessEffect{}, v1.AccessEffect{Allow: true}
	open := v1.DefaultAccessEffect()
	cases := []struct {
		name string
		p    v1.AccessPolicySpec
		want bool
	}{
		{"no rules, default open", v1.AccessPolicySpec{Default: open}, true},
		{"default deny", v1.AccessPolicySpec{Default: deny}, false},
		{"every machine of it denied", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "yc", Subject: everyone, Effect: deny}}, Default: open}, false},
		{"some machines of it denied", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "yc", Machines: []string{"c500-*"}, Subject: everyone, Effect: deny}}, Default: open}, true},
		{"another cluster denied", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "st", Subject: everyone, Effect: deny}}, Default: open}, true},
		{"denied to others", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "yc", Subject: v1.AccessSubject{Kind: "some", Users: []string{"bob"}}, Effect: deny}}, Default: open}, true},
		{"some machines allowed before all denied", v1.AccessPolicySpec{Rules: []v1.AccessRule{
			{Cluster: "yc", Machines: []string{"c500-*"}, Subject: v1.AccessSubject{Kind: "some", Groups: []string{"lab"}}, Effect: allow},
			{Cluster: "yc", Subject: everyone, Effect: deny},
		}, Default: open}, true},
		{"allowed only under a reservation", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "yc", Subject: everyone, Effect: v1.AccessEffect{Allow: true, NeedsReservation: true}}}, Default: deny}, true},
		{"denied everywhere, opened elsewhere only", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "st", Subject: everyone, Effect: allow}}, Default: deny}, false},
	}
	for _, c := range cases {
		if got := ClusterOpen(&c.p, "alice", map[string]bool{"lab": true}, "yc"); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
