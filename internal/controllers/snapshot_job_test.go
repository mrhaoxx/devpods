// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/controllers"
	"github.com/mrhaoxx/devpod/internal/render"
)

// snapshotRig: a running DevPod whose container the kubelet reports with
// containerID, and a snapshot of it, on a fake client (no kubelet needed).
func snapshotRig(t *testing.T, containerID string) (*controllers.DevPodSnapshotReconciler, client.Client, ctrl.Request) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := devpodv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-dev", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{Owner: "alice", Running: true, Pod: &devpodv1alpha1.PodWorkloadSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04"}}},
		}},
		Status: devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: render.PodName(dp), Namespace: "devpods"},
		Spec:       corev1.PodSpec{NodeName: "node-7", Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "dev", ContainerID: containerID}}},
	}
	secret := "push-creds"
	snap := &devpodv1alpha1.DevPodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSnapshotSpec{DevPodName: "alice-dev", Image: "reg.test/alice/img:v1", Push: true,
			PushSecretRef: &devpodv1alpha1.LocalObjectRef{Name: secret}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dp, pod, snap).
		WithStatusSubresource(&devpodv1alpha1.DevPodSnapshot{}).Build()
	r := &controllers.DevPodSnapshotReconciler{Client: c, Scheme: scheme, SnapshotImage: "reg/devpod-snapshot:v1", DockerImage: "reg/docker-cli:v1", ContainerdSocket: "/run/containerd/containerd.sock"}
	return r, c, ctrl.Request{NamespacedName: types.NamespacedName{Name: "s1", Namespace: "devpods"}}
}

// finishJob plays the kubelet and the Job controller: the Job's pod
// terminated with msg, and the Job reached cond.
func finishJob(t *testing.T, c client.Client, cond batchv1.JobConditionType, msg string) {
	t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	if err := c.Get(ctx, types.NamespacedName{Name: "snapshot-s1", Namespace: "devpods"}, &job); err != nil {
		t.Fatal(err)
	}
	jobPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-s1-abcde", Namespace: "devpods", Labels: map[string]string{"job-name": job.Name}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: render.SnapshotContainer, Image: "x"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: render.SnapshotContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}}}}},
	}
	if err := c.Create(ctx, jobPod); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded"}}
	if err := c.Status().Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
}

func getSnap(t *testing.T, c client.Client) devpodv1alpha1.DevPodSnapshot {
	t.Helper()
	var s devpodv1alpha1.DevPodSnapshot
	if err := c.Get(context.Background(), types.NamespacedName{Name: "s1", Namespace: "devpods"}, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshot_ContainerdJobAndResult(t *testing.T) {
	ctx := context.Background()
	r, c, req := snapshotRig(t, "containerd://abc123")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := c.Get(ctx, types.NamespacedName{Name: "snapshot-s1", Namespace: "devpods"}, &job); err != nil {
		t.Fatalf("job: %v", err)
	}
	ps := job.Spec.Template.Spec
	env := map[string]string{}
	for _, e := range ps.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if ps.NodeName != "node-7" || ps.Containers[0].Image != "reg/devpod-snapshot:v1" || env["RUNTIME"] != "containerd" ||
		env["CONTAINER_ID"] != "abc123" || env["TARGET_IMAGE"] != "reg.test/alice/img:v1" || env["DOCKER_CONFIG"] == "" {
		t.Fatalf("job node %q image %q env %v", ps.NodeName, ps.Containers[0].Image, env)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Name != "s1" || job.OwnerReferences[0].Controller == nil || !*job.OwnerReferences[0].Controller {
		t.Fatalf("the Job must be owned by the snapshot so deleting it cancels: %v", job.OwnerReferences)
	}
	s := getSnap(t, c)
	if s.Status.Phase != devpodv1alpha1.SnapshotRunning || s.Status.StartedAt == nil || s.Status.CompletedAt != nil || s.Status.JobRef == nil {
		t.Fatalf("running status %+v", s.Status)
	}

	finishJob(t, c, batchv1.JobComplete, "digest=sha256:feedface size=123456789\n")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	s = getSnap(t, c)
	if s.Status.Phase != devpodv1alpha1.SnapshotSucceeded || s.Status.Digest != "sha256:feedface" || s.Status.SizeBytes != 123456789 || s.Status.CompletedAt == nil {
		t.Fatalf("succeeded status %+v", s.Status)
	}
}

func TestSnapshot_FailureCarriesTheScriptsReason(t *testing.T) {
	ctx := context.Background()
	r, c, req := snapshotRig(t, "containerd://abc123")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	finishJob(t, c, batchv1.JobFailed, "error=push failed\n")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	s := getSnap(t, c)
	if s.Status.Phase != devpodv1alpha1.SnapshotFailed || s.Status.Message != "push failed" || s.Status.CompletedAt == nil {
		t.Fatalf("failed status %+v", s.Status)
	}
}

func TestSnapshot_FailureWithoutAResultFallsBackToTheJob(t *testing.T) {
	ctx := context.Background()
	r, c, req := snapshotRig(t, "containerd://abc123")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	finishJob(t, c, batchv1.JobFailed, "")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if s := getSnap(t, c); s.Status.Phase != devpodv1alpha1.SnapshotFailed || s.Status.Message != "snapshot Job failed: BackoffLimitExceeded" {
		t.Fatalf("status %+v", s.Status)
	}
}

func TestSnapshot_DockerNodeUsesTheDockerImage(t *testing.T) {
	ctx := context.Background()
	r, c, req := snapshotRig(t, "docker://def456")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := c.Get(ctx, types.NamespacedName{Name: "snapshot-s1", Namespace: "devpods"}, &job); err != nil {
		t.Fatal(err)
	}
	cont := job.Spec.Template.Spec.Containers[0]
	if cont.Image != "reg/docker-cli:v1" || job.Spec.Template.Spec.Volumes[0].HostPath.Path != "/var/run/docker.sock" {
		t.Fatalf("docker job image %q volumes %v", cont.Image, job.Spec.Template.Spec.Volumes)
	}
}

func TestSnapshot_UnknownRuntimeFails(t *testing.T) {
	ctx := context.Background()
	r, c, req := snapshotRig(t, "cri-o://xyz")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if s := getSnap(t, c); s.Status.Phase != devpodv1alpha1.SnapshotFailed || s.Status.Message == "" {
		t.Fatalf("status %+v", s.Status)
	}
}

// Saving to an address again overwrites it: once the new snapshot
// succeeds, the older finished records of that address (same owner) go;
// other addresses, other owners and running ones stay.
func TestSnapshot_SuccessDropsOlderRecordsOfTheSameAddress(t *testing.T) {
	ctx := context.Background()
	r, c, req := snapshotRig(t, "containerd://abc123")
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	mk := func(name, image, owner string, phase devpodv1alpha1.DevPodSnapshotPhase) {
		sn := &devpodv1alpha1.DevPodSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods", CreationTimestamp: old, Labels: map[string]string{render.LabelOwner: owner}},
			Spec:       devpodv1alpha1.DevPodSnapshotSpec{DevPodName: "alice-dev", Image: image, Push: true},
		}
		if err := c.Create(ctx, sn); err != nil {
			t.Fatal(err)
		}
		sn.Status.Phase = phase
		if err := c.Status().Update(ctx, sn); err != nil {
			t.Fatal(err)
		}
	}
	// the snapshot under test belongs to alice
	cur := getSnap(t, c)
	cur.Labels = map[string]string{render.LabelOwner: "alice"}
	cur.CreationTimestamp = metav1.Now() // the fake client leaves it zero
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	mk("old-ok", "reg.test/alice/img:v1", "alice", devpodv1alpha1.SnapshotSucceeded)
	mk("old-failed", "reg.test/alice/img:v1", "alice", devpodv1alpha1.SnapshotFailed)
	mk("other-tag", "reg.test/alice/img:v2", "alice", devpodv1alpha1.SnapshotSucceeded)
	mk("other-owner", "reg.test/alice/img:v1", "bob", devpodv1alpha1.SnapshotSucceeded)
	mk("still-running", "reg.test/alice/img:v1", "alice", devpodv1alpha1.SnapshotRunning)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	finishJob(t, c, batchv1.JobComplete, "digest=sha256:feedface size=1\n")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var list devpodv1alpha1.DevPodSnapshotList
	if err := c.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, sn := range list.Items {
		left = append(left, sn.Name)
	}
	sort.Strings(left)
	if got := strings.Join(left, ","); got != "other-owner,other-tag,s1,still-running" {
		t.Fatalf("left: %s", got)
	}
}
