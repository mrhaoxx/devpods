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
			{Cluster: "yc", Machines: []string{"c500-8-65-*"}, Subject: v1.AccessSubject{Kind: "group", Name: "infer"}, Effect: allow(true, false)},
			{Cluster: "yc", Machines: []string{"c500-8-65-*"}, Subject: v1.AccessSubject{Kind: "everyone"}, Effect: v1.AccessEffect{}},
			{Cluster: "yc", Subject: v1.AccessSubject{Kind: "everyone"}, Effect: allow(false, true)},
			{Machines: []string{"gpu=B300"}, Subject: v1.AccessSubject{Kind: "user", Name: "wyc"}, Effect: allow(true, false)},
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
