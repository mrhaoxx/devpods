// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// AnnounceFeatures writes what this controller (version) can do into the
// DevPod namespace ns's ConfigMap devpod-controller — that it runs job
// ranks, and where owners' homes are shared (sharedHomes, "" when they
// are not) — for the cluster's agent (and a console of the cluster
// itself) to read.
func AnnounceFeatures(ctx context.Context, c client.Client, ns, version, sharedHomes string) error {
	data := map[string]string{
		devpodv1alpha1.ControllerVersionKey:  version,
		devpodv1alpha1.ControllerFeaturesKey: devpodv1alpha1.FeatureJobs,
	}
	if sharedHomes != "" {
		data[devpodv1alpha1.ControllerSharedHomesKey] = sharedHomes
	}
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.ControllerFeaturesConfigMap, Namespace: ns},
		Data:       data,
	}
	// applied: what it no longer says (shared homes gone) goes with it
	return c.Patch(ctx, cm, client.Apply, client.FieldOwner("devpod-controller"), client.ForceOwnership)
}
