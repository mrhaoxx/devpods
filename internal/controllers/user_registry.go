// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// dockerAuth is one entry of a dockerconfigjson "auths" map.
type dockerAuth struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

// dockerConfigKey is the "auths" key for a registry host: Docker Hub's
// credentials are looked up under its legacy index URL.
func dockerConfigKey(host string) string {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "https://index.docker.io/v1/"
	}
	return host
}

// reconcileRegistries keeps the user's registry Secret (snapshot spec v2
// §5.1) in step with spec.registries: credentials waiting in
// spec.registryAuth are merged in, hosts no longer listed are pruned, and
// the Secret goes away with the last host. Then spec.registryAuth is
// cleared, so a plaintext password lives in the User for one reconcile.
// Every step can be repeated; nothing here logs a credential.
func (r *UserReconciler) reconcileRegistries(ctx context.Context, u *devpodv1alpha1.User) error {
	if r.DevPodNamespace == "" {
		return nil
	}
	key := types.NamespacedName{Name: devpodv1alpha1.RegistryCredentialsSecretName(u.Name), Namespace: r.DevPodNamespace}
	var sec corev1.Secret
	exists := true
	if err := r.Get(ctx, key, &sec); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get registry secret: %w", err)
		}
		exists = false
	}
	old := map[string]dockerAuth{}
	if exists {
		var cfg struct {
			Auths map[string]dockerAuth `json:"auths"`
		}
		// an unreadable Secret is rebuilt from what is pending
		if json.Unmarshal(sec.Data[corev1.DockerConfigJsonKey], &cfg) == nil && cfg.Auths != nil {
			old = cfg.Auths
		}
	}
	pending := map[string]devpodv1alpha1.UserRegistryAuth{}
	for _, a := range u.Spec.RegistryAuth {
		pending[a.Host] = a // the last one for a host wins
	}
	want := map[string]dockerAuth{}
	for _, reg := range u.Spec.Registries {
		k := dockerConfigKey(reg.Host)
		if a, ok := pending[reg.Host]; ok {
			want[k] = dockerAuth{Username: a.Username, Password: a.Password,
				Auth: base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))}
		} else if a, ok := old[k]; ok {
			want[k] = a
		}
	}

	switch {
	case len(want) == 0:
		if exists && sec.Labels[devpodv1alpha1.RegistryCredentialsLabel] == "true" {
			if err := r.Delete(ctx, &sec); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete registry secret: %w", err)
			}
		}
	default:
		data, err := json.Marshal(map[string]any{"auths": want})
		if err != nil {
			return err
		}
		if !exists {
			sec = corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Type:       corev1.SecretTypeDockerConfigJson,
			}
		}
		labels := map[string]string{devpodv1alpha1.RegistryCredentialsLabel: "true", devpodv1alpha1.OwnerLabel: u.Name}
		same := exists && bytes.Equal(sec.Data[corev1.DockerConfigJsonKey], data) &&
			sec.Labels[devpodv1alpha1.RegistryCredentialsLabel] == "true" && sec.Labels[devpodv1alpha1.OwnerLabel] == u.Name
		if !same {
			if sec.Labels == nil {
				sec.Labels = map[string]string{}
			}
			for k, v := range labels {
				sec.Labels[k] = v
			}
			sec.Data = map[string][]byte{corev1.DockerConfigJsonKey: data}
			// the Secret goes with the User
			if err := controllerutil.SetControllerReference(u, &sec, r.Scheme); err != nil {
				return fmt.Errorf("own registry secret: %w", err)
			}
			if exists {
				err = r.Update(ctx, &sec)
			} else {
				err = r.Create(ctx, &sec)
			}
			if err != nil {
				return fmt.Errorf("write registry secret: %w", err)
			}
		}
	}

	if len(u.Spec.RegistryAuth) > 0 {
		// A conflict here (the webui added another credential meanwhile)
		// is returned and retried: the next pass merges both.
		u.Spec.RegistryAuth = nil
		if err := r.Update(ctx, u); err != nil {
			return fmt.Errorf("clear registryAuth: %w", err)
		}
	}
	return nil
}
