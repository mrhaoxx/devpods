// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// makeDevPod creates owner's DevPod name as given (running, pinned to
// node unless "", with annotations) straight in the cluster, and waits
// for the cache to hold it.
func makeDevPod(t *testing.T, name, owner, node string, running bool, ann map[string]string) {
	t.Helper()
	ctx := context.Background()
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}}}}
	if node != "" {
		spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
	}
	dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods", Annotations: ann},
		Spec: devpodv1alpha1.DevPodSpec{Owner: owner, Running: running, Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: spec}}}
	if err := k8sClient.Create(ctx, dp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, dp) })
	waitDevPod(t, name)
}

// Running though it still carries a mark of the line, a stop stops it
// (not only takes the mark off).
func TestStopRunningWithStaleMarkStops(t *testing.T) {
	p := jobRig(t, "mq", "")
	makeDevPod(t, "mq-alice-x", "mq-alice", "", true, map[string]string{
		devpodv1alpha1.QueuedAtAnnotation: time.Now().UTC().Format(time.RFC3339Nano), devpodv1alpha1.QueuePriorityAnnotation: "0"})
	if code, _, ec := patchAs(t, p, "mq-alice", "mq-alice-x", `{"running":false}`); code != http.StatusOK {
		t.Fatalf("stop: %d %q", code, ec)
	}
	devPodWhen(t, p, "mq-alice-x", "stopped, out of line", func(dp *devpodv1alpha1.DevPod) bool { return !dp.Spec.Running && !queuedNow(*dp) })
}

// The queue turned off, a DevPod left in line starts when started: there
// is no line to wait in.
func TestQueueOffStartsDevPodLeftInLine(t *testing.T) {
	p := jobRig(t, "mo", "")
	name := p.create("mo-alice", http.StatusCreated)
	if dp := p.get(name); dp.Spec.Running || !queuedNow(dp) {
		t.Fatalf("not in line: running %v annotations %v", dp.Spec.Running, dp.Annotations)
	}
	p.s.Queue = nil
	if code, resp, ec := patchAs(t, p, "mo-alice", name, `{"running":true}`); code != http.StatusOK || resp.Queued {
		t.Fatalf("start: %d %q queued %v", code, ec, resp.Queued)
	}
	devPodWhen(t, p, name, "started, out of line", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running && !queuedNow(*dp) })
}

// Pinned by its user to the machine the queue gave it, the pin is the
// user's: it stays when the DevPod stops.
func TestPinToQueuesMachineMakesItUsers(t *testing.T) {
	p := newPlaceRig(t, "mp") // the queue off: pinned at its start
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	name := p.create("mp-alice", http.StatusCreated)
	dp := p.get(name)
	node := pinnedOf(&dp)
	if node == "" || dp.Annotations[devpodv1alpha1.AutoPinAnnotation] == "" {
		t.Fatalf("not pinned by the queue: %q %v", node, dp.Annotations)
	}
	if code, _, ec := patchAs(t, p, "mp-alice", name, `{"node":"`+node+`"}`); code != http.StatusOK {
		t.Fatalf("pin: %d %q", code, ec)
	}
	devPodWhen(t, p, name, "the user's pin", func(dp *devpodv1alpha1.DevPod) bool {
		return pinnedOf(dp) == node && dp.Annotations[devpodv1alpha1.AutoPinAnnotation] == ""
	})
	patchAs(t, p, "mp-alice", name, `{"running":false}`)
	if dp := devPodWhen(t, p, name, "stopped", func(dp *devpodv1alpha1.DevPod) bool { return !dp.Spec.Running }); pinnedOf(&dp) != node {
		t.Fatalf("the pin went at the stop: %q", pinnedOf(&dp))
	}
	eventsOf(t, f, name, "create", "pin", "stop")
}

// Back in line because its Pod would not schedule, a DevPod keeps the
// priority it waited at.
func TestUnschedulableRequeueKeepsPriority(t *testing.T) {
	p := jobRig(t, "mu", "")
	priorityRules(t, map[string]devpodv1alpha1.JobPriorityRuleSpec{"mu-alice": {Condition: "user = mu-alice", MaxPriority: 100}})
	name := p.create("mu-alice", http.StatusCreated)
	patchAs(t, p, "mu-alice", name, `{"running":false}`)
	devPodWhen(t, p, name, "out of line", func(dp *devpodv1alpha1.DevPod) bool { return !queuedNow(*dp) })
	if code, _, ec := patchAs(t, p, "mu-alice", name, `{"running":true,"priority":3}`); code != http.StatusOK {
		t.Fatalf("start at 3: %d %q", code, ec)
	}
	ctx := context.Background()
	p.s.QueueRound(ctx)
	devPodWhen(t, p, name, "started by the round", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running && pinnedOf(dp) != "" })
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "no", LastTransitionTime: metav1.NewTime(time.Now().Add(-6 * time.Minute))}}
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var got corev1.Pod
		return k8sClient.Get(ctx, client_key(name), &got) == nil && len(got.Status.Conditions) == 1
	})
	p.s.QueueRound(ctx)
	back := devPodWhen(t, p, name, "back in line", func(dp *devpodv1alpha1.DevPod) bool { return !dp.Spec.Running && queuedNow(*dp) })
	if got := back.Annotations[devpodv1alpha1.QueuePriorityAnnotation]; got != "3" {
		t.Fatalf("priority %q, want 3", got)
	}
}

// The queue off, a start over the quota where there is no room either is
// told of the quota (as with room).
func TestStartQueueOffOverQuotaSaysQuota(t *testing.T) {
	p, _ := clearRig(t, "mz")
	makeDevPod(t, "mz-alice-x", "mz-alice", "", false, nil)
	if code, v := reserveAs(t, p.s, p.as("mz-bob"), p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	if code, _, ec := patchAs(t, p, "mz-alice", "mz-alice-x", `{"gpu":"8"}`); code != http.StatusOK {
		t.Fatalf("8 cards, stopped: %d %q", code, ec)
	}
	p.s.DefaultQuota = devpodv1alpha1.UserQuota{Compute: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}
	if code, _, ec := patchAs(t, p, "mz-alice", "mz-alice-x", `{"running":true}`); code != http.StatusConflict || ec != "QUOTA_EXCEEDED" {
		t.Fatalf("start over the quota, no room: %d %q", code, ec)
	}
}

// Stopped, a DevPod its user pinned is no longer to be cleared: the note
// goes.
func TestStopClearsExpectClear(t *testing.T) {
	p := newPlaceRig(t, "me")
	makeDevPod(t, "me-alice-x", "me-alice", p.n2, true, map[string]string{devpodv1alpha1.ExpectClearAnnotation: "2026-11-12T03:00:00Z"})
	if code, _, ec := patchAs(t, p, "me-alice", "me-alice-x", `{"running":false}`); code != http.StatusOK {
		t.Fatalf("stop: %d %q", code, ec)
	}
	devPodWhen(t, p, "me-alice-x", "stopped, the note gone", func(dp *devpodv1alpha1.DevPod) bool {
		_, noted := dp.Annotations[devpodv1alpha1.ExpectClearAnnotation]
		return !dp.Spec.Running && !noted
	})
}

// Edited while in line to ask what no machine could ever give, a DevPod
// is refused as its start would be.
func TestEditQueuedNeverFitsRefused(t *testing.T) {
	p := jobRig(t, "mv", "")
	name := p.create("mv-alice", http.StatusCreated)
	if dp := p.get(name); !queuedNow(dp) {
		t.Fatalf("not in line: %v", dp.Annotations)
	}
	if code, _, ec := patchAs(t, p, "mv-alice", name, `{"gpu":"16"}`); code != http.StatusConflict || ec != "NO_MACHINE" {
		t.Fatalf("16 cards while in line: %d %q", code, ec)
	}
	if code, _, ec := patchAs(t, p, "mv-alice", name, `{"gpu":"2"}`); code != http.StatusOK {
		t.Fatalf("2 cards while in line: %d %q", code, ec)
	}
}
