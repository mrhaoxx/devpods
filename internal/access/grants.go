// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"slices"
	"strings"

	v1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Management grants (design 2026-11-20): AccessGrants give the people a
// condition holds for some permissions on the machines it holds for.
// They add up — any one that holds gives — and none takes away. A grant
// that cannot be taken as written (unreadable, an item that can match
// nothing, a group not there, no condition) gives nothing.

// GrantProblem: a grant that gives nothing, and why.
type GrantProblem struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// GrantSet is the AccessGrants compiled.
type GrantSet struct {
	list []compiledGrant
}

type compiledGrant struct {
	name   string
	expr   *Expr
	perms  []v1.GrantPermission
	broken string
}

// CompileGrants reads the grants; groups are every UserGroup there is.
func CompileGrants(gs []v1.AccessGrant, groups map[string]bool) *GrantSet {
	out := &GrantSet{}
	for i := range gs {
		c := compiledGrant{name: gs[i].Name, perms: gs[i].Spec.Permissions}
		m := strings.TrimSpace(gs[i].Spec.Match)
		if m == "" {
			c.broken = "条件是空的"
		} else if e, err := Parse(m); err != nil {
			c.broken = err.Error()
		} else {
			c.expr, c.broken = e, unsound(e, groups)
		}
		out.list = append(out.list, c)
	}
	return out
}

// Problems: the grants that give nothing, and why.
func (g *GrantSet) Problems() []GrantProblem {
	var out []GrantProblem
	for _, c := range g.list {
		if c.broken != "" {
			out = append(out, GrantProblem{Name: c.name, Message: c.broken})
		}
	}
	return out
}

// Has: some grant giving p holds for user (with groups) on node of
// cluster; node nil = the machine is not known, and then only a grant
// that holds whatever the machine counts.
func (g *GrantSet) Has(p v1.GrantPermission, user string, groups map[string]bool, cluster string, node *Node) bool {
	return g.any(p, Facts{Cluster: &cluster, Node: node, User: &user, Groups: groups}, func(t Tri) bool { return t == True })
}

// Possible: some grant giving p may hold for user somewhere — what a
// page shows; each action is judged on its own (Has).
func (g *GrantSet) Possible(p v1.GrantPermission, user string, groups map[string]bool) bool {
	return g.any(p, Facts{User: &user, Groups: groups}, func(t Tri) bool { return t != False })
}

func (g *GrantSet) any(p v1.GrantPermission, f Facts, ok func(Tri) bool) bool {
	for _, c := range g.list {
		if c.broken == "" && slices.Contains(c.perms, p) && ok(c.expr.Eval(f)) {
			return true
		}
	}
	return false
}
