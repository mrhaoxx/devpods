// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"encoding/json"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

func TestMeFeatures(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "mf-alice", false)
	features := func() map[string]any {
		t.Helper()
		rec := doJSON(t, s.HandleMeForTest(), "GET", "/api/me", nil, alice, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("me: %d %s", rec.Code, rec.Body)
		}
		var body struct {
			Features map[string]any `json:"features"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Features
	}
	f := features()
	if f["telemetry"] != false || f["reservations"] != false {
		t.Fatalf("without live deps: telemetry=%v reservations=%v", f["telemetry"], f["reservations"])
	}
	s.Live = &webui.LiveDeps{Hub: live.NewHub()}
	f = features()
	if f["telemetry"] != true || f["reservations"] != false {
		t.Fatalf("with live deps: telemetry=%v reservations=%v", f["telemetry"], f["reservations"])
	}
}

type patchResp struct {
	DevPod  devpodv1alpha1.DevPod `json:"devpod"`
	Rebuild bool                  `json:"rebuild"`
	Code    string                `json:"code"`
}

func TestPatchDevPodEdit(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "ed-alice", false)
	bob := forge(sm, "ed-bob", false)
	root := forge(sm, "ed-root", true)
	t.Cleanup(func() { cleanupDevPods(t) })
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "ed-alice-dev1")

	patch := func(t *testing.T, c *http.Cookie, body string, want int) patchResp {
		t.Helper()
		rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/ed-alice-dev1",
			map[string]string{"name": "ed-alice-dev1"}, c, body)
		if rec.Code != want {
			t.Fatalf("PATCH %s: %d (want %d) %s", body, rec.Code, want, rec.Body)
		}
		var out patchResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v %s", err, rec.Body)
		}
		return out
	}
	target := func(dp devpodv1alpha1.DevPod) corev1.Container { return dp.Spec.Pod.Spec.Containers[0] }

	t.Run("image change on running reports rebuild", func(t *testing.T) {
		out := patch(t, alice, `{"image":"ubuntu:25.04"}`, http.StatusOK)
		if !out.Rebuild || target(out.DevPod).Image != "ubuntu:25.04" {
			t.Fatalf("rebuild=%v image=%q", out.Rebuild, target(out.DevPod).Image)
		}
	})
	t.Run("collaborators only does not rebuild", func(t *testing.T) {
		out := patch(t, alice, `{"collaborators":["ed-bob"]}`, http.StatusOK)
		if out.Rebuild || len(out.DevPod.Spec.Collaborators) != 1 || out.DevPod.Spec.Collaborators[0] != "ed-bob" {
			t.Fatalf("rebuild=%v collaborators=%v", out.Rebuild, out.DevPod.Spec.Collaborators)
		}
	})
	t.Run("gpu sets the nvidia.com/gpu limit", func(t *testing.T) {
		out := patch(t, alice, `{"gpu":"1"}`, http.StatusOK)
		q := target(out.DevPod).Resources.Limits["nvidia.com/gpu"]
		if !out.Rebuild || q.String() != "1" {
			t.Fatalf("rebuild=%v gpu=%s", out.Rebuild, q.String())
		}
		out = patch(t, alice, `{"gpu":""}`, http.StatusOK)
		if _, ok := target(out.DevPod).Resources.Limits["nvidia.com/gpu"]; ok {
			t.Fatalf("empty gpu should drop the limit: %v", target(out.DevPod).Resources.Limits)
		}
	})
	t.Run("cpu over quota is 409 QUOTA_EXCEEDED", func(t *testing.T) {
		out := patch(t, alice, `{"cpu":"9"}`, http.StatusConflict)
		if out.Code != "QUOTA_EXCEEDED" {
			t.Fatalf("code = %q", out.Code)
		}
	})
	t.Run("persistence change is 400 IMMUTABLE", func(t *testing.T) {
		out := patch(t, alice, `{"persistence":{"size":"10Gi"}}`, http.StatusBadRequest)
		if out.Code != "IMMUTABLE" {
			t.Fatalf("code = %q", out.Code)
		}
	})
	t.Run("other user's DevPod is 404", func(t *testing.T) {
		patch(t, bob, `{"image":"evil:1"}`, http.StatusNotFound)
	})
	t.Run("admin may edit", func(t *testing.T) {
		out := patch(t, root, `{"memory":"8Gi"}`, http.StatusOK)
		q := target(out.DevPod).Resources.Limits[corev1.ResourceMemory]
		if !out.Rebuild || q.String() != "8Gi" {
			t.Fatalf("rebuild=%v memory=%s", out.Rebuild, q.String())
		}
	})
	t.Run("editing a hibernated DevPod does not rebuild", func(t *testing.T) {
		patch(t, alice, `{"running":false}`, http.StatusOK)
		out := patch(t, alice, `{"image":"ubuntu:24.04","shell":"zsh"}`, http.StatusOK)
		if out.Rebuild || out.DevPod.Spec.Shell != "zsh" {
			t.Fatalf("rebuild=%v shell=%q", out.Rebuild, out.DevPod.Spec.Shell)
		}
	})
}

func TestCreateDevPodGPUAndCollaborators(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gc-alice", false)
	t.Cleanup(func() { cleanupDevPods(t) })
	body := `{"name":"g","image":"ubuntu:24.04","cpu":"2","memory":"4Gi","gpu":"1","collaborators":["gc-bob"]}`
	rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	dp := waitDevPod(t, "gc-alice-g")
	q := dp.Spec.Pod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
	if q.String() != "1" {
		t.Fatalf("gpu limit = %q", q.String())
	}
	if len(dp.Spec.Collaborators) != 1 || dp.Spec.Collaborators[0] != "gc-bob" {
		t.Fatalf("collaborators = %v", dp.Spec.Collaborators)
	}
}
