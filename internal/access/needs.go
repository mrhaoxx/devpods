// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// HostMount is a host directory a pod mounts, and whether only read-only.
type HostMount struct {
	Path     string
	ReadOnly bool
}

// Needs is what a pod asks of the machine beyond an ordinary container.
type Needs struct {
	Privileged bool
	HostPaths  []HostMount
}

// NeedsOf: Privileged for a privileged container, an added capability, or
// the host's PID, IPC or network; every hostPath volume (cleaned), read-only
// when every mount of it (containers and init containers) is.
func NeedsOf(spec *corev1.PodSpec) Needs {
	var n Needs
	if spec == nil {
		return n
	}
	n.Privileged = spec.HostPID || spec.HostIPC || spec.HostNetwork
	all := append(append([]corev1.Container(nil), spec.InitContainers...), spec.Containers...)
	for _, c := range all {
		if sc := c.SecurityContext; sc != nil {
			if (sc.Privileged != nil && *sc.Privileged) || (sc.Capabilities != nil && len(sc.Capabilities.Add) > 0) {
				n.Privileged = true
			}
		}
	}
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
	if n.Privileged && !d.Privileged {
		return &Refusal{"PRIVILEGE_DENIED", "privileged containers, added capabilities and the host's PID, IPC or network are not allowed on this machine"}
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
