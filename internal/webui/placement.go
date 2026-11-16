// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
)

// Where a DevPod may run (access control design §5–§7), judged for its
// owner — never the one acting: a DevPod pinned to a machine must fit
// there; one that is not carries the console's "not on these machines"
// for every machine of its cluster it does not fit (access.SetExclusion).

// judge is the access rules as they apply to one owner in ctx's cluster,
// read once for the many machines of a cluster.
type judge struct {
	admin    bool
	policy   *devpodv1alpha1.AccessPolicySpec
	owner    string
	groups   map[string]bool
	cluster  string
	reserved map[string]bool // machines the owner holds a reservation of now
}

func (s *Server) judgeFor(ctx context.Context, owner string) (*judge, error) {
	j := &judge{owner: owner, cluster: s.resCluster(ctx)}
	if j.admin = s.ownerIsAdmin(ctx, owner); j.admin {
		return j, nil
	}
	var err error
	if j.policy, _, err = s.accessPolicy(ctx); err != nil {
		return nil, err
	}
	if j.groups, err = s.groupsOf(ctx, owner); err != nil {
		return nil, err
	}
	j.reserved = map[string]bool{}
	if s.Reservations != nil {
		now := s.Reservations.now()
		all, err := s.nodeReservations(ctx)
		if err != nil {
			return nil, err
		}
		for i := range all {
			nr := &all[i]
			if nr.Spec.Cluster == j.cluster && nr.Spec.User == owner && !now.Before(nr.Spec.Start.Time) && now.Before(overAt(nr)) {
				j.reserved[nr.Spec.Node] = true
			}
		}
	}
	return j, nil
}

// on: the decision for a machine (allow-all for an admin).
func (j *judge) on(n *corev1.Node) access.Decision {
	if j.admin {
		return access.Decision{AccessEffect: devpodv1alpha1.DefaultAccessEffect(), Rule: -1}
	}
	return access.Decide(j.policy, j.owner, j.groups, j.cluster, access.Node{Name: n.Name, Labels: n.Labels})
}

// refusal: why the owner may not have a DevPod with needs on n (nil: may).
// forRun adds the reservation the rule may ask for.
func (j *judge) refusal(n *corev1.Node, needs *access.Needs, forRun bool) *serverErr {
	if j.admin {
		return nil
	}
	d := j.on(n)
	if !d.Allow {
		return &serverErr{http.StatusForbidden, "ACL_DENIED", fmt.Sprintf("%s may not use node %s", j.owner, n.Name), map[string]any{"node": n.Name, "rule": d.Rule}}
	}
	if forRun && d.NeedsReservation && !j.reserved[n.Name] {
		return &serverErr{http.StatusConflict, "RESERVATION_REQUIRED", fmt.Sprintf("node %s may be used only under a reservation of %s's: reserve it first", n.Name, j.owner), map[string]any{"node": n.Name, "rule": d.Rule}}
	}
	if needs != nil {
		if r := d.Admits(*needs); r != nil {
			return &serverErr{http.StatusForbidden, r.Code, r.Message, map[string]any{"node": n.Name, "rule": d.Rule}}
		}
	}
	return nil
}

// nodeErr: may owner's DevPod be on node of ctx's cluster? forRun adds
// the reservation requirement. A node that does not exist is not this
// rule's to say.
func (s *Server) nodeErr(ctx context.Context, owner, node string, forRun bool) *serverErr {
	var n corev1.Node
	if err := s.kc(ctx).Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return nil
	}
	j, err := s.judgeFor(ctx, owner)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	return j.refusal(&n, nil, forRun)
}

// placeDevPod is for a DevPod about to run with its Pod (re)created:
// pinned, it must fit its machine; not pinned, the machines it does not
// fit go into its exclusion, and it needs at least one it fits.
func (s *Server) placeDevPod(ctx context.Context, dp *devpodv1alpha1.DevPod) *serverErr {
	if dp.Spec.Pod == nil {
		return nil
	}
	j, err := s.judgeFor(ctx, dp.Spec.Owner)
	if err != nil {
		return &serverErr{http.StatusInternalServerError, "INTERNAL", err.Error(), nil}
	}
	if j.admin {
		access.SetExclusion(dp, nil)
		return nil
	}
	needs := access.NeedsOf(&dp.Spec.Pod.Spec)
	if node := pinnedNode(dp); node != "" {
		access.SetExclusion(dp, nil)
		var n corev1.Node
		if err := s.kc(ctx).Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
			return badRequest("node %q not found", node)
		}
		return j.refusal(&n, &needs, true)
	}
	var nodes corev1.NodeList
	if err := s.kc(ctx).Client.List(ctx, &nodes); err != nil {
		return &serverErr{http.StatusServiceUnavailable, "CLUSTER_UNAVAILABLE", err.Error(), nil}
	}
	var out []string
	fits := 0
	for i := range nodes.Items {
		if j.refusal(&nodes.Items[i], &needs, true) == nil {
			fits++
		} else {
			out = append(out, nodes.Items[i].Name)
		}
	}
	// no machine known at all (a cluster just joined, its cache still
	// filling) is no reason to refuse: there is nothing to keep it off
	if fits == 0 && len(nodes.Items) > 0 {
		return &serverErr{http.StatusForbidden, "ACL_DENIED", fmt.Sprintf("there is no machine in this cluster %s may use for this DevPod", dp.Spec.Owner), nil}
	}
	access.SetExclusion(dp, out)
	return nil
}

// accessOf: what a node is to the caller in the node choices — "ok",
// "reservation" (needs one they do not hold now) or "denied".
func (j *judge) accessOf(n *corev1.Node) (string, access.Decision) {
	d := j.on(n)
	switch {
	case !d.Allow:
		return "denied", d
	case d.NeedsReservation && !j.admin && !j.reserved[n.Name]:
		return "reservation", d
	}
	return "ok", d
}

// placeOnChange applies the access rules to a change of a DevPod: running
// and started, moved, or with its Pod's spec changed → placeDevPod;
// stopped and moved to a machine → nodeErr (no reservation needed yet).
// Nothing else is checked: a running DevPod edited in what does not
// remake its Pod keeps its spec as it is.
func (s *Server) placeOnChange(ctx context.Context, before, dp *devpodv1alpha1.DevPod) *serverErr {
	moved := pinnedNode(dp) != pinnedNode(before)
	remade := !equality.Semantic.DeepEqual(before.Spec.Pod, dp.Spec.Pod) || before.Spec.Shell != dp.Spec.Shell
	switch {
	case dp.Spec.Running && (!before.Spec.Running || moved || remade):
		return s.placeDevPod(ctx, dp)
	case !dp.Spec.Running && moved && pinnedNode(dp) != "":
		return s.nodeErr(ctx, dp.Spec.Owner, pinnedNode(dp), false)
	}
	return nil
}
