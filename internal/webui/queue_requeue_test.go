// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Submitted without requeue, a job stopped for another ends instead of
// waiting again.
func TestPreemptedWithoutRequeueEnds(t *testing.T) {
	p, _ := queueRig(t, "nq", "nq-alice")
	priorityRules(t, map[string]devpodv1alpha1.JobPriorityRuleSpec{"nq-alice": {Condition: "user = nq-alice", MaxPriority: 100}})
	low := submit(t, p, "nq-alice", 1, 8, map[string]any{"preemptible": true, "requeue": false})
	roundUntil(t, p, low, devpodv1alpha1.JobStarting)
	setRank(t, ranksBecome(t, low, 1)[0].Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, low, devpodv1alpha1.JobRunning)
	submit(t, p, "nq-alice", 1, 8, map[string]any{"priority": 100})
	if j := roundUntil(t, p, low, devpodv1alpha1.JobCancelled); j.Status.Reason != "preempted" {
		t.Fatalf("low: %+v", j.Status)
	}
	ranksBecome(t, low, 0)
}

// So for a reservation's clearing.
func TestClearingWithoutRequeueEnds(t *testing.T) {
	p, _ := queueRig(t, "nc", "nc-alice")
	id := submit(t, p, "nc-alice", 1, 4, map[string]any{"timeLimitSeconds": 7200, "preemptible": true, "requeue": false})
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranksBecome(t, id, 1)
	if code, v := reserveAs(t, p.s, p.as("nc-bob"), p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	waitFor(t, func() bool {
		p.s.EnforceReservations(context.Background())
		j := jobNow(t, id)
		return j.Status.Phase == devpodv1alpha1.JobCancelled && j.Status.Reason == "preempted"
	})
}

// Requeue is the default, and the job's view says which.
func TestJobRequeueDefault(t *testing.T) {
	p := jobRig(t, "rd", "rd-alice")
	body := map[string]any{"from": map[string]any{"devpod": "rd-alice-dev1"}, "nodes": 1, "gpus": 1, "command": "true"}
	code, v := submitJob(t, p.s, p.as("rd-alice"), body)
	if code != http.StatusCreated || !v.Requeue || jobNow(t, v.ID).Spec.Requeue != nil {
		t.Fatalf("default: %d %+v %v", code, v, jobNow(t, v.ID).Spec.Requeue)
	}
	body["requeue"] = false
	code, v = submitJob(t, p.s, p.as("rd-alice"), body)
	if r := jobNow(t, v.ID).Spec.Requeue; code != http.StatusCreated || v.Requeue || r == nil || *r {
		t.Fatalf("requeue false: %d %+v %v", code, v, r)
	}
}
