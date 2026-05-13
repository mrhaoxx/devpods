// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// HomePVC renders the per-DevPod home PersistentVolumeClaim.
//
// Returns (nil, nil) when spec.persistence is unset — callers should
// treat that as "no PVC to apply".
//
// The returned object has no OwnerReferences set; the controller
// invokes controllerutil.SetControllerReference before Create.
func HomePVC(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) (*corev1.PersistentVolumeClaim, error) {
	if dp.Spec.Persistence == nil {
		return nil, nil
	}
	p := dp.Spec.Persistence

	var modes []corev1.PersistentVolumeAccessMode
	if len(p.AccessModes) == 0 {
		modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	} else {
		modes = append([]corev1.PersistentVolumeAccessMode(nil), p.AccessModes...)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: ObjectMeta(HomePVCName(dp), cfg.Spec.DevPodNamespace, dp),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: modes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: p.Size},
			},
		},
	}
	if p.StorageClassName != "" {
		pvc.Spec.StorageClassName = ptr.To(p.StorageClassName)
	}
	return pvc, nil
}
