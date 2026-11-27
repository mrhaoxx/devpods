// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

// A job rank whose Pod ended between the reconcile's look at the Pod and
// its status update is recorded as jobFinished records it — its exit code
// and when — not as a bare Failed that nothing would complete (an ended
// rank is never looked at again).
func TestUpdateStatusRecordsAnEndedRank(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := devpodv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "jr-a-0", Namespace: "devpods", UID: "u1"},
		Spec: devpodv1alpha1.DevPodSpec{Owner: "jr", Running: true, Job: &devpodv1alpha1.DevPodJob{Name: "a1b2c"},
			Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x", Command: []string{"false"}}}}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: render.PodName(dp), Namespace: "devpods"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: "dev",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, FinishedAt: metav1.Now()}}}}}}
	if err := controllerutil.SetControllerReference(dp, pod, s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(dp, pod).WithStatusSubresource(&devpodv1alpha1.DevPod{}).Build()
	r := &DevPodReconciler{Client: c, Scheme: s, GwConfig: &devpodv1alpha1.GatewayConfig{Spec: devpodv1alpha1.GatewayConfigSpec{DevPodNamespace: "devpods"}}}
	ctx := context.Background()
	if err := r.updateStatus(ctx, dp); err != nil {
		t.Fatal(err)
	}
	var got devpodv1alpha1.DevPod
	if err := c.Get(ctx, client.ObjectKeyFromObject(dp), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != devpodv1alpha1.DevPodFailed || got.Status.ExitCode == nil || *got.Status.ExitCode != 137 || got.Status.FinishedAt == nil {
		t.Fatalf("phase %q exit %v finished %v", got.Status.Phase, got.Status.ExitCode, got.Status.FinishedAt)
	}
}
