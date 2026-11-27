// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func markDP() *devpodv1alpha1.DevPod {
	return &devpodv1alpha1.DevPod{Spec: devpodv1alpha1.DevPodSpec{Owner: "u", Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{}}}}
}

// A queued DevPod says since when, at which priority; the mark goes whole.
func TestMarkQueuedRoundTrip(t *testing.T) {
	dp := markDP()
	at := time.Date(2026, 11, 27, 3, 4, 5, 600, time.UTC)
	markQueued(dp, at, 7)
	if got, ok := queuedAt(dp); !ok || !got.Equal(at) || queuePriority(dp) != 7 {
		t.Fatalf("queued %v %v prio %d", got, ok, queuePriority(dp))
	}
	setQueueState(dp, queueState{Position: 2, Reason: "capacity"})
	clearQueued(dp)
	for _, k := range []string{devpodv1alpha1.QueuedAtAnnotation, devpodv1alpha1.QueuePriorityAnnotation, devpodv1alpha1.QueueStateAnnotation} {
		if _, has := dp.Annotations[k]; has {
			t.Fatalf("%s left", k)
		}
	}
	if _, ok := queuedAt(dp); ok {
		t.Fatal("still queued")
	}
}

// A state written again unchanged is no change.
func TestSetQueueStateChanged(t *testing.T) {
	dp := markDP()
	exp := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)
	st := queueState{Position: 3, Reason: "behind", Detail: map[string]string{"head": "abc12"}, Expected: &exp}
	if !setQueueState(dp, st) {
		t.Fatal("first write: no change")
	}
	if setQueueState(dp, st) {
		t.Fatal("same state: a change")
	}
	if got := queueStateOf(dp); got.Position != 3 || got.Detail["head"] != "abc12" || got.Expected == nil || !got.Expected.Equal(exp) {
		t.Fatalf("read back %+v", got)
	}
}

// The automatic pin is the queue's only while it is the DevPod's pin.
func TestAutoPinOnlyWhileItsPin(t *testing.T) {
	dp := markDP()
	since := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	setAutoPin(dp, "n1", since, 0)
	if node, at, ok := autoPin(dp); !ok || node != "n1" || !at.Equal(since) || pinnedNode(dp) != "n1" {
		t.Fatalf("auto pin %q %v %v (pin %q)", node, at, ok, pinnedNode(dp))
	}
	dp.Spec.Pod.Spec.NodeSelector[hostnameLabel] = "n2" // the user moved it
	if _, _, ok := autoPin(dp); ok {
		t.Fatal("the user's pin taken for the queue's")
	}
}

// A machine avoided is avoided until its time is up.
func TestAvoidingExpires(t *testing.T) {
	dp := markDP()
	now := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	avoidFor(dp, "n1", now.Add(time.Hour))
	if got := avoiding(dp, now); got != "n1" {
		t.Fatalf("avoiding %q", got)
	}
	if got := avoiding(dp, now.Add(2*time.Hour)); got != "" {
		t.Fatalf("still avoiding %q", got)
	}
}
