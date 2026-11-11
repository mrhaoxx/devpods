// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// podSecretRef is one place a PodSpec names a Secret.
type podSecretRef struct{ Path, Name string }

// podSecretRefs lists every reference to a Secret in spec, by field path:
// volumes (secret, projected sources, and the volume types that carry a
// secretRef), env and envFrom of every kind of container, and
// imagePullSecrets.
func podSecretRefs(spec *corev1.PodSpec) []podSecretRef {
	var out []podSecretRef
	add := func(path, name string) { out = append(out, podSecretRef{path, name}) }
	local := func(path string, r *corev1.LocalObjectReference) {
		if r != nil {
			add(path, r.Name)
		}
	}
	for i, v := range spec.Volumes {
		p := fmt.Sprintf("volumes[%d]", i)
		if v.Secret != nil {
			add(p+".secret", v.Secret.SecretName)
		}
		if v.Projected != nil {
			for j, src := range v.Projected.Sources {
				if src.Secret != nil {
					add(fmt.Sprintf("%s.projected.sources[%d].secret", p, j), src.Secret.Name)
				}
			}
		}
		if v.CSI != nil {
			local(p+".csi.nodePublishSecretRef", v.CSI.NodePublishSecretRef)
		}
		if v.CephFS != nil {
			local(p+".cephfs.secretRef", v.CephFS.SecretRef)
		}
		if v.RBD != nil {
			local(p+".rbd.secretRef", v.RBD.SecretRef)
		}
		if v.ISCSI != nil {
			local(p+".iscsi.secretRef", v.ISCSI.SecretRef)
		}
		if v.FlexVolume != nil {
			local(p+".flexVolume.secretRef", v.FlexVolume.SecretRef)
		}
		if v.ScaleIO != nil {
			local(p+".scaleIO.secretRef", v.ScaleIO.SecretRef)
		}
		if v.StorageOS != nil {
			local(p+".storageos.secretRef", v.StorageOS.SecretRef)
		}
		if v.Cinder != nil {
			local(p+".cinder.secretRef", v.Cinder.SecretRef)
		}
		if v.AzureFile != nil {
			add(p+".azureFile.secretName", v.AzureFile.SecretName)
		}
	}
	env := func(kind string, i int, envs []corev1.EnvVar, from []corev1.EnvFromSource) {
		for j, e := range envs {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(fmt.Sprintf("%s[%d].env[%d].valueFrom.secretKeyRef", kind, i, j), e.ValueFrom.SecretKeyRef.Name)
			}
		}
		for j, f := range from {
			if f.SecretRef != nil {
				add(fmt.Sprintf("%s[%d].envFrom[%d].secretRef", kind, i, j), f.SecretRef.Name)
			}
		}
	}
	for i, c := range spec.Containers {
		env("containers", i, c.Env, c.EnvFrom)
	}
	for i, c := range spec.InitContainers {
		env("initContainers", i, c.Env, c.EnvFrom)
	}
	for i, c := range spec.EphemeralContainers {
		env("ephemeralContainers", i, c.Env, c.EnvFrom)
	}
	for i, r := range spec.ImagePullSecrets {
		add(fmt.Sprintf("imagePullSecrets[%d]", i), r.Name)
	}
	return out
}

// guardUserPodSpec is the check on a PodSpec a non-admin wrote (the YAML
// create path): it may not reach Secrets of the DevPod namespace — other
// users' registry credentials and snapshot push credentials live there —
// nor a service account token. The one Secret a user may name is their
// own registry credentials, as an image pull secret. Pods stamped from an
// admin's preset template are not the user's and are not checked.
func guardUserPodSpec(spec *corev1.PodSpec, user string) *serverErr {
	own := devpodv1alpha1.RegistryCredentialsSecretName(user)
	for _, ref := range podSecretRefs(spec) {
		if ref.Name == own && len(ref.Path) >= 16 && ref.Path[:16] == "imagePullSecrets" {
			continue
		}
		return &serverErr{http.StatusBadRequest, "SECRET_REFERENCE",
			fmt.Sprintf("spec.pod.spec.%s refers to a Secret; DevPods cannot use Secrets (save registry credentials in 设置 → 镜像仓库凭证 instead)", ref.Path),
			map[string]string{"path": "spec.pod.spec." + ref.Path}}
	}
	if sa := spec.ServiceAccountName; sa != "" && sa != "default" {
		return &serverErr{http.StatusBadRequest, "SERVICE_ACCOUNT", "spec.pod.spec.serviceAccountName must be left unset", map[string]string{"path": "spec.pod.spec.serviceAccountName"}}
	}
	if sa := spec.DeprecatedServiceAccount; sa != "" && sa != "default" {
		return &serverErr{http.StatusBadRequest, "SERVICE_ACCOUNT", "spec.pod.spec.serviceAccount must be left unset", map[string]string{"path": "spec.pod.spec.serviceAccount"}}
	}
	if t := spec.AutomountServiceAccountToken; t != nil && *t {
		return &serverErr{http.StatusBadRequest, "SERVICE_ACCOUNT", "spec.pod.spec.automountServiceAccountToken cannot be true", map[string]string{"path": "spec.pod.spec.automountServiceAccountToken"}}
	}
	for i, v := range spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for j, src := range v.Projected.Sources {
			if src.ServiceAccountToken != nil {
				path := fmt.Sprintf("spec.pod.spec.volumes[%d].projected.sources[%d].serviceAccountToken", i, j)
				return &serverErr{http.StatusBadRequest, "SERVICE_ACCOUNT", path + " is not allowed", map[string]string{"path": path}}
			}
		}
	}
	return nil
}
