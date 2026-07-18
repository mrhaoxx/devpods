// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// ApplyTemplate stamps tpl onto dp, server-side. This is the ONLY
// code path that writes kore.zjusct.io/* annotations onto non-admin
// DevPods. It re-validates the binding defensively — templates are
// validated at save time, but an out-of-band kubectl edit must not
// let a bad binding through.
//
// Order matters: PodPreset first (it may construct dp.Spec.Pod), then
// Binding (it requires dp.Spec.Pod and overrides resources).
func ApplyTemplate(dp *devpodv1alpha1.DevPod, tpl *devpodv1alpha1.DevPodTemplate) error {
	if tpl.Spec.PodPreset == nil && tpl.Spec.Binding == nil {
		return fmt.Errorf("template %q has neither podPreset nor binding", tpl.Name)
	}

	if p := tpl.Spec.PodPreset; p != nil {
		if dp.Spec.Pod != nil {
			return fmt.Errorf("template %q is a full preset; the request must not carry its own pod spec", tpl.Name)
		}
		// Passthrough: the preset carries a full pod spec (image, resources,
		// securityContext, volumes, extra containers, …). Stamp it verbatim.
		dp.Spec.Pod = p.Pod.DeepCopy()
		if dp.Spec.Persistence == nil && p.Persistence != nil {
			dp.Spec.Persistence = p.Persistence.DeepCopy()
		}
		if dp.Spec.Shell == "" {
			dp.Spec.Shell = p.Shell
		}
	}

	if b := tpl.Spec.Binding; b != nil {
		if err := ValidateBinding(b); err != nil {
			return fmt.Errorf("template %q binding invalid: %w", tpl.Name, err)
		}
		if dp.Spec.Pod == nil || len(dp.Spec.Pod.Spec.Containers) == 0 {
			return fmt.Errorf("template %q is a binding overlay; the request must supply a pod spec (or use a preset template)", tpl.Name)
		}
		if dp.Spec.Pod.Metadata.Annotations == nil {
			dp.Spec.Pod.Metadata.Annotations = map[string]string{}
		}
		for k, v := range b.Annotations {
			dp.Spec.Pod.Metadata.Annotations[k] = v
		}

		target := &dp.Spec.Pod.Spec.Containers[0]
		if dp.Spec.Persistence != nil && dp.Spec.Persistence.TargetContainer != "" {
			found := false
			for i := range dp.Spec.Pod.Spec.Containers {
				if dp.Spec.Pod.Spec.Containers[i].Name == dp.Spec.Persistence.TargetContainer {
					target = &dp.Spec.Pod.Spec.Containers[i]
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("persistence.targetContainer %q not found", dp.Spec.Persistence.TargetContainer)
			}
		}
		if target.Resources.Requests == nil {
			target.Resources.Requests = corev1.ResourceList{}
		}
		if target.Resources.Limits == nil {
			target.Resources.Limits = corev1.ResourceList{}
		}
		for name, qty := range b.Resources.Requests {
			target.Resources.Requests[name] = qty
		}
		for name, qty := range b.Resources.Limits {
			target.Resources.Limits[name] = qty
		}
	}
	return nil
}
