// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package access evaluates the console's access rules (AccessPolicy): who
// may use which machines of which clusters, with what — privileged
// containers, host directories — and whether only under a reservation.
package access

import (
	"path"
	"strings"

	v1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Node is a machine as the rules see it.
type Node struct {
	Name   string
	Labels map[string]string
}

// Decision is what applies to a user on a machine, and which rule said so.
type Decision struct {
	v1.AccessEffect
	Rule int // the index in Rules; -1 = the default
}

// MatchOf: a rule's condition — Match, or, for a rule in the old form,
// the condition its cluster, machines and subject make. A user of the old
// form was a name, not a glob: its glob characters are escaped.
func MatchOf(r v1.AccessRule) string {
	if r.Match != "" {
		return strings.TrimSpace(r.Match)
	}
	list := func(items []string, each func(string) string) string {
		q := make([]string, len(items))
		for i, it := range items {
			q[i] = QuoteItem(each(it))
		}
		return "(" + strings.Join(q, ", ") + ")"
	}
	same := func(s string) string { return s }
	var parts []string
	if r.Cluster != "" {
		parts = append(parts, "cluster = "+QuoteItem(r.Cluster))
	}
	if len(r.Machines) > 0 {
		parts = append(parts, "machine in "+list(r.Machines, same))
	}
	if s := r.Subject; s != nil && s.Kind != "everyone" {
		var who []string
		if len(s.Users) > 0 {
			who = append(who, "user in "+list(s.Users, escapeGlob))
		}
		if len(s.Groups) > 0 {
			who = append(who, "group in "+list(s.Groups, same))
		}
		switch len(who) {
		case 0:
			parts = append(parts, "not everyone") // "some" naming nobody: nobody
		case 1:
			parts = append(parts, who[0])
		default:
			parts = append(parts, "("+strings.Join(who, " or ")+")")
		}
	}
	return strings.Join(parts, " and ")
}

func escapeGlob(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(s)
}

// Problem is a rule that denies everyone everything: its condition cannot
// be read, or it names a group (not a glob) that is not there — a group
// gone could widen a rule that says "not" it (design 2026-11-19 §5.5).
type Problem struct {
	Rule    int    `json:"rule"` // from 0
	Message string `json:"message"`
}

// Policy is the rules compiled: their conditions read, their effects in
// the new form.
type Policy struct {
	rules   []compiled
	Default v1.AccessEffect
}

type compiled struct {
	expr   *Expr
	effect v1.AccessEffect
	broken string // why it denies everyone everything
}

// Compile reads the rules; groups are every UserGroup there is.
func Compile(spec *v1.AccessPolicySpec, groups map[string]bool) *Policy {
	p := &Policy{Default: NormalizeEffect(spec.Default)}
	for _, r := range spec.Rules {
		c := compiled{effect: NormalizeEffect(r.Effect)}
		e, err := Parse(MatchOf(r))
		if err != nil {
			c.broken = err.Error()
		} else {
			c.expr = e
			c.broken = itemsProblem(e)
			for _, g := range e.Items(FieldGroup) {
				if c.broken == "" && !HasGlob(g) && !groups[g] {
					c.broken = "点名了不存在的组 " + g
				}
			}
		}
		p.rules = append(p.rules, c)
	}
	return p
}

// ItemProblem: why an item of f can match nothing — blank, a malformed
// glob, a machine item neither a name nor key=value — or "". Under not,
// such an item would widen a rule.
func ItemProblem(f Field, item string) string {
	switch {
	case strings.TrimSpace(item) == "":
		return f.String() + " 有一项是空的"
	case f == FieldMachine && !ValidMachine(item):
		return item + " 既不是机器名（可用通配）也不是标签 key=value"
	case f != FieldMachine:
		if _, err := path.Match(item, ""); err != nil {
			return item + " 不是合法的名字或通配"
		}
	}
	return ""
}

func itemsProblem(e *Expr) string {
	for _, f := range []Field{FieldCluster, FieldMachine, FieldUser, FieldGroup} {
		for _, it := range e.Items(f) {
			if p := ItemProblem(f, it); p != "" {
				return p
			}
		}
	}
	return ""
}

// Problems: the rules that deny everyone everything, and why.
func (p *Policy) Problems() []Problem {
	var out []Problem
	for i, c := range p.rules {
		if c.broken != "" {
			out = append(out, Problem{Rule: i, Message: c.broken})
		}
	}
	return out
}

// on: the rule's condition on f (a broken rule holds for everyone) and its
// effect (a broken rule's: deny).
func (c *compiled) on(f Facts) (Tri, v1.AccessEffect) {
	if c.broken != "" {
		return True, v1.AccessEffect{}
	}
	return c.expr.Eval(f), c.effect
}

func facts(user string, groups map[string]bool, cluster string, n *Node) Facts {
	return Facts{Cluster: &cluster, Node: n, User: &user, Groups: groups}
}

// Decide: the first rule whose condition holds decides; the default when
// none does. groups are the user's.
func (p *Policy) Decide(user string, groups map[string]bool, cluster string, n Node) Decision {
	f := facts(user, groups, cluster, &n)
	for i := range p.rules {
		if v, e := p.rules[i].on(f); v == True {
			return Decision{AccessEffect: e, Rule: i}
		}
	}
	return Decision{AccessEffect: p.Default, Rule: -1}
}

// Unrestricted: would every machine of cluster — those there now and any
// that join later — take a DevPod of user's with needs, no reservation
// asked? Judged with the machine not known: a rule that may hold for
// user in cluster (true or unknown) and would not take it says no, unless
// an earlier rule holding for every machine (true) has already decided.
// The console leaves an unrestricted DevPod without its allow-list.
func (p *Policy) Unrestricted(user string, groups map[string]bool, cluster string, needs Needs) bool {
	takes := func(e v1.AccessEffect) bool {
		return e.Allow && !e.NeedsReservation && (Decision{AccessEffect: e}).Admits(needs) == nil
	}
	f := facts(user, groups, cluster, nil)
	for i := range p.rules {
		v, e := p.rules[i].on(f)
		if v == False {
			continue
		}
		if !takes(e) {
			return false
		}
		if v == True {
			return true // every machine is decided here
		}
	}
	return takes(p.Default)
}

// ClusterOpen: may user use some machine of cluster — one there now or
// one that joins later — by the rules alone? The first rule that may hold
// for user there and allows (a reservation asked or not) says yes; a deny
// holding for every machine of it says no, as no rule after it is reached
// for any machine; with neither, the default says.
func (p *Policy) ClusterOpen(user string, groups map[string]bool, cluster string) bool {
	f := facts(user, groups, cluster, nil)
	for i := range p.rules {
		v, e := p.rules[i].on(f)
		switch {
		case v == False:
		case e.Allow:
			return true
		case v == True:
			return false
		}
	}
	return p.Default.Allow
}

// MachineMatches: no selector matches every machine; else any item does —
// "key=value" a label the node has with that value, otherwise a name glob.
func MachineMatches(sel []string, n Node) bool {
	if len(sel) == 0 {
		return true
	}
	for _, m := range sel {
		if k, v, ok := strings.Cut(m, "="); ok {
			if got, has := n.Labels[k]; has && got == v {
				return true
			}
			continue
		}
		if ok, err := path.Match(m, n.Name); err == nil && ok {
			return true
		}
	}
	return false
}

// ValidMachine reports whether m can be a machine selector item.
func ValidMachine(m string) bool {
	if m == "" {
		return false
	}
	if k, _, ok := strings.Cut(m, "="); ok {
		return k != ""
	}
	_, err := path.Match(m, "")
	return err == nil
}
