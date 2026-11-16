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

func in(names ...string) corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{Key: hostname, Operator: corev1.NodeSelectorOpIn, Values: names}
}

func TestSetAllowed(t *testing.T) {
	dp := devpod()
	SetAllowed(dp, []string{"b", "a"})
	if got := terms(dp); len(got) != 1 || !reflect.DeepEqual(got[0].MatchExpressions, []corev1.NodeSelectorRequirement{in("a", "b")}) {
		t.Fatalf("added: %+v", got)
	}
	if dp.Annotations[AnnAllowed] != "a,b" {
		t.Fatalf("annotation %q", dp.Annotations[AnnAllowed])
	}
	SetAllowed(dp, nil)
	if dp.Spec.Pod.Spec.Affinity != nil || dp.Annotations[AnnAllowed] != "" {
		t.Fatalf("removed: %+v %v", dp.Spec.Pod.Spec.Affinity, dp.Annotations)
	}

	gpu := corev1.NodeSelectorRequirement{Key: "gpu", Operator: corev1.NodeSelectorOpIn, Values: []string{"B300"}}
	zone := corev1.NodeSelectorRequirement{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}
	own := devpod(corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{gpu}}, corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{zone}})
	SetAllowed(own, []string{"a", "b"})
	SetAllowed(own, []string{"c"}) // replaces the console's, keeps the user's
	want := []corev1.NodeSelectorTerm{
		{MatchExpressions: []corev1.NodeSelectorRequirement{gpu, in("c")}},
		{MatchExpressions: []corev1.NodeSelectorRequirement{zone, in("c")}},
	}
	if got := terms(own); !reflect.DeepEqual(got, want) {
		t.Fatalf("replaced: %+v", got)
	}
	SetAllowed(own, nil)
	want = []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{gpu}}, {MatchExpressions: []corev1.NodeSelectorRequirement{zone}}}
	if got := terms(own); !reflect.DeepEqual(got, want) || own.Annotations[AnnAllowed] != "" {
		t.Fatalf("the user's kept: %+v", got)
	}
	// the user's own hostname In with the same names is theirs: not taken
	mine := devpod(corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{in("a")}})
	SetAllowed(mine, nil)
	if got := terms(mine); len(got) != 1 || !reflect.DeepEqual(got[0].MatchExpressions, []corev1.NodeSelectorRequirement{in("a")}) {
		t.Fatalf("the user's hostname In taken: %+v", got)
	}
}
