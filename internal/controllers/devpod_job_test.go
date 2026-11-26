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
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// waitPod: the rendered Pod of dp, once there.
func waitPod(t *testing.T, env *testEnv, dp *devpodv1alpha1.DevPod) corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var pod corev1.Pod
		if env.Client.Get(env.Ctx, clientKey(dp), &pod) == nil {
			return pod
		}
		if time.Now().After(deadline) {
			t.Fatal("pod never appeared")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// endPod: the Pod's command ended with code (the kubelet's part).
func endPod(t *testing.T, env *testEnv, pod *corev1.Pod, phase corev1.PodPhase, code int32) {
	t.Helper()
	patch := client.MergeFrom(pod.DeepCopy())
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "dev", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, FinishedAt: metav1.Now(), Reason: "Completed"}}}}
	if err := env.Client.Status().Patch(env.Ctx, pod, patch); err != nil {
		t.Fatal(err)
	}
}

// devPodBecomes polls dp until ok.
func devPodBecomes(t *testing.T, env *testEnv, dp *devpodv1alpha1.DevPod, what string, ok func(*devpodv1alpha1.DevPod) bool) devpodv1alpha1.DevPod {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var got devpodv1alpha1.DevPod
		if env.Client.Get(env.Ctx, clientKey(dp), &got) == nil && ok(&got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: phase %q exit %v message %q", what, got.Status.Phase, got.Status.ExitCode, got.Status.Message)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func ended(phase devpodv1alpha1.DevPodPhase, code int32) func(*devpodv1alpha1.DevPod) bool {
	return func(d *devpodv1alpha1.DevPod) bool {
		return d.Status.Phase == phase && d.Status.ExitCode != nil && *d.Status.ExitCode == code && d.Status.FinishedAt != nil
	}
}

// A job rank whose command ends with 0 is Succeeded, and its Pod stays.
func TestJobRankSucceeded(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	dp := jobDevPod("js-a-0", "js", "a1b2c", 0, "true")
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatal(err)
	}
	pod := waitPod(t, env, dp)
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy %q", pod.Spec.RestartPolicy)
	}
	endPod(t, env, &pod, corev1.PodSucceeded, 0)
	devPodBecomes(t, env, dp, "Succeeded", ended(devpodv1alpha1.DevPodSucceeded, 0))
	time.Sleep(2 * time.Second) // more reconciles
	var still corev1.Pod
	if err := env.Client.Get(env.Ctx, clientKey(dp), &still); err != nil || still.UID != pod.UID {
		t.Fatalf("the finished Pod went: %v", err)
	}
	devPodBecomes(t, env, dp, "still Succeeded", ended(devpodv1alpha1.DevPodSucceeded, 0))
}

// A failed job rank is Failed with its code — not deleted, not tried
// again; gone, it is not made again.
func TestJobRankFailedNotRecreated(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	dp := jobDevPod("jx-a-0", "jx", "a1b2c", 0, "false")
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatal(err)
	}
	pod := waitPod(t, env, dp)
	endPod(t, env, &pod, corev1.PodFailed, 137)
	got := devPodBecomes(t, env, dp, "Failed", ended(devpodv1alpha1.DevPodFailed, 137))
	if got.Status.RetryCount != 0 {
		t.Errorf("retryCount %d", got.Status.RetryCount)
	}
	time.Sleep(2 * time.Second)
	var still corev1.Pod
	if err := env.Client.Get(env.Ctx, clientKey(dp), &still); err != nil || still.UID != pod.UID {
		t.Fatalf("the failed Pod went: %v", err)
	}
	if err := env.Client.Delete(env.Ctx, &still); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	var again corev1.Pod
	if err := env.Client.Get(env.Ctx, clientKey(dp), &again); err == nil && again.UID != still.UID && again.DeletionTimestamp == nil {
		t.Fatal("a new Pod was made for a finished rank")
	}
	devPodBecomes(t, env, dp, "still Failed", ended(devpodv1alpha1.DevPodFailed, 137))
}

// A job rank with nothing to run fails at once, with no Pod.
func TestJobRankWithoutCommand(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	dp := jobDevPod("jn-a-0", "jn", "a1b2c", 0)
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatal(err)
	}
	devPodBecomes(t, env, dp, "Failed for no command", func(d *devpodv1alpha1.DevPod) bool {
		return d.Status.Phase == devpodv1alpha1.DevPodFailed && strings.Contains(d.Status.Message, "command")
	})
	var pod corev1.Pod
	if err := env.Client.Get(env.Ctx, clientKey(dp), &pod); err == nil {
		t.Fatal("a Pod was made")
	}
}

// A job rank takes no Pod it did not make: one of the same name left by a
// former start of its job is waited out — its end is not the rank's.
func TestJobRankWaitsForAnotherPod(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	stray := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "jw-a-0", Namespace: "devpods"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}
	if err := env.Client.Create(env.Ctx, stray); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Client.Delete(env.Ctx, stray) })
	patch := client.MergeFrom(stray.DeepCopy())
	stray.Status.Phase, stray.Status.PodIP = corev1.PodRunning, "10.0.0.9"
	if err := env.Client.Status().Patch(env.Ctx, stray, patch); err != nil {
		t.Fatal(err)
	}
	dp := jobDevPod("jw-a-0", "jw", "a1b2c", 0, "true")
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	endPod(t, env, stray, corev1.PodFailed, 143)
	time.Sleep(2 * time.Second)
	var got devpodv1alpha1.DevPod
	if err := env.Client.Get(env.Ctx, clientKey(dp), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == devpodv1alpha1.DevPodRunning || got.Status.Phase == devpodv1alpha1.DevPodFailed || got.Status.ExitCode != nil {
		t.Fatalf("took the stray Pod: phase %q exit %v", got.Status.Phase, got.Status.ExitCode)
	}
	if err := env.Client.Delete(env.Ctx, stray, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var pod corev1.Pod
		if env.Client.Get(env.Ctx, clientKey(dp), &pod) == nil && metav1.IsControlledBy(&pod, &got) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rank never made its own Pod")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A job rank whose Pod goes away before it ends has failed: its command
// is never run a second time.
func TestJobRankPodGoneFails(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	dp := jobDevPod("jv-a-0", "jv", "a1b2c", 0, "true")
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatal(err)
	}
	pod := waitPod(t, env, dp)
	devPodBecomes(t, env, dp, "its Pod seen", func(d *devpodv1alpha1.DevPod) bool { return d.Status.WorkloadRef != nil })
	if err := env.Client.Delete(env.Ctx, &pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	devPodBecomes(t, env, dp, "Failed for its Pod gone", func(d *devpodv1alpha1.DevPod) bool {
		return d.Status.Phase == devpodv1alpha1.DevPodFailed && strings.Contains(d.Status.Message, "went away") && d.Status.FinishedAt != nil
	})
	time.Sleep(2 * time.Second)
	var again corev1.Pod
	if err := env.Client.Get(env.Ctx, clientKey(dp), &again); err == nil && again.UID != pod.UID && again.DeletionTimestamp == nil {
		t.Fatal("a new Pod was made")
	}
}
