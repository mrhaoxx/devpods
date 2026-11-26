// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"testing"
	"time"

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
	// the client reads a cache: what was written shows a moment later
	becomes := func(what string, ok func(map[string]string) bool) {
		t.Helper()
		var cm corev1.ConfigMap
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: "devpods", Name: devpodv1alpha1.ControllerFeaturesConfigMap}, &cm)
			if err == nil && ok(cm.Data) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %v %v", what, cm.Data, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := controllers.AnnounceFeatures(env.Ctx, env.Client, "devpods", "v1.2.3", "/home"); err != nil {
		t.Fatal(err)
	}
	becomes("data", func(d map[string]string) bool {
		return d["version"] == "v1.2.3" && d["features"] == "jobs" && d["sharedHomes"] == "/home"
	})
	if err := controllers.AnnounceFeatures(env.Ctx, env.Client, "devpods", "v1.2.4", ""); err != nil {
		t.Fatal(err)
	}
	becomes("again", func(d map[string]string) bool { return d["version"] == "v1.2.4" && d["sharedHomes"] == "" })
}
