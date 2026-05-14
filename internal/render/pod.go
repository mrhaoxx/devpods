// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// ErrWorkloadKindMissing is returned when DevPod.spec has neither pod
// nor vm set (validation should normally reject this earlier).
var ErrWorkloadKindMissing = errors.New("no workload kind on DevPod")

// Pod renders the Kubernetes Pod for a Pod-backed DevPod.
//
// v2 layout: prepend an initContainer that copies the supervisor's
// /opt/devpod tree into a shared emptyDir; wrap the target container's
// command/args with the supervisor; mount the bin emptyDir and the
// per-DevPod host-key Secret on the target. No sidecar container, no
// shareProcessNamespace, no setns at runtime.
//
// Other user containers (when containers[] has > 1 entry) pass
// through verbatim.
func Pod(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) (*corev1.Pod, error) {
	if dp.Spec.Pod == nil {
		return nil, ErrWorkloadKindMissing
	}
	if len(dp.Spec.Pod.Spec.Containers) == 0 {
		return nil, errors.New("spec.pod.spec.containers must be non-empty")
	}

	user := dp.Spec.Pod.Spec.DeepCopy()

	// Shared volumes: binaries emptyDir + per-DevPod host-key Secret.
	user.Volumes = append(user.Volumes,
		supervisorBinVolume(),
		hostKeySecretVolume(dp),
	)

	// Optional persistence volume (unchanged from M2).
	if dp.Spec.Persistence != nil {
		user.Volumes = append(user.Volumes, homeVolume(dp))
		if err := injectHomeMount(user, dp); err != nil {
			return nil, err
		}
	}

	// Optional hostPath home directory injection from shared filesystem.
	if cfg.Spec.HomeDir != nil && dp.Spec.Persistence == nil {
		injectHostPathHome(user, dp, cfg)
	}

	// Wrap the target container's command with the supervisor.
	if err := wrapTargetWithSupervisor(user, dp, cfg); err != nil {
		return nil, err
	}

	// Prepend the bootstrap initContainer (cp binaries into emptyDir).
	user.InitContainers = append(
		[]corev1.Container{bootstrapInitContainer(cfg.Spec.SupervisorImage)},
		user.InitContainers...,
	)

	pod := &corev1.Pod{
		ObjectMeta: ObjectMeta(PodName(dp), cfg.Spec.DevPodNamespace, dp),
		Spec:       *user,
	}
	mergeStrings(pod.Labels, dp.Spec.Pod.ObjectMeta.Labels)
	pod.Annotations = mergeStringsCopy(pod.Annotations, dp.Spec.Pod.ObjectMeta.Annotations)
	return pod, nil
}

// wrapTargetWithSupervisor mutates the target container so its
// command becomes the supervisor and the user's original command +
// args falls into args. Also adds the two volumeMounts the supervisor
// needs and the env vars that tune supervisor behavior.
//
// Target container selection mirrors spec.persistence.targetContainer
// for symmetry: if set use that name, else containers[0].
func wrapTargetWithSupervisor(spec *corev1.PodSpec, dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) error {
	targetName := ""
	if dp.Spec.Persistence != nil && dp.Spec.Persistence.TargetContainer != "" {
		targetName = dp.Spec.Persistence.TargetContainer
	} else {
		targetName = spec.Containers[0].Name
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name != targetName {
			continue
		}
		c := &spec.Containers[i]

		// User's original command + args → supervisor's args. Note that
		// when the user provided no command (relying on image ENTRYPOINT),
		// args becomes empty and the supervisor runs only sshd.
		var orig []string
		orig = append(orig, c.Command...)
		orig = append(orig, c.Args...)

		c.Command = []string{"/opt/devpod/devpod-supervisor"}
		c.Args = orig

		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: VolumeSupervisorBin, MountPath: "/opt/devpod", ReadOnly: true},
			corev1.VolumeMount{Name: VolumeSupervisorHost, MountPath: "/etc/devpod", ReadOnly: true},
		)

		// Tune the supervisor via env so the binary stays generic.
		port := BackendPort(dp, cfg)
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "SUPERVISOR_SSHD_PORT", Value: strconv.Itoa(int(port))},
		)
		if dp.Spec.ExitOnUserCommandExit {
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "SUPERVISOR_EXIT_ON_USER_CMD", Value: "true"},
			)
		}
		if dp.Spec.Shell != "" {
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "DEVPOD_SHELL", Value: dp.Spec.Shell},
			)
		}
		return nil
	}
	return fmt.Errorf("supervisor: target container %q not found in spec.pod.spec.containers", targetName)
}

func bootstrapInitContainer(supervisorImage string) corev1.Container {
	return corev1.Container{
		Name:    SupervisorBootstrapContainerName,
		Image:   supervisorImage,
		Command: []string{"/opt/devpod/devpod-supervisor", "bootstrap"},
		Env: []corev1.EnvVar{
			{Name: "SUPERVISOR_BOOTSTRAP_SRC", Value: "/opt/devpod"},
			{Name: "SUPERVISOR_BOOTSTRAP_DST", Value: "/devpod-bin"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeSupervisorBin, MountPath: "/devpod-bin"},
		},
	}
}

func supervisorBinVolume() corev1.Volume {
	limit := resource.MustParse("100Mi")
	return corev1.Volume{
		Name: VolumeSupervisorBin,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit},
		},
	}
}

func hostKeySecretVolume(dp *devpodv1alpha1.DevPod) corev1.Volume {
	return corev1.Volume{
		Name: VolumeSupervisorHost,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  HostKeySecretName(dp),
				DefaultMode: ptr.To[int32](0o400),
			},
		},
	}
}

func homeVolume(dp *devpodv1alpha1.DevPod) corev1.Volume {
	return corev1.Volume{
		Name: VolumeHome,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: HomePVCName(dp),
			},
		},
	}
}

const VolumeHostHome = "devpod-host-home"

// injectHostPathHome adds a hostPath volume + mount for the owner's
// home directory from the node's shared filesystem.
func injectHostPathHome(spec *corev1.PodSpec, dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) {
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	hostPath := cfg.Spec.HomeDir.HostPathPrefix + "/" + dp.Spec.Owner
	mountPath := cfg.Spec.HomeDir.MountPrefix + "/" + dp.Spec.Owner
	if cfg.Spec.HomeDir.MountPrefix == "" {
		mountPath = "/home/" + dp.Spec.Owner
	}

	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: VolumeHostHome,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: hostPath,
				Type: &dirOrCreate,
			},
		},
	})
	spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name:      VolumeHostHome,
		MountPath: mountPath,
	})
	spec.Containers[0].Env = append(spec.Containers[0].Env, corev1.EnvVar{
		Name:  "HOME",
		Value: mountPath,
	})
}

// injectHomeMount appends a volumeMount named VolumeHome at
// spec.persistence.mountPath onto the target container in pod.
// Target is spec.persistence.targetContainer, or containers[0] when
// unset. Returns an error if the named target does not exist; the
// webhook is the primary defense, this is just belt-and-braces.
func injectHomeMount(pod *corev1.PodSpec, dp *devpodv1alpha1.DevPod) error {
	p := dp.Spec.Persistence
	targetName := p.TargetContainer
	if targetName == "" {
		targetName = pod.Containers[0].Name
	}
	for i := range pod.Containers {
		if pod.Containers[i].Name == targetName {
			pod.Containers[i].VolumeMounts = append(pod.Containers[i].VolumeMounts, corev1.VolumeMount{
				Name:      VolumeHome,
				MountPath: p.MountPath,
			})
			return nil
		}
	}
	return fmt.Errorf("persistence.targetContainer %q not found in spec.pod.spec.containers", targetName)
}

// mergeStrings inserts entries from src into dst when the key is not
// already present. dst must be non-nil; callers in this package always
// pass a freshly-allocated map from DevPodLabels.
//
// Reserved DevPod label keys are already in dst when this is called,
// so any user attempt to override them silently no-ops. The
// validating webhook is the proper place to reject such inputs.
func mergeStrings(dst, src map[string]string) {
	for k, v := range src {
		if _, taken := dst[k]; !taken {
			dst[k] = v
		}
	}
}

func mergeStringsCopy(dst, src map[string]string) map[string]string {
	if dst == nil && src == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if _, taken := out[k]; !taken {
			out[k] = v
		}
	}
	return out
}
