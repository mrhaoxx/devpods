// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// The access rules and user groups are the console's (its own cluster).
// Decisions read them through the cache; the admin API reads them afresh.

// accessPolicy: the rules and their resourceVersion; with none kept yet,
// the default rule alone (everything allowed) and "".
func (s *Server) accessPolicy(ctx context.Context) (*devpodv1alpha1.AccessPolicySpec, string, error) {
	var p devpodv1alpha1.AccessPolicy
	err := s.Client.Get(ctx, types.NamespacedName{Name: devpodv1alpha1.AccessPolicyName}, &p)
	if apierrors.IsNotFound(err) {
		return &devpodv1alpha1.AccessPolicySpec{Default: devpodv1alpha1.DefaultAccessEffect()}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return &p.Spec, p.ResourceVersion, nil
}

// accessFor: the rules compiled against the groups there are (a rule
// naming one that is not denies everyone), and the groups user is in.
func (s *Server) accessFor(ctx context.Context, user string) (*access.Policy, map[string]bool, error) {
	spec, rv, err := s.accessPolicy(ctx)
	if err != nil {
		return nil, nil, err
	}
	var list devpodv1alpha1.UserGroupList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, nil, err
	}
	all, mine := map[string]bool{}, map[string]bool{}
	for i := range list.Items {
		g := &list.Items[i]
		all[g.Name] = true
		if slices.Contains(g.Spec.Members, user) {
			mine[g.Name] = true
		}
	}
	p := access.Compile(spec, all)
	for _, pr := range p.Problems() {
		// once per version of the rules (design §5.5), not on every request
		if _, seen := s.accessLogged.LoadOrStore(rv+"/"+strconv.Itoa(pr.Rule)+"/"+pr.Message, true); !seen {
			slog.Error("access_rule_denies_everyone", "rule", pr.Rule+1, "why", pr.Message, "resourceVersion", rv)
		}
	}
	return p, mine, nil
}

// grantsFor: the management grants compiled against the groups there
// are (one naming a group that is not gives nothing), and the groups user
// is in. A grant that gives nothing is logged once per version of it.
func (s *Server) grantsFor(ctx context.Context, user string) (*access.GrantSet, map[string]bool, error) {
	// bounded: a cache that cannot sync them (the CRD or the console's
	// RBAC missing) must not hold a request up
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var gl devpodv1alpha1.AccessGrantList
	if err := s.Client.List(lctx, &gl); err != nil {
		return nil, nil, err
	}
	var list devpodv1alpha1.UserGroupList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, nil, err
	}
	all, mine := map[string]bool{}, map[string]bool{}
	for i := range list.Items {
		g := &list.Items[i]
		all[g.Name] = true
		if slices.Contains(g.Spec.Members, user) {
			mine[g.Name] = true
		}
	}
	gs := access.CompileGrants(gl.Items, all)
	rv := map[string]string{}
	for i := range gl.Items {
		rv[gl.Items[i].Name] = gl.Items[i].ResourceVersion
	}
	for _, pr := range gs.Problems() {
		if _, seen := s.accessLogged.LoadOrStore("grant/"+pr.Name+"/"+rv[pr.Name]+"/"+pr.Message, true); !seen {
			slog.Error("access_grant_grants_nothing", "grant", pr.Name, "why", pr.Message, "resourceVersion", rv[pr.Name])
		}
	}
	return gs, mine, nil
}

// groupsOf: the groups user is in.
func (s *Server) groupsOf(ctx context.Context, user string) (map[string]bool, error) {
	var list devpodv1alpha1.UserGroupList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	in := map[string]bool{}
	for i := range list.Items {
		if slices.Contains(list.Items[i].Spec.Members, user) {
			in[list.Items[i].Name] = true
		}
	}
	return in, nil
}

// ownerIsAdmin: the rules do not bind admins.
// clusterGate says which clusters the session's user is shown: those the
// rules give them some machine of (access.ClusterOpen); every one to an
// admin. Read once per request. It hides, it does not guard: where a
// DevPod may run is judged where one comes to run, and a user still
// reaches the DevPods they have in a cluster no longer shown.
func (s *Server) clusterGate(ctx context.Context, sess Session) func(cluster string) bool {
	all := func(string) bool { return true }
	if sess.Admin || s.ownerIsAdmin(ctx, sess.User) {
		return all
	}
	p, groups, err := s.accessFor(ctx, sess.User)
	if err != nil {
		slog.Warn("cluster_gate", "user", sess.User, "err", err)
		return all
	}
	return func(cluster string) bool { return p.ClusterOpen(sess.User, groups, cluster) }
}

func (s *Server) ownerIsAdmin(ctx context.Context, owner string) bool {
	var u devpodv1alpha1.User
	if err := s.Client.Get(ctx, types.NamespacedName{Name: owner}, &u); err != nil {
		return s.adminFor(owner, nil)
	}
	return s.adminFor(owner, &u)
}

// decide is what applies to owner on n in cluster; true for an admin, to
// whom everything is allowed.
func (s *Server) decide(ctx context.Context, owner, cluster string, n access.Node) (access.Decision, bool, error) {
	oa, err := s.ownerAccess(ctx, owner)
	if err != nil {
		return access.Decision{}, false, err
	}
	d, admin := oa.decide(cluster, n)
	return d, admin, nil
}

// ownerAccessOf: one owner's way through the access rules, read once —
// for judging many machines (the queue: every job's every machine).
type ownerAccessOf struct {
	owner  string
	admin  bool
	policy *access.Policy
	groups map[string]bool
}

func (s *Server) ownerAccess(ctx context.Context, owner string) (*ownerAccessOf, error) {
	if s.ownerIsAdmin(ctx, owner) {
		return &ownerAccessOf{owner: owner, admin: true}, nil
	}
	p, groups, err := s.accessFor(ctx, owner)
	if err != nil {
		return nil, err
	}
	return &ownerAccessOf{owner: owner, policy: p, groups: groups}, nil
}

// decide: as Server.decide, for this owner.
func (o *ownerAccessOf) decide(cluster string, n access.Node) (access.Decision, bool) {
	if o.admin {
		return access.Decision{AccessEffect: devpodv1alpha1.DefaultAccessEffect(), Rule: -1}, true
	}
	return o.policy.Decide(o.owner, o.groups, cluster, n), false
}
