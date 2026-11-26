// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/controllers"
)

// The controller says what it can do, and where homes are shared; said
// again without shared homes, that goes.
func TestAnnounceFeatures(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	get := func() map[string]string {
		t.Helper()
		var cm corev1.ConfigMap
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: "devpods", Name: devpodv1alpha1.ControllerFeaturesConfigMap}, &cm); err != nil {
			t.Fatal(err)
		}
		return cm.Data
	}
	if err := controllers.AnnounceFeatures(env.Ctx, env.Client, "devpods", "v1.2.3", "/home"); err != nil {
		t.Fatal(err)
	}
	if d := get(); d["version"] != "v1.2.3" || d["features"] != "jobs" || d["sharedHomes"] != "/home" {
		t.Fatalf("data %v", d)
	}
	if err := controllers.AnnounceFeatures(env.Ctx, env.Client, "devpods", "v1.2.4", ""); err != nil {
		t.Fatal(err)
	}
	if d := get(); d["version"] != "v1.2.4" || d["sharedHomes"] != "" {
		t.Fatalf("again: %v", d)
	}
}
