// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// ControllerCaps: of chart (the capabilities the agent was given), what
// the cluster offers given what its controller says it can do (cm, its
// devpod-controller ConfigMap; nil: nothing) — "jobs" only where the
// controller running (deployed: its image's tag, "" unknown) said it runs
// job ranks — and where it says owners' homes are shared.
func ControllerCaps(chart []string, deployed string, cm *corev1.ConfigMap) (caps []string, sharedHomes string) {
	var features []string
	if cm != nil && (deployed == "" || cm.Data[devpodv1alpha1.ControllerVersionKey] == deployed) {
		for _, f := range strings.Split(cm.Data[devpodv1alpha1.ControllerFeaturesKey], ",") {
			features = append(features, strings.TrimSpace(f))
		}
		sharedHomes = cm.Data[devpodv1alpha1.ControllerSharedHomesKey]
	}
	for _, c := range chart {
		if c == CapJobs && !slices.Contains(features, devpodv1alpha1.FeatureJobs) {
			continue // an older controller would keep restarting the ranks
		}
		caps = append(caps, c)
	}
	return caps, sharedHomes
}
