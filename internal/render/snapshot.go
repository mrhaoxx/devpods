// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const (
	LabelSnapshot = "devpod.io/snapshot"

	// SnapshotContainer is the Job's only container: devpod-snapshot
	// (images/snapshot), whose stdout carries the snapshot v2 protocol
	// (spec §2.1).
	SnapshotContainer = "snapshot"

	// Container runtimes a snapshot Job knows how to commit from.
	RuntimeContainerd = "containerd"
	RuntimeDocker     = "docker"

	DefaultContainerdSocket = "/run/containerd/containerd.sock"
	dockerSocket            = "/var/run/docker.sock"
	// The kubelet's containers live in this containerd namespace.
	containerdNamespace = "k8s.io"

	// snapshotGraceSeconds is how long a cancelled Job has to stop and
	// undo what it did on the node (devpod-snapshot allows itself 20 s).
	snapshotGraceSeconds = 30
	// snapshotDeadlineSeconds bounds a stuck commit or push.
	snapshotDeadlineSeconds = 2 * 60 * 60
)

// SnapshotJobName returns the deterministic Job name for a DevPodSnapshot.
func SnapshotJobName(snap *devpodv1alpha1.DevPodSnapshot) string {
	return "snapshot-" + snap.Name
}

// SplitContainerID splits a ContainerStatus.containerID
// ("containerd://<id>", "docker://<id>") into the runtime and the id.
func SplitContainerID(containerID string) (runtime, id string, err error) {
	scheme, id, ok := strings.Cut(containerID, "://")
	if !ok || id == "" {
		return "", "", fmt.Errorf("container id %q has no runtime prefix", containerID)
	}
	switch scheme {
	case RuntimeContainerd, RuntimeDocker:
		return scheme, id, nil
	}
	return "", "", fmt.Errorf("snapshots are not supported on the %q container runtime", scheme)
}

// SnapshotTarget is what a snapshot Job acts on.
type SnapshotTarget struct {
	Runtime     string // RuntimeContainerd or RuntimeDocker
	ContainerID string // without the runtime prefix
	NodeName    string
	// PushSecret names a dockerconfigjson Secret for the push; nil = the
	// node's own credentials.
	PushSecret *string
	// Image is the Job image (images/snapshot): its entrypoint is
	// devpod-snapshot, configured through the environment.
	Image string
	// ContainerdSocket on the node; "" = DefaultContainerdSocket.
	ContainerdSocket string
}

// SnapshotJob renders the batch/v1 Job that commits the container on its
// node (and pushes the image unless spec.push is false). The Job mounts
// the node's runtime socket, which is why it runs pinned to that node;
// devpod-snapshot talks to the runtime's API through it (containerd: the
// containerd client and nerdctl's commit package; Docker: the Engine API).
func SnapshotJob(snap *devpodv1alpha1.DevPodSnapshot, t SnapshotTarget) *batchv1.Job {
	labels := map[string]string{
		LabelSnapshot: snap.Name,
		LabelDevPod:   snap.Spec.DevPodName,
		LabelManaged:  "devpod-controller",
	}
	pause := snap.Spec.Pause == nil || *snap.Spec.Pause
	push := snap.Spec.Push

	socket := dockerSocket
	if t.Runtime == RuntimeContainerd {
		if socket = t.ContainerdSocket; socket == "" {
			socket = DefaultContainerdSocket
		}
	}
	env := []corev1.EnvVar{
		{Name: "RUNTIME", Value: t.Runtime},
		{Name: "RUNTIME_SOCKET", Value: socket},
		{Name: "CONTAINER_ID", Value: t.ContainerID},
		{Name: "TARGET_IMAGE", Value: snap.Spec.Image},
		{Name: "PUSH", Value: strconv.FormatBool(push)},
		{Name: "PAUSE", Value: strconv.FormatBool(pause)},
	}
	if t.Runtime == RuntimeContainerd {
		env = append(env, corev1.EnvVar{Name: "CONTAINERD_NAMESPACE", Value: containerdNamespace})
	}

	socketType := corev1.HostPathSocket
	volumes := []corev1.Volume{{
		Name:         "runtime-sock",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: socket, Type: &socketType}},
	}}
	mounts := []corev1.VolumeMount{{Name: "runtime-sock", MountPath: socket}}

	if push && t.PushSecret != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "push-secret",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: *t.PushSecret,
					Items:      []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "push-secret", MountPath: "/root/.docker", ReadOnly: true})
		// devpod-snapshot reads ${DOCKER_CONFIG}/config.json for either runtime
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/root/.docker"})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SnapshotJobName(snap),
			Namespace: snap.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To[int32](300),
			BackoffLimit:            ptr.To[int32](0),
			ActiveDeadlineSeconds:   ptr.To[int64](snapshotDeadlineSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeName:      t.NodeName,
					// a cancel (the snapshot is deleted) sends SIGTERM:
					// devpod-snapshot stops and removes the image it made
					TerminationGracePeriodSeconds: ptr.To[int64](snapshotGraceSeconds),
					Containers: []corev1.Container{{
						Name:                     SnapshotContainer,
						Image:                    t.Image,
						Env:                      env,
						VolumeMounts:             mounts,
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// SnapshotResult is what the Job's termination message says.
type SnapshotResult struct {
	Digest    string
	SizeBytes int64
	Error     string
}

// ParseSnapshotResult reads devpod-snapshot's termination message:
// "digest=sha256:… size=123" on success (either key may be missing),
// "error=<text>" on failure. Anything else (the log tail Kubernetes falls
// back to when nothing was written) is returned as the error text of
// its last non-empty line.
func ParseSnapshotResult(msg string) SnapshotResult {
	var r SnapshotResult
	msg = strings.TrimSpace(msg)
	if e, ok := strings.CutPrefix(msg, "error="); ok {
		r.Error = strings.TrimSpace(e)
		return r
	}
	known := false
	for _, f := range strings.Fields(msg) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "digest":
			r.Digest, known = v, true
		case "size":
			r.SizeBytes, _ = strconv.ParseInt(v, 10, 64)
			known = true
		}
	}
	if !known && msg != "" {
		lines := strings.Split(msg, "\n")
		last := strings.TrimSpace(lines[len(lines)-1])
		r.Error = strings.TrimPrefix(last, "@error ")
	}
	return r
}
