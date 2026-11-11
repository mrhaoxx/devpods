// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const (
	LabelSnapshot        = "devpod.io/snapshot"
	LabelSnapshotRuntime = "devpod.io/snapshot-runtime"

	snapshotCommitPush = `set -eo pipefail; docker commit "$CONTAINER_ID" "$TARGET_IMAGE" && docker push "$TARGET_IMAGE" 2>&1 | tee /tmp/push.log && sed -n 's/.*digest: \([^[:space:]]*\).*/\1/p' /tmp/push.log > /dev/termination-log`
	snapshotCommitOnly = `docker commit "$CONTAINER_ID" "$TARGET_IMAGE" > /dev/termination-log`

	containerdSnapshotCLI = `set -eu
nerdctl() {
  command nerdctl --address "$CONTAINERD_ADDRESS" --namespace "$CONTAINERD_NAMESPACE" --data-root /work/nerdctl --insecure-registry="$CONTAINERD_INSECURE_REGISTRY" "$@"
}
`
	containerdCommitOnly = containerdSnapshotCLI + `nerdctl commit --format oci "$CONTAINER_ID" "$TARGET_IMAGE" > /dev/termination-log`
	containerdCommitPush = containerdSnapshotCLI + `nerdctl commit --format oci "$CONTAINER_ID" "$TARGET_IMAGE"
nerdctl push "$TARGET_IMAGE"
nerdctl image inspect --mode native --format '{{.Image.Target.Digest}}' "$TARGET_IMAGE" > /dev/termination-log`
)

// SnapshotOptions are operator settings, shared by all snapshot Jobs.
type SnapshotOptions struct {
	DockerImage                string
	ContainerdImage            string
	ContainerdAddress          string
	ContainerdNamespace        string
	ContainerdInsecureRegistry bool
}

// SnapshotJobName returns the deterministic Job name for a DevPodSnapshot.
func SnapshotJobName(snap *devpodv1alpha1.DevPodSnapshot) string {
	return "snapshot-" + snap.Name
}

// SnapshotJob renders a commit (+ optional push) Job for the source runtime.
func SnapshotJob(
	snap *devpodv1alpha1.DevPodSnapshot,
	runtimeName string,
	containerID string,
	nodeName string,
	opts SnapshotOptions,
) (*batchv1.Job, error) {
	labels := map[string]string{
		LabelSnapshot:        snap.Name,
		LabelSnapshotRuntime: runtimeName,
		LabelDevPod:          snap.Spec.DevPodName,
		LabelManaged:         "devpod-controller",
	}

	image, socketName, socketPath := opts.DockerImage, "docker-sock", "/var/run/docker.sock"
	script := snapshotCommitOnly
	if snap.Spec.Push {
		script = snapshotCommitPush
	}
	env := []corev1.EnvVar{
		{Name: "CONTAINER_ID", Value: containerID},
		{Name: "TARGET_IMAGE", Value: snap.Spec.Image},
	}
	switch runtimeName {
	case "docker":
	case "containerd":
		if opts.ContainerdImage == "" || opts.ContainerdAddress == "" || opts.ContainerdNamespace == "" {
			return nil, fmt.Errorf("containerd snapshots require an image, socket address and namespace")
		}
		image, socketName, socketPath = opts.ContainerdImage, "containerd-sock", opts.ContainerdAddress
		script = containerdCommitOnly
		if snap.Spec.Push {
			script = containerdCommitPush
		}
		env = append(env,
			corev1.EnvVar{Name: "CONTAINERD_ADDRESS", Value: socketPath},
			corev1.EnvVar{Name: "CONTAINERD_NAMESPACE", Value: opts.ContainerdNamespace},
			corev1.EnvVar{Name: "CONTAINERD_INSECURE_REGISTRY", Value: strconv.FormatBool(opts.ContainerdInsecureRegistry)},
		)
	default:
		return nil, fmt.Errorf("unsupported container runtime %q (supported: docker, containerd)", runtimeName)
	}

	socketType := corev1.HostPathSocket
	volumes := []corev1.Volume{{
		Name: socketName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: socketPath,
				Type: &socketType,
			},
		},
	}}

	mounts := []corev1.VolumeMount{{
		Name:      socketName,
		MountPath: socketPath,
	}}

	if runtimeName == "containerd" {
		volumes = append(volumes, corev1.Volume{
			Name:         "work",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "work", MountPath: "/work"})
	}

	// nerdctl commit may fetch missing base layers even without a push.
	if snap.Spec.PushSecretRef != nil && (snap.Spec.Push || runtimeName == "containerd") {
		volumes = append(volumes, corev1.Volume{
			Name: "push-secret",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: snap.Spec.PushSecretRef.Name,
					Items: []corev1.KeyToPath{{
						Key:  ".dockerconfigjson",
						Path: "config.json",
					}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "push-secret",
			MountPath: "/root/.docker",
			ReadOnly:  true,
		})
		env = append(env, corev1.EnvVar{
			Name:  "DOCKER_CONFIG",
			Value: "/root/.docker",
		})
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
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeName:      nodeName,
					Containers: []corev1.Container{{
						Name:                     "snapshot",
						Image:                    image,
						Command:                  []string{"sh", "-c"},
						Args:                     []string{script},
						Env:                      env,
						VolumeMounts:             mounts,
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
					Volumes: volumes,
				},
			},
		},
	}, nil
}
