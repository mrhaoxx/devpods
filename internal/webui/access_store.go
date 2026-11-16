// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"log/slog"
	"slices"

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
	p, _, err := s.accessPolicy(ctx)
	if err != nil {
		slog.Warn("cluster_gate", "user", sess.User, "err", err)
		return all
	}
	groups, err := s.groupsOf(ctx, sess.User)
	if err != nil {
		slog.Warn("cluster_gate", "user", sess.User, "err", err)
		return all
	}
	return func(cluster string) bool { return access.ClusterOpen(p, sess.User, groups, cluster) }
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
	if s.ownerIsAdmin(ctx, owner) {
		return access.Decision{AccessEffect: devpodv1alpha1.DefaultAccessEffect(), Rule: -1}, true, nil
	}
	p, _, err := s.accessPolicy(ctx)
	if err != nil {
		return access.Decision{}, false, err
	}
	groups, err := s.groupsOf(ctx, owner)
	if err != nil {
		return access.Decision{}, false, err
	}
	return access.Decide(p, owner, groups, cluster, n), false, nil
}
