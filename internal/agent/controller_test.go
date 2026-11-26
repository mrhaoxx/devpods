// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The cluster takes queue jobs only where its controller, the one
// running, says it runs them; where homes are shared is the controller's
// to say too.
func TestControllerCaps(t *testing.T) {
	cm := func(version, features, homes string) *corev1.ConfigMap {
		d := map[string]string{"version": version, "features": features}
		if homes != "" {
			d["sharedHomes"] = homes
		}
		return &corev1.ConfigMap{Data: d}
	}
	chart := []string{"snapshots", "jobs", "telemetry"}
	if caps, homes := ControllerCaps(chart, "v0.9.48", cm("v0.9.48", "jobs", "/home")); !slices.Equal(caps, chart) || homes != "/home" {
		t.Errorf("the running controller runs jobs: %v %q", caps, homes)
	}
	for name, c := range map[string]struct {
		deployed string
		cm       *corev1.ConfigMap
	}{
		"no word from the controller (an older one)": {"v0.5.4", nil},
		"word from another version (a downgrade)":    {"v0.5.4", cm("v0.9.48", "jobs", "/home")},
		"a controller that does not run jobs":        {"v0.9.48", cm("v0.9.48", "", "")},
	} {
		caps, homes := ControllerCaps(chart, c.deployed, c.cm)
		if slices.Contains(caps, "jobs") || !slices.Contains(caps, "snapshots") || homes != "" {
			t.Errorf("%s: %v %q", name, caps, homes)
		}
	}
	// the deployed version unknown: its word is taken
	if caps, _ := ControllerCaps(chart, "", cm("v0.9.48", "jobs", "")); !slices.Contains(caps, "jobs") {
		t.Errorf("unknown deployed version: %v", caps)
	}
	// not asked of the chart: not given
	if caps, _ := ControllerCaps([]string{"snapshots"}, "v0.9.48", cm("v0.9.48", "jobs", "")); slices.Contains(caps, "jobs") {
		t.Errorf("not enabled: %v", caps)
	}
}
