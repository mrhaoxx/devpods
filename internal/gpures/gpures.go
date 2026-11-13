// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package gpures says which extended resources are GPUs. A cluster's
// cards are not all NVIDIA's: each vendor's device plugin has a resource
// name of its own, and a GPU count is the sum over them. The list is the
// site's to give (--gpu-resources); without it, the vendors known here.
package gpures

import (
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// Type is one kind of GPU: its resource (one whole card each) and what
// people call it.
type Type struct {
	Resource corev1.ResourceName `json:"resource"`
	Label    string              `json:"label"`
}

// Total is the quota's name for GPUs of every type together: a limit on
// it is a limit on their sum. (A limit on one type's own resource still
// limits that type.)
const Total corev1.ResourceName = "devpod.io/gpu"

// NVIDIA's resource: what a quota's GPU limit was written as before
// there were several types (read as Total where Total is not set).
const NVIDIA corev1.ResourceName = "nvidia.com/gpu"

var (
	mu    sync.RWMutex
	types = []Type{{NVIDIA, "NVIDIA"}, {"mthreads.com/gpu", "Moore Threads"}, {"metax-tech.com/gpu", "MetaX"}}
)

// Set replaces the types with spec: "resource=Label,resource=Label…"
// (a resource alone is labelled by its domain); the first is the default
// type. "" keeps the built-in ones.
func Set(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	var out []Type
	seen := map[corev1.ResourceName]bool{}
	for _, item := range strings.Split(spec, ",") {
		name, label, _ := strings.Cut(strings.TrimSpace(item), "=")
		name, label = strings.TrimSpace(name), strings.TrimSpace(label)
		if name == "" || !strings.Contains(name, "/") || seen[corev1.ResourceName(name)] || corev1.ResourceName(name) == Total {
			return fmt.Errorf("gpu resources: %q is not a resource name given once (e.g. nvidia.com/gpu=NVIDIA)", item)
		}
		if label == "" {
			label, _, _ = strings.Cut(name, "/")
		}
		seen[corev1.ResourceName(name)] = true
		out = append(out, Type{corev1.ResourceName(name), label})
	}
	mu.Lock()
	types = out
	mu.Unlock()
	return nil
}

// Types lists the types, the default first.
func Types() []Type {
	mu.RLock()
	defer mu.RUnlock()
	return append([]Type(nil), types...)
}

// Is says whether name is a GPU type's resource.
func Is(name corev1.ResourceName) bool {
	for _, t := range Types() {
		if t.Resource == name {
			return true
		}
	}
	return false
}

// Resolve finds a type by its resource or its label (any case); "" is
// the default type.
func Resolve(s string) (corev1.ResourceName, bool) {
	ts := Types()
	if s == "" {
		return ts[0].Resource, true
	}
	for _, t := range ts {
		if string(t.Resource) == s || strings.EqualFold(t.Label, s) {
			return t.Resource, true
		}
	}
	return "", false
}

// Count is the number of GPUs in l, of every type.
func Count(l corev1.ResourceList) int {
	n := 0
	for _, t := range Types() {
		if q, ok := l[t.Resource]; ok {
			n += int(q.Value())
		}
	}
	return n
}

// Vendor labels whose cards l has ("" for none, the first for several).
func Vendor(l corev1.ResourceList) string {
	for _, t := range Types() {
		if q, ok := l[t.Resource]; ok && !q.IsZero() {
			return t.Label
		}
	}
	return ""
}
