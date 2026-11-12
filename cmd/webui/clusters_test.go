// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Every namespaced kind the console keeps of a cluster is read in the
// DevPod namespace: the agent's rights to it are a Role there (see the
// chart's agent-rbac.yaml), and a cache that cannot list one kind never
// becomes usable.
func TestClusterCache_NamespacedKindsAreScoped(t *testing.T) {
	warm, byObject := clusterCache("devpods")
	clusterScoped := map[reflect.Type]bool{
		reflect.TypeOf(&corev1.Node{}):                true,
		reflect.TypeOf(&devpodv1alpha1.Reservation{}): true,
	}
	scoped := map[reflect.Type]bool{}
	for obj, by := range byObject {
		if _, ok := by.Namespaces["devpods"]; !ok || len(by.Namespaces) != 1 {
			t.Fatalf("%T is not read in the DevPod namespace alone: %v", obj, by.Namespaces)
		}
		scoped[reflect.TypeOf(obj)] = true
	}
	for _, obj := range warm {
		ty := reflect.TypeOf(obj)
		if !clusterScoped[ty] && !scoped[ty] {
			t.Fatalf("%T is namespaced and would be listed across all namespaces", obj)
		}
		if clusterScoped[ty] && scoped[ty] {
			t.Fatalf("%T is cluster-scoped but given a namespace", obj)
		}
	}
}
