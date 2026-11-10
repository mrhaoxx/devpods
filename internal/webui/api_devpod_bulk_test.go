// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Migrate (PATCH node) pins a DevPod to a node; redeploy recreates its Pod.
func TestDevPodMigrateAndRedeploy(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "mg-alice", false)
	root := forge(sm, "mg-root", true)
	ctx := context.Background()
	t.Cleanup(func() { cleanupDevPods(t) })
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "mg-alice-dev1")
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mg-node-1"}}
	if err := k8sClient.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })
	path := map[string]string{"name": "mg-alice-dev1"}
	patch := func(c *http.Cookie, body string, want int) {
		t.Helper()
		if rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/mg-alice-dev1", path, c, body); rec.Code != want {
			t.Fatalf("PATCH %s: %d (want %d) %s", body, rec.Code, want, rec.Body)
		}
	}
	selector := func() map[string]string {
		var dp devpodv1alpha1.DevPod
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: "mg-alice-dev1", Namespace: "devpods"}, &dp); err != nil {
			t.Fatal(err)
		}
		return dp.Spec.Pod.Spec.NodeSelector
	}

	t.Run("migrate pins to a node that exists (owners too: the node rule decides)", func(t *testing.T) {
		patch(alice, `{"node":"no-such-node"}`, http.StatusBadRequest)
		patch(root, `{"node":"no-such-node"}`, http.StatusBadRequest)
		// the node may not be in the cache yet
		deadline := time.Now().Add(10 * time.Second)
		for {
			rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/mg-alice-dev1", path, root, `{"node":"mg-node-1"}`)
			if rec.Code == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("migrate: %d %s", rec.Code, rec.Body)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if got := selector()["kubernetes.io/hostname"]; got != "mg-node-1" {
			t.Fatalf("node selector %v", selector())
		}
		patch(root, `{"node":""}`, http.StatusOK) // back to the scheduler's choice
		if _, pinned := selector()["kubernetes.io/hostname"]; pinned {
			t.Fatalf("still pinned: %v", selector())
		}
	})

	t.Run("redeploy deletes the Pod; without one it is 409", func(t *testing.T) {
		redeploy := func(c *http.Cookie, want int) {
			t.Helper()
			if rec := doJSON(t, s.HandleRedeployDevPodForTest(), "POST", "/api/devpods/mg-alice-dev1/redeploy", path, c, ""); rec.Code != want {
				t.Fatalf("redeploy: %d (want %d) %s", rec.Code, want, rec.Body)
			}
		}
		redeploy(alice, http.StatusConflict)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "mg-alice-dev1", Namespace: "devpods"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04"}}}}
		if err := k8sClient.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		var dp devpodv1alpha1.DevPod
		if err := s.Reader.Get(ctx, types.NamespacedName{Name: "mg-alice-dev1", Namespace: "devpods"}, &dp); err != nil {
			t.Fatal(err)
		}
		dp.Status.WorkloadRef = &devpodv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "Pod", Name: "mg-alice-dev1"}
		if err := k8sClient.Status().Update(ctx, &dp); err != nil {
			t.Fatal(err)
		}
		redeploy(forge(sm, "mg-bob", false), http.StatusNotFound) // not theirs
		deadline := time.Now().Add(10 * time.Second)
		for { // the status write reaches the cache
			rec := doJSON(t, s.HandleRedeployDevPodForTest(), "POST", "/api/devpods/mg-alice-dev1/redeploy", path, alice, "")
			if rec.Code == http.StatusNoContent {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("redeploy: %d %s", rec.Code, rec.Body)
			}
			time.Sleep(100 * time.Millisecond)
		}
		var got corev1.Pod
		err := s.Reader.Get(ctx, types.NamespacedName{Name: "mg-alice-dev1", Namespace: "devpods"}, &got)
		if !apierrors.IsNotFound(err) && got.DeletionTimestamp == nil {
			t.Fatalf("pod still there: %v", err)
		}
	})
}
