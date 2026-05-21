// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

func TestSnapshotJob_BasicFields(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-1"
	snap.Namespace = "devpods"
	snap.Spec.DevPodName = "my-devpod"
	snap.Spec.Image = "registry.example.com/repo:v1"

	job := render.SnapshotJob(snap, "abc123def", "node-1", nil)

	if job.Name != "snapshot-snap-1" {
		t.Errorf("name = %q, want snapshot-snap-1", job.Name)
	}
	if job.Namespace != "devpods" {
		t.Errorf("namespace = %q, want devpods", job.Namespace)
	}
	if *job.Spec.TTLSecondsAfterFinished != int32(300) {
		t.Errorf("ttl = %d, want 300", *job.Spec.TTLSecondsAfterFinished)
	}
	if *job.Spec.BackoffLimit != int32(0) {
		t.Errorf("backoffLimit = %d, want 0", *job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.NodeName != "node-1" {
		t.Errorf("nodeName = %q, want node-1", job.Spec.Template.Spec.NodeName)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestSnapshotJob_ContainerEnv(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-2"
	snap.Namespace = "devpods"
	snap.Spec.DevPodName = "dp"
	snap.Spec.Image = "reg/img:tag"

	job := render.SnapshotJob(snap, "containerid123", "n1", nil)
	c := job.Spec.Template.Spec.Containers[0]

	envMap := map[string]string{}
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["CONTAINER_ID"] != "containerid123" {
		t.Errorf("CONTAINER_ID = %q, want containerid123", envMap["CONTAINER_ID"])
	}
	if envMap["TARGET_IMAGE"] != "reg/img:tag" {
		t.Errorf("TARGET_IMAGE = %q, want reg/img:tag", envMap["TARGET_IMAGE"])
	}
}

func TestSnapshotJob_DockerSocket(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-3"
	snap.Namespace = "devpods"
	snap.Spec.Image = "reg:tag"

	job := render.SnapshotJob(snap, "cid", "n1", nil)

	found := false
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "docker-sock" && v.HostPath != nil && v.HostPath.Path == "/var/run/docker.sock" {
			found = true
		}
	}
	if !found {
		t.Error("docker-sock volume not found or misconfigured")
	}

	c := job.Spec.Template.Spec.Containers[0]
	mountFound := false
	for _, m := range c.VolumeMounts {
		if m.Name == "docker-sock" && m.MountPath == "/var/run/docker.sock" {
			mountFound = true
		}
	}
	if !mountFound {
		t.Error("docker-sock volumeMount not found")
	}
}

func TestSnapshotJob_WithPushSecret(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-4"
	snap.Namespace = "devpods"
	snap.Spec.Image = "reg:tag"
	snap.Spec.PushSecretRef = &devpodv1alpha1.LocalObjectRef{Name: "my-creds"}

	secretName := "my-creds"
	job := render.SnapshotJob(snap, "cid", "n1", &secretName)

	var secretVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "push-secret" {
			secretVol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if secretVol == nil {
		t.Fatal("push-secret volume not found")
	}
	if secretVol.Secret.SecretName != "my-creds" {
		t.Errorf("secret name = %q, want my-creds", secretVol.Secret.SecretName)
	}

	c := job.Spec.Template.Spec.Containers[0]
	envMap := map[string]string{}
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["DOCKER_CONFIG"] != "/root/.docker" {
		t.Errorf("DOCKER_CONFIG = %q, want /root/.docker", envMap["DOCKER_CONFIG"])
	}
}

func TestSnapshotJob_WithoutPushSecret(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-5"
	snap.Namespace = "devpods"
	snap.Spec.Image = "reg:tag"

	job := render.SnapshotJob(snap, "cid", "n1", nil)

	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "push-secret" {
			t.Error("push-secret volume should not exist when no secret ref")
		}
	}

	c := job.Spec.Template.Spec.Containers[0]
	for _, e := range c.Env {
		if e.Name == "DOCKER_CONFIG" {
			t.Error("DOCKER_CONFIG env should not exist when no secret ref")
		}
	}
}

func TestSnapshotJob_Labels(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-6"
	snap.Namespace = "devpods"
	snap.Spec.DevPodName = "my-dp"
	snap.Spec.Image = "reg:tag"

	job := render.SnapshotJob(snap, "cid", "n1", nil)

	if job.Labels["devpod.io/snapshot"] != "snap-6" {
		t.Errorf("label devpod.io/snapshot = %q, want snap-6", job.Labels["devpod.io/snapshot"])
	}
	if job.Labels["devpod.io/devpod"] != "my-dp" {
		t.Errorf("label devpod.io/devpod = %q, want my-dp", job.Labels["devpod.io/devpod"])
	}
}

func TestSnapshotJob_TerminationMessage(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "snap-7"
	snap.Namespace = "devpods"
	snap.Spec.Image = "reg:tag"

	job := render.SnapshotJob(snap, "cid", "n1", nil)
	c := job.Spec.Template.Spec.Containers[0]

	if c.TerminationMessagePath != "/dev/termination-log" {
		t.Errorf("terminationMessagePath = %q", c.TerminationMessagePath)
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("terminationMessagePolicy = %q, want FallbackToLogsOnError", c.TerminationMessagePolicy)
	}
}

func TestSnapshotJob_ReturnsJob(t *testing.T) {
	snap := &devpodv1alpha1.DevPodSnapshot{}
	snap.Name = "s"
	snap.Namespace = "devpods"
	snap.Spec.Image = "r:t"

	var _ *batchv1.Job = render.SnapshotJob(snap, "c", "n", nil)
}
