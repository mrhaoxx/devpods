// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const (
	LabelSnapshot = "devpod.io/snapshot"

	snapshotScript = `set -eo pipefail; docker commit "$CONTAINER_ID" "$TARGET_IMAGE" && docker push "$TARGET_IMAGE" 2>&1 | tee /tmp/push.log && sed -n 's/.*digest: \(\S\+\).*/\1/p' /tmp/push.log > /dev/termination-log`
)

// SnapshotJobName returns the deterministic Job name for a DevPodSnapshot.
func SnapshotJobName(snap *devpodv1alpha1.DevPodSnapshot) string {
	return "snapshot-" + snap.Name
}

// SnapshotJob renders the batch/v1 Job that performs docker commit + push.
func SnapshotJob(
	snap *devpodv1alpha1.DevPodSnapshot,
	containerID string,
	nodeName string,
	pushSecret *string,
	snapshotImage string,
) *batchv1.Job {
	labels := map[string]string{
		LabelSnapshot: snap.Name,
		LabelDevPod:   snap.Spec.DevPodName,
		LabelManaged:  "devpod-controller",
	}

	socketType := corev1.HostPathSocket
	volumes := []corev1.Volume{{
		Name: "docker-sock",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/var/run/docker.sock",
				Type: &socketType,
			},
		},
	}}

	mounts := []corev1.VolumeMount{{
		Name:      "docker-sock",
		MountPath: "/var/run/docker.sock",
	}}

	env := []corev1.EnvVar{
		{Name: "CONTAINER_ID", Value: containerID},
		{Name: "TARGET_IMAGE", Value: snap.Spec.Image},
	}

	if pushSecret != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "push-secret",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: *pushSecret,
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
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeName:      nodeName,
					Containers: []corev1.Container{{
						Name:                     "snapshot",
						Image:                    snapshotImage,
						Command:                  []string{"sh", "-c"},
						Args:                     []string{snapshotScript},
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
