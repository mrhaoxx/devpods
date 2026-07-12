// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestUserReconciler_BlocksDeleteUntilDevPodsRemoved(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "carol"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA carol"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "carol-data-dev", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "carol",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	// Wait until finalizer is present on user (set by UserReconciler).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var u devpodv1alpha1.User
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "carol"}, &u); err == nil {
			if len(u.Finalizers) > 0 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Request delete of user; should NOT vanish because DevPod still exists.
	if err := env.Client.Delete(env.Ctx, user); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	time.Sleep(2 * time.Second)
	var stillThere devpodv1alpha1.User
	if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "carol"}, &stillThere); err != nil {
		t.Fatalf("user vanished while DevPod still exists: %v", err)
	}
	if stillThere.DeletionTimestamp.IsZero() {
		t.Errorf("expected DeletionTimestamp set, got nil")
	}

	// Remove the DevPod.
	if err := env.Client.Delete(env.Ctx, dp); err != nil {
		t.Fatalf("delete devpod: %v", err)
	}

	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var u devpodv1alpha1.User
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "carol"}, &u); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("user not garbage-collected after DevPod removal")
}
