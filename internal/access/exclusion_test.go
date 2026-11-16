// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const hostname = "kubernetes.io/hostname"

func devpod(terms ...corev1.NodeSelectorTerm) *v1.DevPod {
	dp := &v1.DevPod{Spec: v1.DevPodSpec{Pod: &v1.PodWorkloadSpec{}}}
	if len(terms) > 0 {
		dp.Spec.Pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms}}}
	}
	return dp
}

func terms(dp *v1.DevPod) []corev1.NodeSelectorTerm {
	a := dp.Spec.Pod.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	return a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
}

func notIn(names ...string) corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{Key: hostname, Operator: corev1.NodeSelectorOpNotIn, Values: names}
}

func TestSetExclusion(t *testing.T) {
	dp := devpod()
	SetExclusion(dp, []string{"b", "a"})
	if got := terms(dp); len(got) != 1 || !reflect.DeepEqual(got[0].MatchExpressions, []corev1.NodeSelectorRequirement{notIn("a", "b")}) {
		t.Fatalf("added: %+v", got)
	}
	if dp.Annotations[AnnExcluded] != "a,b" {
		t.Fatalf("annotation %q", dp.Annotations[AnnExcluded])
	}
	SetExclusion(dp, nil)
	if dp.Spec.Pod.Spec.Affinity != nil || dp.Annotations[AnnExcluded] != "" {
		t.Fatalf("removed: %+v %v", dp.Spec.Pod.Spec.Affinity, dp.Annotations)
	}

	gpu := corev1.NodeSelectorRequirement{Key: "gpu", Operator: corev1.NodeSelectorOpIn, Values: []string{"B300"}}
	zone := corev1.NodeSelectorRequirement{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}
	own := devpod(corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{gpu}}, corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{zone}})
	SetExclusion(own, []string{"a", "b"})
	SetExclusion(own, []string{"c"}) // replaces the console's, keeps the user's
	want := []corev1.NodeSelectorTerm{
		{MatchExpressions: []corev1.NodeSelectorRequirement{gpu, notIn("c")}},
		{MatchExpressions: []corev1.NodeSelectorRequirement{zone, notIn("c")}},
	}
	if got := terms(own); !reflect.DeepEqual(got, want) {
		t.Fatalf("replaced: %+v", got)
	}
	SetExclusion(own, nil)
	want = []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{gpu}}, {MatchExpressions: []corev1.NodeSelectorRequirement{zone}}}
	if got := terms(own); !reflect.DeepEqual(got, want) || own.Annotations[AnnExcluded] != "" {
		t.Fatalf("the user's kept: %+v", got)
	}
}
