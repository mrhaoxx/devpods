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
	if n := NeedsOf(&corev1.PodSpec{Containers: []corev1.Container{{SecurityContext: &corev1.SecurityContext{Privileged: &yes}}}}); !n.PrivilegedContainer {
		t.Fatal("a privileged container")
	}
	if n := NeedsOf(&corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{}}}); !n.HostNetwork {
		t.Fatal("the host's network")
	}
	caps := &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"IPC_LOCK"}}}
	if n := NeedsOf(&corev1.PodSpec{Containers: []corev1.Container{{SecurityContext: caps}}}); len(n.Capabilities) != 1 {
		t.Fatal("an added capability")
	}
	if n := NeedsOf(&corev1.PodSpec{Containers: []corev1.Container{{}}}); !reflect.DeepEqual(n, Needs{}) {
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
		{"root, writable, covers all", Decision{AccessEffect: v1.DefaultAccessEffect()}, Needs{PrivilegedContainer: true, HostPaths: []HostMount{{Path: "/var/run/docker.sock"}}}, ""},
		{"denied", Decision{}, Needs{}, "ACL_DENIED"},
		{"privileged not allowed", ro, Needs{PrivilegedContainer: true}, "PRIVILEGE_DENIED"},
	} {
		if got := code(c.d.Admits(c.n)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Each of the five is its own: the host's PID, IPC and network, a
// privileged container, the capabilities added (named as the grants are).
func TestNeedsOfSplit(t *testing.T) {
	yes := true
	plain := []corev1.Container{{}}
	withCaps := func(cs ...corev1.Capability) corev1.Container {
		return corev1.Container{SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: cs}}}
	}
	for name, c := range map[string]struct {
		spec corev1.PodSpec
		want Needs
	}{
		"host PID":                    {corev1.PodSpec{HostPID: true, Containers: plain}, Needs{HostPID: true}},
		"host IPC":                    {corev1.PodSpec{HostIPC: true, Containers: plain}, Needs{HostIPC: true}},
		"host network":                {corev1.PodSpec{HostNetwork: true, Containers: plain}, Needs{HostNetwork: true}},
		"a privileged init container": {corev1.PodSpec{InitContainers: []corev1.Container{{SecurityContext: &corev1.SecurityContext{Privileged: &yes}}}, Containers: plain}, Needs{PrivilegedContainer: true}},
		"capabilities, named alike":   {corev1.PodSpec{Containers: []corev1.Container{withCaps("cap_sys_ptrace", "NET_ADMIN"), withCaps(" NET_ADMIN")}}, Needs{Capabilities: []string{"NET_ADMIN", "SYS_PTRACE"}}},
	} {
		if got := NeedsOf(&c.spec); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}

func TestAdmitsEachPrivilege(t *testing.T) {
	grant := func(p v1.AccessPrivileges) Decision {
		return Decision{AccessEffect: v1.AccessEffect{Allow: true, Privileges: &p}}
	}
	pidOnly := grant(v1.AccessPrivileges{HostPID: true})
	ptrace := grant(v1.AccessPrivileges{Capabilities: []string{"sys_ptrace"}})
	anyCap := grant(v1.AccessPrivileges{Capabilities: []string{"*"}})
	container := grant(v1.AccessPrivileges{Privileged: true})
	legacy := Decision{AccessEffect: v1.AccessEffect{Allow: true, Privileged: true}}
	msg := func(r *Refusal) string {
		if r == nil {
			return ""
		}
		if r.Code != "PRIVILEGE_DENIED" {
			return r.Code
		}
		return r.Message
	}
	for _, c := range []struct {
		name string
		d    Decision
		n    Needs
		want string
	}{
		{"host PID granted", pidOnly, Needs{HostPID: true}, ""},
		{"host network not", pidOnly, Needs{HostNetwork: true}, "the host's network is not allowed on this machine"},
		{"host IPC not", pidOnly, Needs{HostIPC: true}, "the host's IPC is not allowed on this machine"},
		{"host PID not", ptrace, Needs{HostPID: true}, "the host's PID is not allowed on this machine"},
		{"a capability granted in another case", ptrace, Needs{Capabilities: []string{"SYS_PTRACE"}}, ""},
		{"a capability not", ptrace, Needs{Capabilities: []string{"NET_ADMIN", "SYS_PTRACE"}}, "capability NET_ADMIN is not allowed on this machine"},
		{"ALL wants any", ptrace, Needs{Capabilities: []string{"ALL"}}, "capability ALL is not allowed on this machine"},
		{"ALL under any", anyCap, Needs{Capabilities: []string{"ALL"}}, ""},
		{"privileged containers bring every capability", container, Needs{Capabilities: []string{"NET_ADMIN"}}, ""},
		{"a privileged container not", anyCap, Needs{PrivilegedContainer: true}, "privileged containers are not allowed on this machine"},
		{"the first missing is said", pidOnly, Needs{PrivilegedContainer: true, HostNetwork: true}, "privileged containers are not allowed on this machine"},
		{"the old privileged: all five", legacy, Needs{PrivilegedContainer: true, Capabilities: []string{"ALL"}, HostPID: true, HostIPC: true, HostNetwork: true}, ""},
	} {
		if got := msg(c.d.Admits(c.n)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

var allFive = v1.AccessPrivileges{Privileged: true, Capabilities: []string{"*"}, HostPID: true, HostIPC: true, HostNetwork: true}

func TestGrantsLegacy(t *testing.T) {
	for name, c := range map[string]struct {
		e    v1.AccessEffect
		want v1.AccessPrivileges
	}{
		"the old privileged":                  {v1.AccessEffect{Allow: true, Privileged: true}, allFive},
		"nothing":                             {v1.AccessEffect{Allow: true}, v1.AccessPrivileges{}},
		"privileges set: the old one ignored": {v1.AccessEffect{Allow: true, Privileged: true, Privileges: &v1.AccessPrivileges{HostPID: true}}, v1.AccessPrivileges{HostPID: true}},
		"capabilities named alike":            {v1.AccessEffect{Allow: true, Privileges: &v1.AccessPrivileges{Capabilities: []string{"cap_net_admin", " sys_ptrace ", "NET_ADMIN"}}}, v1.AccessPrivileges{Capabilities: []string{"NET_ADMIN", "SYS_PTRACE"}}},
	} {
		if got := Grants(c.e); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}

func TestNormalizeEffect(t *testing.T) {
	all := allFive
	for name, c := range map[string]struct {
		e, want v1.AccessEffect
	}{
		"a deny keeps nothing": {v1.AccessEffect{Privileged: true, HostPaths: []v1.HostPathGrant{{Path: "/"}}, NeedsReservation: true}, v1.AccessEffect{}},
		"the old privileged":   {v1.AccessEffect{Allow: true, Privileged: true, NeedsReservation: true}, v1.AccessEffect{Allow: true, Privileges: &all, NeedsReservation: true}},
		"no privileges":        {v1.AccessEffect{Allow: true}, v1.AccessEffect{Allow: true}},
	} {
		if got := NormalizeEffect(c.e); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}
