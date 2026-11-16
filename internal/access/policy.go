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

// Decide: the first rule whose cluster ("" = any), machines and subject
// match decides; the default when none does. groups are the user's.
func Decide(p *v1.AccessPolicySpec, user string, groups map[string]bool, cluster string, n Node) Decision {
	for i := range p.Rules {
		r := &p.Rules[i]
		if (r.Cluster == "" || r.Cluster == cluster) && MachineMatches(r.Machines, n) && subjectMatches(r.Subject, user, groups) {
			return Decision{AccessEffect: r.Effect, Rule: i}
		}
	}
	return Decision{AccessEffect: p.Default, Rule: -1}
}

func subjectMatches(s v1.AccessSubject, user string, groups map[string]bool) bool {
	switch s.Kind {
	case "everyone":
		return true
	case "user":
		return s.Name == user
	case "group":
		return groups[s.Name]
	}
	return false
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
