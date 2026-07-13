// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gateway"
)

func devpod(name, owner string) *devpodv1alpha1.DevPod {
	return &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
		Spec:       devpodv1alpha1.DevPodSpec{Owner: owner},
	}
}

// The recorder must be given the RESOLVED, owner-scoped DevPod name
// ("<owner>-<pod>"). Given that, it creates the Event; given the bare
// pod segment, its DevPod lookup misses and no Event is created — the
// regression that dropped every SSH session event under owner-scoped
// naming.
func TestEventRecorder_ResolvedNameCreatesEvent(t *testing.T) {
	reader := fakeClient(t, devpod("star-920b", "star"))
	cs := fake.NewSimpleClientset()
	rec := gateway.NewEventRecorder(reader, cs, "devpods")

	rec.SessionConnected(context.Background(), "star-920b", "star", "1.2.3.4:5", "direct")

	events, err := cs.CoreV1().Events("devpods").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("want 1 event, got %d", len(events.Items))
	}
	e := events.Items[0]
	if e.Reason != "SessionConnected" || e.InvolvedObject.Name != "star-920b" || e.InvolvedObject.Kind != "DevPod" {
		t.Fatalf("wrong event: reason=%q involved=%s/%s", e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name)
	}
}

func TestEventRecorder_BarePodNameDropsEvent(t *testing.T) {
	reader := fakeClient(t, devpod("star-920b", "star"))
	cs := fake.NewSimpleClientset()
	rec := gateway.NewEventRecorder(reader, cs, "devpods")

	// The pre-fix caller passed the bare "920b" — DevPod lookup misses.
	rec.SessionConnected(context.Background(), "920b", "star", "1.2.3.4:5", "direct")

	events, _ := cs.CoreV1().Events("devpods").List(context.Background(), metav1.ListOptions{})
	if len(events.Items) != 0 {
		t.Fatalf("bare name must not create an event, got %d", len(events.Items))
	}
}

func TestEventRecorder_NilSafe(t *testing.T) {
	var rec *gateway.EventRecorder
	// Must not panic when disabled.
	rec.SessionConnected(context.Background(), "x", "y", "z", "direct")
}
