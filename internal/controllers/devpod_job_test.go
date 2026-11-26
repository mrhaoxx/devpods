// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// jobDevPod: rank rank of job name, owned by owner, running cmd.
func jobDevPod(name, owner, job string, rank int32, cmd ...string) *devpodv1alpha1.DevPod {
	return &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   owner,
			Running: true,
			Job:     &devpodv1alpha1.DevPodJob{Name: job, Rank: rank},
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x", Command: cmd}}},
			},
		},
	}
}

// A DevPod's job never changes; how it ended is its status.
func TestDevPodJobFieldImmutable(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	dp := jobDevPod("jf-a-0", "jf", "a1b2c", 0, "true")
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create: %v", err)
	}
	// seen first (the client reads through a cache)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var seen devpodv1alpha1.DevPod
		if env.Client.Get(env.Ctx, clientKey(dp), &seen) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the DevPod never reached the cache")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// the rank cannot change (re-read on a conflict: the controller writes too)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var moved devpodv1alpha1.DevPod
		if err := env.Client.Get(env.Ctx, clientKey(dp), &moved); err != nil {
			return err
		}
		moved.Spec.Job.Rank = 1
		return env.Client.Update(env.Ctx, &moved)
	})
	if err == nil || !strings.Contains(err.Error(), "job") {
		t.Fatalf("changing the rank: %v", err)
	}
	// the phase and exit code are accepted (the controller writes too: retried)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var got devpodv1alpha1.DevPod
		if err := env.Client.Get(env.Ctx, clientKey(dp), &got); err != nil {
			return err
		}
		code := int32(3)
		got.Status.Phase = devpodv1alpha1.DevPodSucceeded
		got.Status.ExitCode = &code
		now := metav1.Now()
		got.Status.FinishedAt = &now
		return env.Client.Status().Update(env.Ctx, &got)
	})
	if err != nil {
		t.Fatalf("status Succeeded: %v", err)
	}
}

func clientKey(dp *devpodv1alpha1.DevPod) types.NamespacedName {
	return types.NamespacedName{Namespace: dp.Namespace, Name: dp.Name}
}
