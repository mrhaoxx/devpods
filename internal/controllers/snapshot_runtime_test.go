// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/controllers"
	"github.com/mrhaoxx/devpod/internal/render"
)

// These tests reconcile real API objects through a fake client. No kubeconfig,
// Kubernetes server, or container runtime is used.
func snapshotFixture(t *testing.T, containerID string) (*controllers.DevPodSnapshotReconciler, ctrl.Request) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := devpodv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "devpods"},
		Status:     devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: render.PodName(dp), Namespace: dp.Namespace},
		Spec:       corev1.PodSpec{NodeName: "source-node", Containers: []corev1.Container{{Name: "dev"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", ContainerID: "docker://wrong-container"},
			{Name: "dev", ContainerID: containerID},
		}},
	}
	snap := &devpodv1alpha1.DevPodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: dp.Namespace, UID: types.UID("snapshot-uid")},
		Spec: devpodv1alpha1.DevPodSnapshotSpec{DevPodName: dp.Name, Image: "registry.example.com/dev:snapshot", Push: true,
			PushSecretRef: &devpodv1alpha1.LocalObjectRef{Name: "creds"}},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&devpodv1alpha1.DevPod{}, &devpodv1alpha1.DevPodSnapshot{}, &corev1.Pod{}, &batchv1.Job{}).
		WithObjects(dp, pod, snap).Build()
	return &controllers.DevPodSnapshotReconciler{
		Client: c, Scheme: s,
		SnapshotOptions: render.SnapshotOptions{
			DockerImage: "docker:cli", ContainerdImage: "snapshot:nerdctl",
			ContainerdAddress: "/run/k3s/containerd/containerd.sock", ContainerdNamespace: "k8s.io",
		},
	}, ctrl.Request{NamespacedName: types.NamespacedName{Name: snap.Name, Namespace: snap.Namespace}}
}

func TestSnapshotRuntimeDispatch(t *testing.T) {
	for _, tc := range []struct{ id, image, runtimeName, failure string }{
		{id: "docker://full-container-id", image: "docker:cli", runtimeName: "docker"},
		{id: "containerd://full-container-id", image: "snapshot:nerdctl", runtimeName: "containerd"},
		{id: "cri-o://id", failure: "unsupported container runtime"},
		{id: "", failure: "has no containerID"},
		{id: "containerd://", failure: "expected runtime://id"},
		{id: "bare-id", failure: "expected runtime://id"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			r, req := snapshotFixture(t, tc.id)
			ctx := context.Background()
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			var snap devpodv1alpha1.DevPodSnapshot
			if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
				t.Fatal(err)
			}
			var jobs batchv1.JobList
			if err := r.List(ctx, &jobs); err != nil {
				t.Fatal(err)
			}
			if tc.failure != "" {
				if snap.Status.Phase != devpodv1alpha1.SnapshotFailed || !strings.Contains(snap.Status.Message, tc.failure) || len(jobs.Items) != 0 {
					t.Fatalf("want failure %q without a Job, got status=%+v jobs=%d", tc.failure, snap.Status, len(jobs.Items))
				}
				return
			}
			if snap.Status.Phase != devpodv1alpha1.SnapshotRunning || snap.Status.JobRef == nil || len(jobs.Items) != 1 {
				t.Fatalf("want Running with one Job, got status=%+v jobs=%d", snap.Status, len(jobs.Items))
			}
			job := jobs.Items[0]
			if job.Name != snap.Status.JobRef.Name || len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != snap.UID {
				t.Fatalf("Job ownership/reference not set: %+v", job.ObjectMeta)
			}
			pod := job.Spec.Template
			if pod.Spec.NodeName != "source-node" || pod.Spec.Containers[0].Image != tc.image || pod.Labels[render.LabelSnapshotRuntime] != tc.runtimeName {
				t.Fatalf("wrong Job runtime/image/node: %+v", pod)
			}
			var id string
			for _, e := range pod.Spec.Containers[0].Env {
				if e.Name == "CONTAINER_ID" {
					id = e.Value
				}
			}
			if id != "full-container-id" {
				t.Fatalf("wrong container selected: %q", id)
			}
			// Reconciliation must reuse the existing Job.
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := r.List(ctx, &jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 1 {
				t.Fatal("duplicate snapshot Job")
			}
		})
	}
}

func TestSnapshotRuntimeResult(t *testing.T) {
	for _, tc := range []struct {
		name      string
		condition batchv1.JobConditionType
		message   string
		phase     devpodv1alpha1.DevPodSnapshotPhase
	}{
		{name: "success", condition: batchv1.JobComplete, message: "sha256:manifest\n", phase: devpodv1alpha1.SnapshotSucceeded},
		{name: "failed-push", condition: batchv1.JobFailed, message: "nerdctl push: registry returned 503", phase: devpodv1alpha1.SnapshotFailed},
		{name: "no-termination-message", condition: batchv1.JobFailed, phase: devpodv1alpha1.SnapshotFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, req := snapshotFixture(t, "containerd://full-id")
			ctx := context.Background()
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			var snap devpodv1alpha1.DevPodSnapshot
			if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
				t.Fatal(err)
			}
			var job batchv1.Job
			if err := r.Get(ctx, types.NamespacedName{Name: snap.Status.JobRef.Name, Namespace: snap.Namespace}, &job); err != nil {
				t.Fatal(err)
			}
			job.Status.Conditions = []batchv1.JobCondition{{Type: tc.condition, Status: corev1.ConditionTrue, Message: "Job condition fallback"}}
			if err := r.Status().Update(ctx, &job); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot-pod", Namespace: snap.Namespace, Labels: map[string]string{"job-name": job.Name}},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "snapshot", State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Message: tc.message},
				}}}},
			}
			if err := r.Create(ctx, pod); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
				t.Fatal(err)
			}
			if snap.Status.Phase != tc.phase {
				t.Fatalf("phase=%s, want %s", snap.Status.Phase, tc.phase)
			}
			if tc.phase == devpodv1alpha1.SnapshotSucceeded {
				if snap.Status.Digest != "sha256:manifest" {
					t.Fatalf("digest=%q", snap.Status.Digest)
				}
			} else {
				want := tc.message
				if want == "" {
					want = "Job condition fallback"
				}
				if !strings.Contains(snap.Status.Message, want) {
					t.Fatalf("message=%q, want %q", snap.Status.Message, want)
				}
			}
		})
	}
}
