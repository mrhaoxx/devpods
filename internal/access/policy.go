// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package access evaluates the console's access rules (AccessPolicy): who
// may use which machines of which clusters, with what — privileged
// containers, host directories — and whether only under a reservation.
package access

import (
	"path"
	"slices"
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

// Unrestricted: would every machine of cluster — those there now and any
// that join later — take a DevPod of user's with needs, no reservation
// asked? Judged from the rules alone, since machines to come cannot be
// listed: any rule for user in cluster that would not take it says no,
// unless an earlier rule for every machine has already decided. The
// console leaves an unrestricted DevPod without its allow-list.
func Unrestricted(p *v1.AccessPolicySpec, user string, groups map[string]bool, cluster string, needs Needs) bool {
	takes := func(e v1.AccessEffect) bool {
		return e.Allow && !e.NeedsReservation && (Decision{AccessEffect: e}).Admits(needs) == nil
	}
	for i := range p.Rules {
		r := &p.Rules[i]
		if (r.Cluster != "" && r.Cluster != cluster) || !subjectMatches(r.Subject, user, groups) {
			continue
		}
		if !takes(r.Effect) {
			return false
		}
		if len(r.Machines) == 0 {
			return true // every machine is decided here
		}
	}
	return takes(p.Default)
}

// ClusterOpen: may user use some machine of cluster — one there now or
// one that joins later — by the rules alone? The first rule for user
// there that allows (a reservation asked or not) says yes; one that
// denies every machine of it says no, as no rule after it is reached for
// any machine; with neither, the default says.
func ClusterOpen(p *v1.AccessPolicySpec, user string, groups map[string]bool, cluster string) bool {
	for i := range p.Rules {
		r := &p.Rules[i]
		if (r.Cluster != "" && r.Cluster != cluster) || !subjectMatches(r.Subject, user, groups) {
			continue
		}
		if r.Effect.Allow {
			return true
		}
		if len(r.Machines) == 0 {
			return false
		}
	}
	return p.Default.Allow
}

func subjectMatches(s *v1.AccessSubject, user string, groups map[string]bool) bool {
	if s == nil || s.Kind == "everyone" {
		return true
	}
	if slices.Contains(s.Users, user) {
		return true
	}
	for _, g := range s.Groups {
		if groups[g] {
			return true
		}
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
