// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"reflect"
	"strings"
	"testing"

	v1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// known: the groups that exist, every one these tests name.
var known = map[string]bool{"infer": true, "ops": true, "lab": true, "x": true}

func allow(privileged, reservation bool) v1.AccessEffect {
	return v1.AccessEffect{Allow: true, Privileged: privileged, NeedsReservation: reservation}
}

// The first rule whose resource and subject match decides; else the default.
func TestDecideOrder(t *testing.T) {
	p := &v1.AccessPolicySpec{
		Rules: []v1.AccessRule{
			{Cluster: "yc", Machines: []string{"c500-8-65-*"}, Subject: &v1.AccessSubject{Kind: "some", Groups: []string{"infer"}}, Effect: allow(true, false)},
			{Cluster: "yc", Machines: []string{"c500-8-65-*"}, Subject: &v1.AccessSubject{Kind: "everyone"}, Effect: v1.AccessEffect{}},
			{Cluster: "yc", Subject: &v1.AccessSubject{Kind: "everyone"}, Effect: allow(false, true)},
			{Machines: []string{"gpu=B300"}, Subject: &v1.AccessSubject{Kind: "some", Users: []string{"wyc"}}, Effect: allow(true, false)},
		},
		Default: v1.DefaultAccessEffect(),
	}
	inInfer := map[string]bool{"infer": true}
	n33 := Node{Name: "c500-8-65-33"}
	if d := Compile(p, known).Decide("wyc", inInfer, "yc", n33); d.Rule != 0 || !d.Allow || !Grants(d.AccessEffect).Privileged {
		t.Fatalf("group member on c500-8-65-33: %+v", d)
	}
	if d := Compile(p, known).Decide("bob", nil, "yc", n33); d.Rule != 1 || d.Allow {
		t.Fatalf("others on c500-8-65-33: %+v", d)
	}
	if d := Compile(p, known).Decide("bob", nil, "yc", Node{Name: "c500-16-23"}); d.Rule != 2 || !d.Allow || !d.NeedsReservation || Grants(d.AccessEffect).Privileged {
		t.Fatalf("others on c500-16-23: %+v", d)
	}
	if d := Compile(p, known).Decide("wyc", nil, "st", Node{Name: "g1", Labels: map[string]string{"gpu": "B300"}}); d.Rule != 3 || !d.Allow {
		t.Fatalf("wyc on a B300 of st: %+v", d)
	}
	if d := Compile(p, known).Decide("bob", nil, "st", Node{Name: "g1"}); d.Rule != -1 || !d.Allow || !Grants(d.AccessEffect).Privileged {
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
	everyone := &v1.AccessSubject{Kind: "everyone"}
	allow := v1.AccessEffect{Allow: true}
	open := v1.DefaultAccessEffect()
	priv := Needs{PrivilegedContainer: true}
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
		{"a deny for other users", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: &v1.AccessSubject{Kind: "some", Users: []string{"bob", "carol"}}}}, Default: open}, none, true},
		{"a deny for a group not mine", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: &v1.AccessSubject{Kind: "some", Groups: []string{"ops"}}}}, Default: open}, none, true},
		{"a deny for my group among others", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: &v1.AccessSubject{Kind: "some", Users: []string{"bob"}, Groups: []string{"ops", "lab"}}}}, Default: open}, none, false},
		{"a deny in another cluster", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "yc", Subject: everyone}}, Default: open}, none, true},
		{"an allow of every machine decides before a deny", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: everyone, Effect: open}, {Machines: []string{"m1"}, Subject: everyone}}, Default: open}, none, true},
		{"allowed without privileges, wanting them", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Machines: []string{"m*"}, Subject: everyone, Effect: allow}}, Default: open}, priv, false},
		{"allowed without privileges, not wanting them", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Machines: []string{"m*"}, Subject: everyone, Effect: allow}}, Default: open}, none, true},
		{"needs a reservation somewhere", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Machines: []string{"g1"}, Subject: everyone, Effect: v1.AccessEffect{Allow: true, NeedsReservation: true}}}, Default: open}, none, false},
	}
	for _, c := range cases {
		if got := Compile(&c.p, known).Unrestricted("alice", map[string]bool{"lab": true}, "zjusct", c.needs); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// A rule names any number of users and groups: it is about each of them.
func TestSubjectSome(t *testing.T) {
	p := &v1.AccessPolicySpec{
		Rules:   []v1.AccessRule{{Subject: &v1.AccessSubject{Kind: "some", Users: []string{"alice", "bob"}, Groups: []string{"lab", "ops"}}}},
		Default: v1.DefaultAccessEffect(),
	}
	n := Node{Name: "g1"}
	for user, groups := range map[string]map[string]bool{"alice": nil, "bob": nil, "carol": {"ops": true}, "dave": {"lab": true, "x": true}} {
		if d := Compile(p, known).Decide(user, groups, "st", n); d.Rule != 0 || d.Allow {
			t.Errorf("%s: %+v", user, d)
		}
	}
	if d := Compile(p, known).Decide("eve", map[string]bool{"x": true}, "st", n); d.Rule != -1 {
		t.Errorf("eve, in neither: %+v", d)
	}
}

// Whether a cluster is shown to a user: some machine of it, there now or
// to come, the rules let them use.
func TestClusterOpen(t *testing.T) {
	everyone := &v1.AccessSubject{Kind: "everyone"}
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
		{"denied to others", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "yc", Subject: &v1.AccessSubject{Kind: "some", Users: []string{"bob"}}, Effect: deny}}, Default: open}, true},
		{"some machines allowed before all denied", v1.AccessPolicySpec{Rules: []v1.AccessRule{
			{Cluster: "yc", Machines: []string{"c500-*"}, Subject: &v1.AccessSubject{Kind: "some", Groups: []string{"lab"}}, Effect: allow},
			{Cluster: "yc", Subject: everyone, Effect: deny},
		}, Default: open}, true},
		{"allowed only under a reservation", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "yc", Subject: everyone, Effect: v1.AccessEffect{Allow: true, NeedsReservation: true}}}, Default: deny}, true},
		{"denied everywhere, opened elsewhere only", v1.AccessPolicySpec{Rules: []v1.AccessRule{{Cluster: "st", Subject: everyone, Effect: allow}}, Default: deny}, false},
	}
	for _, c := range cases {
		if got := Compile(&c.p, known).ClusterOpen("alice", map[string]bool{"lab": true}, "yc"); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestMatchOf(t *testing.T) {
	some := func(users, groups []string) *v1.AccessSubject {
		return &v1.AccessSubject{Kind: "some", Users: users, Groups: groups}
	}
	for want, r := range map[string]v1.AccessRule{
		"cluster = yc and machine in (c500-*, gpu=B300) and (user in (alice) or group in (ops))": {Cluster: "yc", Machines: []string{"c500-*", "gpu=B300"}, Subject: some([]string{"alice"}, []string{"ops"})},
		"":                              {Subject: &v1.AccessSubject{Kind: "everyone"}},
		"user in (a, b)":                {Subject: some([]string{"a", "b"}, nil)},
		"cluster = st and group in (g)": {Cluster: "st", Subject: some(nil, []string{"g"})},
		"machine in (m1)":               {Machines: []string{"m1"}},
		"cluster = st":                  {Match: "  cluster = st  ", Cluster: "yc"},
		`user in ("a\\*b")`:             {Subject: some([]string{"a*b"}, nil)},
	} {
		if got := MatchOf(r); got != want {
			t.Errorf("MatchOf(%+v) = %s, want %s", r, got, want)
		}
	}
	// a user named with a glob character is that user only, as before
	p := Compile(&v1.AccessPolicySpec{Rules: []v1.AccessRule{{Subject: some([]string{"a*b"}, nil)}}, Default: v1.DefaultAccessEffect()}, known)
	if d := p.Decide("a*b", nil, "st", Node{Name: "g1"}); d.Rule != 0 {
		t.Errorf("a*b: %+v", d)
	}
	if d := p.Decide("axb", nil, "st", Node{Name: "g1"}); d.Rule != -1 {
		t.Errorf("axb: %+v", d)
	}
}

// The old form's "some" naming nobody matched nobody: so does its condition.
func TestMatchOfEmptySome(t *testing.T) {
	r := v1.AccessRule{Cluster: "yc", Subject: &v1.AccessSubject{Kind: "some"}}
	if got := MatchOf(r); got != "cluster = yc and not everyone" {
		t.Fatalf("MatchOf: %s", got)
	}
	p := Compile(&v1.AccessPolicySpec{Rules: []v1.AccessRule{r}, Default: v1.DefaultAccessEffect()}, known)
	if d := p.Decide("alice", known, "yc", Node{Name: "g1"}); d.Rule != -1 {
		t.Errorf("matched: %+v", d)
	}
}

// b300's rules as they are (2026-11-19), in the old form and as
// conditions: the same answers everywhere.
func TestPolicyB300(t *testing.T) {
	everyone := &v1.AccessSubject{Kind: "everyone"}
	resv := v1.AccessEffect{Allow: true, Privileged: true, HostPaths: []v1.HostPathGrant{{Path: "/", Writable: true}}, NeedsReservation: true}
	old := v1.AccessPolicySpec{Rules: []v1.AccessRule{
		{Cluster: "meihua", Subject: everyone, Effect: v1.AccessEffect{Allow: true}},
		{Cluster: "st2", Subject: everyone, Effect: resv},
		{Cluster: "a5", Subject: everyone, Effect: resv},
		{Cluster: "a5", Subject: everyone},
		{Cluster: "st2", Subject: everyone},
		{Cluster: "shanhu", Subject: everyone},
	}, Default: v1.AccessEffect{Allow: true, Privileged: true, HostPaths: []v1.HostPathGrant{{Path: "/", Writable: true}}}}
	conv := v1.AccessPolicySpec{Default: NormalizeEffect(old.Default)}
	wantMatch := []string{"cluster = meihua", "cluster = st2", "cluster = a5", "cluster = a5", "cluster = st2", "cluster = shanhu"}
	for i, r := range old.Rules {
		if m := MatchOf(r); m != wantMatch[i] {
			t.Errorf("rule %d: %s", i, m)
		}
		conv.Rules = append(conv.Rules, v1.AccessRule{Match: MatchOf(r), Effect: NormalizeEffect(r.Effect)})
	}
	po, pc := Compile(&old, known), Compile(&conv, known)
	g1 := Node{Name: "g1"}
	for user, groups := range map[string]map[string]bool{"alice": nil, "bob": {"ops": true}} {
		for _, c := range []string{"meihua", "st2", "a5", "shanhu", "st", "yc"} {
			if a, b := po.Decide(user, groups, c, g1), pc.Decide(user, groups, c, g1); !reflect.DeepEqual(a, b) {
				t.Errorf("Decide %s %s: %+v vs %+v", user, c, a, b)
			}
			if a, b := po.ClusterOpen(user, groups, c), pc.ClusterOpen(user, groups, c); a != b {
				t.Errorf("ClusterOpen %s %s: %v vs %v", user, c, a, b)
			}
			for _, n := range []Needs{{}, {HostPID: true}} {
				if a, b := po.Unrestricted(user, groups, c, n), pc.Unrestricted(user, groups, c, n); a != b {
					t.Errorf("Unrestricted %s %s %+v: %v vs %v", user, c, n, a, b)
				}
			}
		}
	}
	if d := pc.Decide("alice", nil, "st2", g1); d.Rule != 1 || !d.NeedsReservation || !Grants(d.AccessEffect).HostNetwork {
		t.Errorf("st2: %+v", d)
	}
}

// "st but st5": a part of st — machines to come are not open until placed.
func TestPolicyExcept(t *testing.T) {
	p := Compile(&v1.AccessPolicySpec{Rules: []v1.AccessRule{{Match: "cluster = st and machine != st5", Effect: v1.AccessEffect{Allow: true}}}, Default: v1.AccessEffect{}}, known)
	if d := p.Decide("alice", nil, "st", Node{Name: "st5"}); d.Allow {
		t.Errorf("st5: %+v", d)
	}
	if d := p.Decide("alice", nil, "st", Node{Name: "st6"}); !d.Allow || d.Rule != 0 {
		t.Errorf("st6: %+v", d)
	}
	if p.Unrestricted("alice", nil, "st", Needs{}) {
		t.Error("unrestricted in st: it needs an allow-list")
	}
	if !p.ClusterOpen("alice", nil, "st") || p.ClusterOpen("alice", nil, "yc") {
		t.Error("ClusterOpen")
	}
}

// A rule about people alone holds in every cluster, on every machine.
func TestPolicyUserOnlyRule(t *testing.T) {
	p := Compile(&v1.AccessPolicySpec{Rules: []v1.AccessRule{{Match: "group = ops"}}, Default: v1.DefaultAccessEffect()}, known)
	ops := map[string]bool{"ops": true}
	for _, c := range []string{"st", "yc"} {
		if p.ClusterOpen("bob", ops, c) || p.Unrestricted("bob", ops, c, Needs{}) {
			t.Errorf("bob in ops, %s: open", c)
		}
		if !p.ClusterOpen("alice", nil, c) || !p.Unrestricted("alice", nil, c, Needs{}) {
			t.Errorf("alice, %s: closed", c)
		}
	}
}

// A rule that cannot be read, or names a group that is not there, denies
// everyone everything from where it stands; a glob of groups and a
// cluster that is not there are taken as written.
func TestPolicyBroken(t *testing.T) {
	open := v1.DefaultAccessEffect()
	for bad, says := range map[string]string{"cluster in (": "第 13 个字符", "group = gone": "点名了不存在的组 gone", "everyone and group != interns[": "interns[", `machine = "["`: "["} {
		p := Compile(&v1.AccessPolicySpec{Rules: []v1.AccessRule{
			{Match: "user = vip", Effect: open},
			{Match: bad, Effect: open},
			{Effect: open},
		}, Default: open}, known)
		if pr := p.Problems(); len(pr) != 1 || pr[0].Rule != 1 || !strings.Contains(pr[0].Message, says) {
			t.Errorf("%s: problems %+v", bad, pr)
		}
		if d := p.Decide("alice", nil, "st", Node{Name: "g1"}); d.Rule != 1 || d.Allow {
			t.Errorf("%s: alice %+v", bad, d)
		}
		if d := p.Decide("vip", nil, "st", Node{Name: "g1"}); d.Rule != 0 || !d.Allow {
			t.Errorf("%s: vip %+v", bad, d)
		}
		if p.ClusterOpen("alice", nil, "st") || p.Unrestricted("alice", nil, "st", Needs{}) {
			t.Errorf("%s: open to alice", bad)
		}
	}
	p := Compile(&v1.AccessPolicySpec{Rules: []v1.AccessRule{{Match: "group = gone*"}, {Match: "cluster = gone"}}, Default: open}, known)
	if pr := p.Problems(); len(pr) != 0 {
		t.Errorf("as written: %+v", pr)
	}
	if d := p.Decide("alice", nil, "st", Node{Name: "g1"}); d.Rule != -1 || !d.Allow {
		t.Errorf("as written: %+v", d)
	}
}
