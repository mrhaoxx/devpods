// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestSnapshotReconciler_FailsForNonexistentDevPod(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	snap := &devpodv1alpha1.DevPodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-snap", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSnapshotSpec{
			DevPodName: "does-not-exist",
			Image:      "registry.example.com/test:v1",
		},
	}
	if err := env.Client.Create(env.Ctx, snap); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPodSnapshot
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "bad-snap", Namespace: "devpods"}, &got); err == nil {
			if got.Status.Phase == devpodv1alpha1.SnapshotFailed {
				if got.Status.Message == "" {
					t.Fatal("expected non-empty failure message")
				}
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("snapshot never transitioned to Failed for missing DevPod")
}

func TestSnapshotReconciler_FailsForNonRunningDevPod(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "snapuser2"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA snap2"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "snapdp2", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "snapuser2",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "dev",
						Image:   "ubuntu:24.04",
						Command: []string{"sleep", "infinity"},
					}},
				},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	// Wait for controller to create Pod, but in envtest the Pod
	// status.phase stays at "" (no kubelet), and DevPod status won't
	// reach Running. The snapshot should fail.
	time.Sleep(3 * time.Second)

	snap := &devpodv1alpha1.DevPodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "nonrun-snap", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSnapshotSpec{
			DevPodName: "snapdp2",
			Image:      "registry.example.com/test:v2",
		},
	}
	if err := env.Client.Create(env.Ctx, snap); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPodSnapshot
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "nonrun-snap", Namespace: "devpods"}, &got); err == nil {
			if got.Status.Phase == devpodv1alpha1.SnapshotFailed {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("snapshot never transitioned to Failed for non-running DevPod")
}
