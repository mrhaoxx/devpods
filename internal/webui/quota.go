// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// QuotaViolation names one exceeded resource with the
// requested/used/limit triple, pre-formatted for the JSON error body.
type QuotaViolation struct {
	Resource  string `json:"resource"`
	Requested string `json:"requested"`
	Used      string `json:"used"`
	Limit     string `json:"limit"`
}

// QuotaError aggregates all violations of one check so the UI can
// show every exceeded bar at once.
type QuotaError struct {
	Violations []QuotaViolation `json:"violations"`
}

func (e *QuotaError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = fmt.Sprintf("%s: requested %s, used %s, limit %s", v.Resource, v.Requested, v.Used, v.Limit)
	}
	return "quota exceeded: " + strings.Join(parts, "; ")
}

// EffectiveQuota resolves a user's quota field-wise against the
// global defaults: any unset field falls back.
func EffectiveQuota(u *devpodv1alpha1.User, def devpodv1alpha1.UserQuota) devpodv1alpha1.UserQuota {
	out := def
	if u == nil || u.Spec.Quota == nil {
		return out
	}
	q := u.Spec.Quota
	if q.MaxDevPods != nil {
		out.MaxDevPods = q.MaxDevPods
	}
	if q.Compute != nil {
		out.Compute = q.Compute
	}
	if q.Storage != nil {
		out.Storage = q.Storage
	}
	return out
}

// PodLimits sums resource limits over containers + initContainers.
// Deliberately conservative: no max(init, main) refinement (spec §4.1).
func PodLimits(spec *corev1.PodSpec) corev1.ResourceList {
	sum := corev1.ResourceList{}
	add := func(cs []corev1.Container) {
		for _, c := range cs {
			for name, qty := range c.Resources.Limits {
				cur := sum[name]
				cur.Add(qty)
				sum[name] = cur
			}
		}
	}
	add(spec.Containers)
	add(spec.InitContainers)
	return sum
}

// RequireLimits enforces the must-declare-limits rule for non-admin
// DevPods: every container and initContainer needs cpu and memory
// limits, otherwise quota cannot be summed.
func RequireLimits(spec *corev1.PodSpec) error {
	check := func(kind string, cs []corev1.Container) error {
		for _, c := range cs {
			if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
				return fmt.Errorf("%s %q must declare cpu and memory limits (required for quota accounting)", kind, c.Name)
			}
		}
		return nil
	}
	if err := check("container", spec.Containers); err != nil {
		return err
	}
	return check("initContainer", spec.InitContainers)
}

// CheckQuota validates proposed against q given the user's other
// DevPods. existing MUST exclude the DevPod being updated. Compute
// counts running DevPods only (waking therefore re-checks); storage
// counts everything. Returns nil when within quota.
func CheckQuota(q devpodv1alpha1.UserQuota, existing []devpodv1alpha1.DevPod, proposed *devpodv1alpha1.DevPod) *QuotaError {
	var violations []QuotaViolation

	if q.MaxDevPods != nil && int32(len(existing))+1 > *q.MaxDevPods {
		violations = append(violations, QuotaViolation{
			Resource:  "devpods",
			Requested: "1",
			Used:      fmt.Sprintf("%d", len(existing)),
			Limit:     fmt.Sprintf("%d", *q.MaxDevPods),
		})
	}

	if len(q.Compute) > 0 && proposed.Spec.Running && proposed.Spec.Pod != nil {
		used := corev1.ResourceList{}
		for _, dp := range existing {
			if !dp.Spec.Running || dp.Spec.Pod == nil {
				continue
			}
			for name, qty := range PodLimits(&dp.Spec.Pod.Spec) {
				cur := used[name]
				cur.Add(qty)
				used[name] = cur
			}
		}
		requested := PodLimits(&proposed.Spec.Pod.Spec)
		names := make([]string, 0, len(q.Compute))
		for name := range q.Compute {
			names = append(names, string(name))
		}
		sort.Strings(names)
		for _, name := range names {
			rn := corev1.ResourceName(name)
			limit := q.Compute[rn]
			req := requested[rn]
			u := used[rn]
			total := u.DeepCopy()
			total.Add(req)
			if total.Cmp(limit) > 0 {
				violations = append(violations, QuotaViolation{
					Resource:  name,
					Requested: req.String(),
					Used:      u.String(),
					Limit:     limit.String(),
				})
			}
		}
	}

	if q.Storage != nil {
		used := resource.Quantity{}
		for _, dp := range existing {
			if dp.Spec.Persistence != nil {
				used.Add(dp.Spec.Persistence.Size)
			}
		}
		req := resource.Quantity{}
		if proposed.Spec.Persistence != nil {
			req = proposed.Spec.Persistence.Size
		}
		total := used.DeepCopy()
		total.Add(req)
		if total.Cmp(*q.Storage) > 0 {
			violations = append(violations, QuotaViolation{
				Resource:  "storage",
				Requested: req.String(),
				Used:      used.String(),
				Limit:     q.Storage.String(),
			})
		}
	}

	if len(violations) > 0 {
		return &QuotaError{Violations: violations}
	}
	return nil
}
