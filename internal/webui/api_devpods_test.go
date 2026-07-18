// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func newServer(t *testing.T) (*webui.Server, *webui.SessionManager) {
	t.Helper()
	sm := webui.NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	return &webui.Server{
		Client:   k8sClient,
		Reader:   k8sManager.GetAPIReader(),
		Cache:    k8sManager.GetCache(),
		NS:       "devpods",
		Sessions: sm,
		DefaultQuota: devpodv1alpha1.UserQuota{
			MaxDevPods: ptr.To(int32(5)),
			Compute: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("8"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
		},
		KoreEnabled:       true,
		PubkeySelfService: true,
		SSHHost:           "gw.example.com",
		SSHPort:           2222,
	}, sm
}

func forge(sm *webui.SessionManager, user string, admin bool) *http.Cookie {
	return &http.Cookie{Name: webui.SessionCookie, Value: sm.Mint(user, admin, time.Now())}
}

func doJSON(t *testing.T, h http.HandlerFunc, method, target string, pathVals map[string]string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func cleanupDevPods(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var list devpodv1alpha1.DevPodList
	if err := k8sClient.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		_ = k8sClient.Delete(ctx, &list.Items[i])
	}
	// Wait until the cache reflects the deletions — the next subtest's
	// quota aggregation reads through the same cache.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var left devpodv1alpha1.DevPodList
		if err := k8sClient.List(ctx, &left); err == nil && len(left.Items) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("devpods never drained from cache")
}

const createBody = `{"name":"dev1","image":"ubuntu:24.04","cpu":"2","memory":"4Gi"}`

// waitDevPod polls the CACHED client until the informer has seen the
// object — handlers read through the cache, so tests must not act on
// a fresh create until the cache caught up.
func waitDevPod(t *testing.T, name string) devpodv1alpha1.DevPod {
	t.Helper()
	var dp devpodv1alpha1.DevPod
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "devpods"}, &dp); err == nil {
			return dp
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("devpod %s never appeared in cache", name)
	return dp
}

// waitTemplate polls the cached client until the informer has the
// template — the create handler resolves templateRef through the cache.
func waitTemplate(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var tpl devpodv1alpha1.DevPodTemplate
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, &tpl); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("template %s never appeared in cache", name)
}

// waitRunning polls until the cached object reports the wanted
// running state (post-PATCH visibility for subsequent quota reads).
func waitRunning(t *testing.T, name string, want bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var dp devpodv1alpha1.DevPod
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "devpods"}, &dp); err == nil && dp.Spec.Running == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("devpod %s never reached running=%v in cache", name, want)
}

func TestDevPodAPI(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)
	bob := forge(sm, "gl-bob", false)

	t.Run("create plain custom", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		dp := waitDevPod(t, "gl-alice-dev1")
		if dp.Spec.Owner != "gl-alice" || !dp.Spec.Running {
			t.Fatalf("spec = %+v", dp.Spec)
		}
	})

	t.Run("kore annotations rejected for non-admin YAML", func(t *testing.T) {
		yamlBody := fmt.Sprintf(`{"yaml":%q}`, `
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: gl-alice-pinned
spec:
  owner: gl-alice
  running: true
  pod:
    metadata:
      annotations:
        kore.zjusct.io/pin: "true"
    spec:
      containers:
      - name: dev
        image: ubuntu:24.04
        resources:
          limits: {cpu: "2", memory: "4Gi"}
`)
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, yamlBody)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "KORE_ANNOTATIONS_FORBIDDEN") {
			t.Fatalf("wrong code: %s", rec.Body)
		}
	})

	t.Run("overlay template stamps annotations", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		tpl := pinTemplate()
		tpl.Name = "pin8"
		if err := k8sClient.Create(context.Background(), tpl); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tpl) })
		waitTemplate(t, "pin8")

		body := `{"name":"pinned","image":"ubuntu:24.04","cpu":"2","memory":"4Gi","templateRef":"pin8"}`
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		dp := waitDevPod(t, "gl-alice-pinned")
		if dp.Spec.Pod.Metadata.Annotations["kore.zjusct.io/pin"] != "true" {
			t.Fatalf("annotations = %v", dp.Spec.Pod.Metadata.Annotations)
		}
		if dp.Spec.Pod.Spec.Containers[0].Resources.Limits.Cpu().String() != "8" {
			t.Fatal("binding resources not applied")
		}
	})

	t.Run("preset template builds pod", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		tpl := &devpodv1alpha1.DevPodTemplate{}
		tpl.Name = "preset1"
		tpl.Spec = devpodv1alpha1.DevPodTemplateSpec{
			DisplayName: "Plain Ubuntu 4C8G",
			PodPreset: &devpodv1alpha1.PodPresetSpec{
				Pod: devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "dev",
					Image: "ubuntu:24.04",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
					},
				}}}},
				Shell: "zsh",
			},
		}
		if err := k8sClient.Create(context.Background(), tpl); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), tpl) })
		waitTemplate(t, "preset1")

		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice,
			`{"name":"fromtpl","templateRef":"preset1"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		dp := waitDevPod(t, "gl-alice-fromtpl")
		if dp.Spec.Pod == nil || dp.Spec.Pod.Spec.Containers[0].Image != "ubuntu:24.04" || dp.Spec.Shell != "zsh" {
			t.Fatalf("preset not applied: %+v", dp.Spec)
		}
	})

	t.Run("quota exceeded on create", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		big := `{"name":"big","image":"ubuntu:24.04","cpu":"6","memory":"8Gi"}`
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, big); rec.Code != http.StatusCreated {
			t.Fatalf("first create: %d %s", rec.Code, rec.Body)
		}
		waitDevPod(t, "gl-alice-big")
		big2 := `{"name":"big2","image":"ubuntu:24.04","cpu":"6","memory":"8Gi"}`
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, big2)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
		var body struct {
			Code   string                 `json:"code"`
			Detail []webui.QuotaViolation `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != "QUOTA_EXCEEDED" || len(body.Detail) == 0 || body.Detail[0].Resource != "cpu" {
			t.Fatalf("body = %+v", body)
		}
	})

	t.Run("hibernate then wake over quota", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		mk := func(name, cpu string) {
			b := fmt.Sprintf(`{"name":%q,"image":"ubuntu:24.04","cpu":%q,"memory":"1Gi"}`, name, cpu)
			if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, b); rec.Code != http.StatusCreated {
				t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
			}
			waitDevPod(t, "gl-alice-"+name)
		}
		mk("a", "6")
		rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/gl-alice-a", map[string]string{"name": "gl-alice-a"}, alice, `{"running":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("hibernate: %d %s", rec.Code, rec.Body)
		}
		waitRunning(t, "gl-alice-a", false)
		mk("b", "6") // fits because a is hibernated
		rec = doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/gl-alice-a", map[string]string{"name": "gl-alice-a"}, alice, `{"running":true}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("wake should exceed quota: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("ownership isolation", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d", rec.Code)
		}
		waitDevPod(t, "gl-alice-dev1")
		rec := doJSON(t, s.HandleGetDevPodForTest(), "GET", "/api/devpods/gl-alice-dev1", map[string]string{"name": "gl-alice-dev1"}, bob, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("bob should get 404, got %d", rec.Code)
		}
		rec = doJSON(t, s.HandleDeleteDevPodForTest(), "DELETE", "/api/devpods/gl-alice-dev1", map[string]string{"name": "gl-alice-dev1"}, bob, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("bob delete should 404, got %d", rec.Code)
		}
		rec = doJSON(t, s.HandleListDevPodsForTest(), "GET", "/api/devpods", nil, bob, "")
		if strings.Contains(rec.Body.String(), "gl-alice") {
			t.Fatalf("bob sees alice's pods: %s", rec.Body)
		}
	})

	t.Run("missing limits rejected", func(t *testing.T) {
		body := `{"name":"nolim","image":"ubuntu:24.04"}`
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("vm rejected for non-admin", func(t *testing.T) {
		yamlBody := fmt.Sprintf(`{"yaml":%q}`, `
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: gl-alice-vm
spec:
  owner: gl-alice
  running: true
  vm:
    template: {}
`)
		rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, yamlBody)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
		}
	})

	t.Run("delete own devpod", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d", rec.Code)
		}
		waitDevPod(t, "gl-alice-dev1")
		rec := doJSON(t, s.HandleDeleteDevPodForTest(), "DELETE", "/api/devpods/gl-alice-dev1", map[string]string{"name": "gl-alice-dev1"}, alice, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("unauthenticated 401", func(t *testing.T) {
		rec := doJSON(t, s.HandleListDevPodsForTest(), "GET", "/api/devpods", nil, nil, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
