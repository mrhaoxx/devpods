// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

// queueRig: jobRig with a clock the test moves, from 10:10 +08:00.
func queueRig(t *testing.T, prefix, user string) (*placeRig, *time.Time) {
	t.Helper()
	p := jobRig(t, prefix, user)
	clock := time.Date(2026, 11, 12, 2, 10, 0, 0, time.UTC)
	p.s.Queue.Now = func() time.Time { return clock }
	return p, &clock
}

// submit: user's job of nodes × gpus from <user>-dev1 (extra fields too).
func submit(t *testing.T, p *placeRig, user string, nodes, gpus int, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"from": map[string]any{"devpod": user + "-dev1"}, "nodes": nodes, "gpus": gpus, "cpus": 4, "command": "python t.py"}
	for k, v := range extra {
		body[k] = v
	}
	code, v := submitJob(t, p.s, p.as(user), body)
	if code != http.StatusCreated {
		t.Fatalf("submit: %d %+v", code, v)
	}
	return v.ID
}

func jobNow(t *testing.T, id string) devpodv1alpha1.Job {
	t.Helper()
	var j devpodv1alpha1.Job
	if err := k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Name: id}, &j); err != nil {
		t.Fatal(err)
	}
	return j
}

// rankDevPods: the job's rank DevPods the cache has.
func rankDevPods(t *testing.T, id string) []devpodv1alpha1.DevPod {
	t.Helper()
	var l devpodv1alpha1.DevPodList
	if err := k8sClient.List(context.Background(), &l, client.InNamespace("devpods"), client.MatchingLabels{"devpod.io/job": id}); err != nil {
		t.Fatal(err)
	}
	var out []devpodv1alpha1.DevPod
	for _, d := range l.Items {
		if d.DeletionTimestamp == nil {
			out = append(out, d)
		}
	}
	return out
}

// ranksBecome waits until the cache has n ranks of the job.
func ranksBecome(t *testing.T, id string, n int) []devpodv1alpha1.DevPod {
	t.Helper()
	var got []devpodv1alpha1.DevPod
	waitFor(t, func() bool { got = rankDevPods(t, id); return len(got) == n })
	return got
}

// setRank plays the controller: the rank DevPod's phase (and exit code).
func setRank(t *testing.T, name string, phase devpodv1alpha1.DevPodPhase, code *int32) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var dp devpodv1alpha1.DevPod
		if err := k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Namespace: "devpods", Name: name}, &dp); err != nil {
			return err
		}
		dp.Status.Phase, dp.Status.ExitCode = phase, code
		return k8sClient.Status().Update(context.Background(), &dp)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var dp devpodv1alpha1.DevPod
		return k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "devpods", Name: name}, &dp) == nil && dp.Status.Phase == phase
	})
}

// roundUntil runs rounds until the job is in phase.
func roundUntil(t *testing.T, p *placeRig, id string, phase devpodv1alpha1.JobPhase) devpodv1alpha1.Job {
	t.Helper()
	var j devpodv1alpha1.Job
	waitFor(t, func() bool {
		p.s.QueueRound(context.Background())
		j = jobNow(t, id)
		return j.Status.Phase == phase
	})
	return j
}

func gpuMachine(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	var n corev1.Node
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[devpodv1alpha1.ReservableNodeLabel] = "true"
	if err := k8sClient.Update(ctx, &n); err != nil {
		t.Fatal(err)
	}
	n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("192"), gpures.NVIDIA: resource.MustParse("8")}
	n.Status.Capacity = n.Status.Allocatable
	if err := k8sClient.Status().Update(ctx, &n); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var got corev1.Node
		q := got.Status.Allocatable[gpures.NVIDIA]
		_ = q
		return k8sClient.Get(ctx, types.NamespacedName{Name: name}, &got) == nil && got.Labels[devpodv1alpha1.ReservableNodeLabel] == "true" && len(got.Status.Allocatable) == 2
	})
}

// A job starts on a free machine, runs, and ends with its ranks.
func TestQueueStartsAndSucceeds(t *testing.T) {
	p, _ := queueRig(t, "qs", "qs-alice")
	id := submit(t, p, "qs-alice", 1, 4, nil)
	j := roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranks := ranksBecome(t, id, 1)
	r := ranks[0]
	if r.Name != "qs-alice-"+id+"-0" || r.Spec.Job == nil || r.Spec.Pod.Spec.NodeSelector["kubernetes.io/hostname"] != p.n1 || len(j.Status.Ranks) != 1 || j.Status.Attempts != 1 {
		t.Fatalf("rank %s %+v job %+v", r.Name, r.Spec.Job, j.Status)
	}
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	if j := roundUntil(t, p, id, devpodv1alpha1.JobRunning); j.Status.StartedAt == nil {
		t.Fatal("no StartedAt")
	}
	zero := int32(0)
	setRank(t, r.Name, devpodv1alpha1.DevPodSucceeded, &zero)
	j = roundUntil(t, p, id, devpodv1alpha1.JobSucceeded)
	if j.Status.FinishedAt == nil || j.Status.Ranks[0].ExitCode == nil || *j.Status.Ranks[0].ExitCode != 0 {
		t.Fatalf("ended: %+v", j.Status)
	}
	ranksBecome(t, id, 0)
}

// One rank failing ends the job, and takes the others with it.
func TestQueueFailedRankEndsJob(t *testing.T) {
	p, _ := queueRig(t, "qf", "qf-alice")
	gpuMachine(t, p.n2)
	id := submit(t, p, "qf-alice", 2, 4, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranks := ranksBecome(t, id, 2)
	for _, r := range ranks {
		setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	}
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	three := int32(3)
	setRank(t, "qf-alice-"+id+"-1", devpodv1alpha1.DevPodFailed, &three)
	j := roundUntil(t, p, id, devpodv1alpha1.JobFailed)
	if j.Status.Detail["rank"] != "1" || j.Status.Detail["exit"] != "3" {
		t.Fatalf("detail %v", j.Status.Detail)
	}
	ranksBecome(t, id, 0)
}

// A job out of time is ended.
func TestQueueTimeLimit(t *testing.T) {
	p, clock := queueRig(t, "qt", "qt-alice")
	id := submit(t, p, "qt-alice", 1, 1, map[string]any{"timeLimitSeconds": 3600})
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	r := ranksBecome(t, id, 1)[0]
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	*clock = clock.Add(61 * time.Minute)
	roundUntil(t, p, id, devpodv1alpha1.JobTimedOut)
	ranksBecome(t, id, 0)
}

// Ranks that do not all run within the gang timeout are taken back.
func TestQueueGangTimeout(t *testing.T) {
	p, clock := queueRig(t, "qg", "qg-alice")
	id := submit(t, p, "qg-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranksBecome(t, id, 1)
	*clock = clock.Add(6 * time.Minute)
	waitFor(t, func() bool {
		p.s.QueueRound(context.Background())
		j := jobNow(t, id)
		return j.Status.Reason == "gang-timeout" && j.Status.Phase != devpodv1alpha1.JobStarting && j.Status.Attempts == 1
	})
}

// A higher job stops a lower preemptible one, which waits again.
func TestQueuePreempts(t *testing.T) {
	p, _ := queueRig(t, "qp", "qp-alice")
	priorityRules(t, map[string]devpodv1alpha1.JobPriorityRuleSpec{"qp-alice": {Condition: "user = qp-alice", MaxPriority: 100}})
	low := submit(t, p, "qp-alice", 1, 8, map[string]any{"preemptible": true})
	roundUntil(t, p, low, devpodv1alpha1.JobStarting)
	setRank(t, ranksBecome(t, low, 1)[0].Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, low, devpodv1alpha1.JobRunning)
	high := submit(t, p, "qp-alice", 1, 8, map[string]any{"priority": 100})
	j := roundUntil(t, p, low, devpodv1alpha1.JobQueued)
	if j.Status.Reason != "preempted" || j.Status.Preemptions != 1 {
		t.Fatalf("low: %+v", j.Status)
	}
	ranksBecome(t, low, 0)
	roundUntil(t, p, high, devpodv1alpha1.JobStarting)
}

// A rank stopped by hand ends its job.
func TestQueueRankStoppedByHand(t *testing.T) {
	p, _ := queueRig(t, "qh", "qh-alice")
	id := submit(t, p, "qh-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	r := ranksBecome(t, id, 1)[0]
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	p.patch("qh-alice", r.Name, `{"running":false}`, http.StatusOK)
	j := roundUntil(t, p, id, devpodv1alpha1.JobFailed)
	if j.Status.Reason != "rank-stopped" {
		t.Fatalf("reason %q", j.Status.Reason)
	}
}

// A console coming back (no memory of its own) starts nothing twice.
func TestQueueNoDoubleStart(t *testing.T) {
	p, clock := queueRig(t, "qd", "qd-alice")
	id := submit(t, p, "qd-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranksBecome(t, id, 1)
	again, _ := newServer(t)
	queueOn(again)
	again.Queue.Now = func() time.Time { return *clock }
	again.QueueRound(context.Background())
	again.QueueRound(context.Background())
	if n := len(rankDevPods(t, id)); n != 1 {
		t.Fatalf("%d ranks", n)
	}
	if j := jobNow(t, id); j.Status.Phase != devpodv1alpha1.JobStarting || j.Status.Attempts != 1 {
		t.Fatalf("job %+v", j.Status)
	}
}

// Before submitting, a dry run says where the job would stand.
func TestJobDryRun(t *testing.T) {
	p, _ := queueRig(t, "qy", "qy-alice")
	dry := func() map[string]any {
		t.Helper()
		b := `{"from":{"devpod":"qy-alice-dev1"},"nodes":1,"gpus":8,"cpus":4,"command":"true","timeLimitSeconds":3600,"dryRun":true}`
		rec := doJSON(t, p.s.HandleSubmitJobForTest(), "POST", "/api/jobs", nil, p.as("qy-alice"), b)
		var out map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
		}
		return out
	}
	if out := dry(); out["startsNow"] != true {
		t.Fatalf("free machine: %v", out)
	}
	id := submit(t, p, "qy-alice", 1, 8, map[string]any{"timeLimitSeconds": 7200})
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	if out := dry(); out["position"] != float64(1) || out["expectedStart"] == nil || out["reason"] != "capacity" {
		t.Fatalf("machine taken for 2h: %v", out)
	}
	if len(listJobs(t, p.s, p.as("qy-alice"), "?mine=1")) != 1 {
		t.Fatal("a dry run made a job")
	}
}
