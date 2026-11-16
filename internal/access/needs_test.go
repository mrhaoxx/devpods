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

func hostVol(name, path string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path}}}
}

func TestNeedsOf(t *testing.T) {
	yes := true
	if n := NeedsOf(&corev1.PodSpec{Containers: []corev1.Container{{SecurityContext: &corev1.SecurityContext{Privileged: &yes}}}}); !n.Privileged {
		t.Fatal("a privileged container")
	}
	if n := NeedsOf(&corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{}}}); !n.Privileged {
		t.Fatal("the host's network")
	}
	caps := &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"IPC_LOCK"}}}
	if n := NeedsOf(&corev1.PodSpec{Containers: []corev1.Container{{SecurityContext: caps}}}); !n.Privileged {
		t.Fatal("an added capability")
	}
	if n := NeedsOf(&corev1.PodSpec{Containers: []corev1.Container{{}}}); n.Privileged || len(n.HostPaths) != 0 {
		t.Fatalf("a plain pod: %+v", n)
	}
	spec := &corev1.PodSpec{
		Volumes: []corev1.Volume{hostVol("models", "/data/models/"), hostVol("unused", "/srv")},
		Containers: []corev1.Container{
			{VolumeMounts: []corev1.VolumeMount{{Name: "models", MountPath: "/m", ReadOnly: true}}},
			{VolumeMounts: []corev1.VolumeMount{{Name: "models", MountPath: "/m"}}},
		},
	}
	want := []HostMount{{Path: "/data/models", ReadOnly: false}, {Path: "/srv", ReadOnly: true}}
	if got := NeedsOf(spec).HostPaths; !reflect.DeepEqual(got, want) {
		t.Fatalf("host paths: %+v", got)
	}
}

func TestAdmitsHostPaths(t *testing.T) {
	ro := Decision{AccessEffect: v1.AccessEffect{Allow: true, HostPaths: []v1.HostPathGrant{{Path: "/data"}}}}
	code := func(r *Refusal) string {
		if r == nil {
			return ""
		}
		return r.Code
	}
	for _, c := range []struct {
		name string
		d    Decision
		n    Needs
		want string
	}{
		{"read-only under the grant", ro, Needs{HostPaths: []HostMount{{Path: "/data/models", ReadOnly: true}}}, ""},
		{"writable under a read-only grant", ro, Needs{HostPaths: []HostMount{{Path: "/data/models"}}}, "PRIVILEGE_DENIED"},
		{"cleaned out of the grant", ro, Needs{HostPaths: []HostMount{{Path: "/data/../etc", ReadOnly: true}}}, "PRIVILEGE_DENIED"},
		{"a sibling with the prefix", ro, Needs{HostPaths: []HostMount{{Path: "/database", ReadOnly: true}}}, "PRIVILEGE_DENIED"},
		{"relative", ro, Needs{HostPaths: []HostMount{{Path: "data", ReadOnly: true}}}, "PRIVILEGE_DENIED"},
		{"root, writable, covers all", Decision{AccessEffect: v1.DefaultAccessEffect()}, Needs{Privileged: true, HostPaths: []HostMount{{Path: "/var/run/docker.sock"}}}, ""},
		{"denied", Decision{}, Needs{}, "ACL_DENIED"},
		{"privileged not allowed", ro, Needs{Privileged: true}, "PRIVILEGE_DENIED"},
	} {
		if got := code(c.d.Admits(c.n)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
