// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// AnnExcluded names the machines of the console's own requirement in a
// DevPod's node affinity (comma-separated, sorted): how it is told apart
// from the user's.
const AnnExcluded = "devpod.io/access-excluded"

const hostnameLabel = "kubernetes.io/hostname"

// SetExclusion makes the console's "not on these machines" requirement
// say nodes: the one written before (AnnExcluded) is taken out of every
// required term, the new one put into each (terms are ORed). No nodes
// removes it, and whatever it alone had made; the user's own terms and
// expressions stay as they are.
func SetExclusion(dp *v1.DevPod, nodes []string) {
	var prev []string
	if s := dp.Annotations[AnnExcluded]; s != "" {
		prev = strings.Split(s, ",")
	}
	if dp.Spec.Pod == nil {
		delete(dp.Annotations, AnnExcluded)
		return
	}
	spec := &dp.Spec.Pod.Spec
	if prev != nil && spec.Affinity != nil && spec.Affinity.NodeAffinity != nil && spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		sel := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		var kept []corev1.NodeSelectorTerm
		for _, t := range sel.NodeSelectorTerms {
			t.MatchExpressions = slices.DeleteFunc(slices.Clone(t.MatchExpressions), func(r corev1.NodeSelectorRequirement) bool {
				return r.Key == hostnameLabel && r.Operator == corev1.NodeSelectorOpNotIn && slices.Equal(r.Values, prev)
			})
			if len(t.MatchExpressions) > 0 || len(t.MatchFields) > 0 {
				kept = append(kept, t)
			}
		}
		sel.NodeSelectorTerms = kept
		tidy(spec)
	}
	if len(nodes) == 0 {
		delete(dp.Annotations, AnnExcluded)
		return
	}
	names := slices.Sorted(slices.Values(nodes))
	req := corev1.NodeSelectorRequirement{Key: hostnameLabel, Operator: corev1.NodeSelectorOpNotIn, Values: names}
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	sel := na.RequiredDuringSchedulingIgnoredDuringExecution
	if len(sel.NodeSelectorTerms) == 0 {
		sel.NodeSelectorTerms = []corev1.NodeSelectorTerm{{}}
	}
	for i := range sel.NodeSelectorTerms {
		sel.NodeSelectorTerms[i].MatchExpressions = append(sel.NodeSelectorTerms[i].MatchExpressions, req)
	}
	if dp.Annotations == nil {
		dp.Annotations = map[string]string{}
	}
	dp.Annotations[AnnExcluded] = strings.Join(names, ",")
}

// tidy drops the node selector, node affinity and affinity left empty.
func tidy(spec *corev1.PodSpec) {
	a := spec.Affinity
	if a == nil || a.NodeAffinity == nil {
		return
	}
	if r := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution; r != nil && len(r.NodeSelectorTerms) == 0 {
		a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
	}
	if a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil && len(a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		a.NodeAffinity = nil
	}
	if a.NodeAffinity == nil && a.PodAffinity == nil && a.PodAntiAffinity == nil {
		spec.Affinity = nil
	}
}
