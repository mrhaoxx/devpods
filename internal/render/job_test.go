// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

func jobEnv(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// A job rank runs its command once: never restarted, the supervisor in its
// job mode, a grace for its command to stop, its job on its Pod.
func TestPodJobRank(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Job = &devpodv1alpha1.DevPodJob{Name: "a1b2c", Rank: 3}
	dp.Spec.Pod.Spec.Containers[0].Command = []string{"/opt/devpod/bin/bash", "-lc", "python t.py"}
	pod, err := render.Pod(dp, cfg())
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy %q", pod.Spec.RestartPolicy)
	}
	c := pod.Spec.Containers[0]
	for _, k := range []string{"SUPERVISOR_EXIT_ON_USER_CMD", "SUPERVISOR_JOB"} {
		if v := jobEnv(c, k); v != "true" {
			t.Errorf("%s=%q", k, v)
		}
	}
	if g := pod.Spec.TerminationGracePeriodSeconds; g == nil || *g != 120 {
		t.Errorf("grace %v", g)
	}
	if pod.Labels[render.JobLabel] != "a1b2c" || pod.Labels[render.RankLabel] != "3" {
		t.Errorf("labels %v", pod.Labels)
	}
	// a grace of the spec's own is kept
	thirty := int64(30)
	dp.Spec.Pod.Spec.TerminationGracePeriodSeconds = &thirty
	if pod, _ := render.Pod(dp, cfg()); pod == nil || *pod.Spec.TerminationGracePeriodSeconds != 30 {
		t.Errorf("own grace not kept")
	}
	// no command: nothing to run once
	dp.Spec.Pod.Spec.Containers[0].Command = nil
	if _, err := render.Pod(dp, cfg()); err == nil {
		t.Error("a job rank without a command rendered")
	}
}

// An owner's DevPods reach each other under isolation (a job's ranks).
func TestOwnerPolicyEgressToOwnPods(t *testing.T) {
	np := render.OwnerAllowNetworkPolicy("devpods", "alice", "devpod-system", 2222, 0)
	for _, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.PodSelector != nil && p.PodSelector.MatchLabels[render.LabelOwner] == "alice" && p.NamespaceSelector == nil && len(r.Ports) == 0 {
				return
			}
		}
	}
	t.Fatalf("no egress to the owner's pods: %+v", np.Spec.Egress)
}
