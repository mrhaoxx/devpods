// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"fmt"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// HostMount is a host directory a pod mounts, and whether only read-only.
type HostMount struct {
	Path     string
	ReadOnly bool
}

// Needs is what a pod asks of the machine beyond an ordinary container.
type Needs struct {
	PrivilegedContainer bool
	// Capabilities added, named as NormalizeCap names them, sorted, once.
	Capabilities []string
	HostPID      bool
	HostIPC      bool
	HostNetwork  bool
	HostPaths    []HostMount
}

// NeedsOf: each of a privileged container, the capabilities added and the
// host's PID, IPC and network (containers and init containers alike);
// every hostPath volume (cleaned), read-only when every mount of it is.
func NeedsOf(spec *corev1.PodSpec) Needs {
	var n Needs
	if spec == nil {
		return n
	}
	n.HostPID, n.HostIPC, n.HostNetwork = spec.HostPID, spec.HostIPC, spec.HostNetwork
	all := append(append([]corev1.Container(nil), spec.InitContainers...), spec.Containers...)
	var caps []string
	for _, c := range all {
		if sc := c.SecurityContext; sc != nil {
			if sc.Privileged != nil && *sc.Privileged {
				n.PrivilegedContainer = true
			}
			if sc.Capabilities != nil {
				for _, c := range sc.Capabilities.Add {
					caps = append(caps, string(c))
				}
			}
		}
	}
	n.Capabilities = normalizeCaps(caps)
	for _, v := range spec.Volumes {
		if v.HostPath == nil {
			continue
		}
		ro := true
		for _, c := range all {
			for _, m := range c.VolumeMounts {
				if m.Name == v.Name && !m.ReadOnly {
					ro = false
				}
			}
		}
		n.HostPaths = append(n.HostPaths, HostMount{Path: cleanPath(v.HostPath.Path), ReadOnly: ro})
	}
	return n
}

// NormalizeCap names a capability one way: SYS_PTRACE for cap_sys_ptrace.
func NormalizeCap(s string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "CAP_")
}

// normalizeCaps: named one way, sorted, once each; nil for none.
func normalizeCaps(in []string) []string {
	var out []string
	for _, c := range in {
		if c = NormalizeCap(c); c != "" {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Grants: the privileges an effect gives — Privileges, or, in the old
// form, all five for Privileged.
func Grants(e v1.AccessEffect) v1.AccessPrivileges {
	switch {
	case e.Privileges != nil:
		p := *e.Privileges
		p.Capabilities = normalizeCaps(p.Capabilities)
		return p
	case e.Privileged:
		return v1.AccessPrivileges{Privileged: true, Capabilities: []string{"*"}, HostPID: true, HostIPC: true, HostNetwork: true}
	}
	return v1.AccessPrivileges{}
}

// NormalizeEffect writes an effect in the new form: a deny with nothing
// else; an allow with its privileges in Privileges (none: nil).
func NormalizeEffect(e v1.AccessEffect) v1.AccessEffect {
	if !e.Allow {
		return v1.AccessEffect{}
	}
	out := e
	out.Privileged, out.Privileges = false, nil
	if g := Grants(e); g.Privileged || len(g.Capabilities) > 0 || g.HostPID || g.HostIPC || g.HostNetwork {
		out.Privileges = &g
	}
	return out
}

func cleanPath(p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return p
}

// Refusal is why a decision does not admit a pod.
type Refusal struct {
	Code    string // ACL_DENIED or PRIVILEGE_DENIED
	Message string
}

// Admits reports whether the pod's needs fit the decision (nil: they do).
// The reservation a decision may need is the caller's to check.
func (d Decision) Admits(n Needs) *Refusal {
	if !d.Allow {
		return &Refusal{"ACL_DENIED", "you may not use this machine"}
	}
	g := Grants(d.AccessEffect)
	deny := func(what string) *Refusal { return &Refusal{"PRIVILEGE_DENIED", what + " not allowed on this machine"} }
	if n.PrivilegedContainer && !g.Privileged {
		return deny("privileged containers are")
	}
	if !g.Privileged && !slices.Contains(g.Capabilities, "*") {
		for _, c := range n.Capabilities {
			if !slices.Contains(g.Capabilities, c) {
				return deny("capability " + c + " is")
			}
		}
	}
	switch {
	case n.HostPID && !g.HostPID:
		return deny("the host's PID is")
	case n.HostIPC && !g.HostIPC:
		return deny("the host's IPC is")
	case n.HostNetwork && !g.HostNetwork:
		return deny("the host's network is")
	}
	for _, h := range n.HostPaths {
		if !strings.HasPrefix(h.Path, "/") {
			return &Refusal{"PRIVILEGE_DENIED", fmt.Sprintf("host path %q is not absolute", h.Path)}
		}
		p := path.Clean(h.Path)
		covered, writable := false, false
		for _, g := range d.HostPaths {
			gp := path.Clean(g.Path)
			if gp == "/" || p == gp || strings.HasPrefix(p, gp+"/") {
				covered = true
				writable = writable || g.Writable
			}
		}
		switch {
		case !covered:
			return &Refusal{"PRIVILEGE_DENIED", fmt.Sprintf("host path %s may not be mounted on this machine", p)}
		case !writable && !h.ReadOnly:
			return &Refusal{"PRIVILEGE_DENIED", fmt.Sprintf("host path %s may only be mounted read-only on this machine", p)}
		}
	}
	return nil
}
