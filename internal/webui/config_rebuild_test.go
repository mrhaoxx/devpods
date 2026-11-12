// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestRebuildFromConfig(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	t.Cleanup(func() { cleanupDevPods(t); cleanupConfigs(t) })
	alice, bob, carol := forge(sm, "rb-alice", false), forge(sm, "rb-bob", false), forge(sm, "rb-carol", false)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		return apiCall(t, srv, method, path, auth, body, want)
	}
	hash := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])[:16]
	}
	// a config of alice's that bob may use
	text := func(name, image, cpu string) string {
		return "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: " + name + "\nspec:\n  running: true\n  pod:\n    spec:\n" +
			"      containers:\n        - name: dev\n          image: " + image + "\n          env:\n            - {name: LEADER, value: '${LEADER}'}\n" +
			"          resources:\n            limits: {cpu: '" + cpu + "', memory: 1Gi}\n"
	}
	save := func(id, content string, decls ...map[string]any) {
		t.Helper()
		body := map[string]any{"id": id, "title": id, "content": content, "sharedWith": []string{"rb-bob"}}
		if len(decls) > 0 {
			body["variables"] = decls
		}
		call("POST", "/api/configs", alice, jsonBody(body), http.StatusCreated)
	}
	edit := func(id, content string, decls ...map[string]any) {
		t.Helper()
		body := map[string]any{"content": content}
		if len(decls) > 0 {
			body["variables"] = decls
		}
		call("PATCH", "/api/configs/rb-alice-"+id, alice, jsonBody(body), http.StatusOK)
	}
	launch := func(id string, vars map[string]string, count int) {
		t.Helper()
		created := launchItems(t, call("POST", "/api/configs/rb-alice-"+id+"/launch", bob, jsonBody(map[string]any{"vars": vars, "count": count}), http.StatusOK))
		for _, it := range created {
			waitDevPod(t, it.Name)
		}
	}
	rebuild := func(name string, auth any, body string, want int) map[string]any {
		t.Helper()
		return call("POST", "/api/devpods/"+name+"/rebuild-from-config", auth, body, want)
	}
	outdated := func(name string, auth any) any {
		t.Helper()
		return call("GET", "/api/devpods/"+name, auth, "", http.StatusOK)["configOutdated"]
	}
	image := func(name string) string {
		t.Helper()
		dp := waitDevPod(t, name)
		return dp.Spec.Pod.Spec.Containers[0].Image
	}
	env := func(dp devpodv1alpha1.DevPod, name string) string {
		for _, e := range dp.Spec.Pod.Spec.Containers[0].Env {
			if e.Name == name {
				return e.Value
			}
		}
		return "<unset>"
	}

	group := text("${NAME}", "${IMAGE:-ubuntu:24.04}", "1")
	save("g", group)
	launch("g", map[string]string{"NAME": "g"}, 2)

	t.Run("outdated", func(t *testing.T) {
		if got := outdated("rb-bob-g-0", bob); got != false {
			t.Fatalf("fresh: configOutdated %v", got)
		}
		group = text("${NAME}", "${IMAGE:-debian:12}", "1")
		edit("g", group)
		if got := outdated("rb-bob-g-0", bob); got != true {
			t.Fatalf("after an edit: configOutdated %v", got)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		v := rebuild("rb-bob-g-0", bob, `{"dryRun":true}`, http.StatusOK)
		if y, _ := v["yaml"].(string); !strings.Contains(y, "debian:12") {
			t.Fatalf("dry run %+v", v)
		}
		if got := image("rb-bob-g-0"); got != "ubuntu:24.04" {
			t.Fatalf("a dry run changed the image to %s", got)
		}
	})

	t.Run("apply", func(t *testing.T) {
		rebuild("rb-bob-g-0", bob, `{}`, http.StatusOK)
		if got := image("rb-bob-g-0"); got != "debian:12" {
			t.Fatalf("image %s", got)
		}
		if dp := waitDevPod(t, "rb-bob-g-0"); dp.Annotations["devpod.io/config-hash"] != hash(group) {
			t.Fatalf("hash %q, want %q", dp.Annotations["devpod.io/config-hash"], hash(group))
		}
		if got := outdated("rb-bob-g-0", bob); got != false {
			t.Fatalf("after a rebuild: configOutdated %v", got)
		}
		// only a comment changes: nothing in the spec moves, the hash still does
		group += "# a note\n"
		edit("g", group)
		rebuild("rb-bob-g-0", bob, `{}`, http.StatusOK)
		if got := outdated("rb-bob-g-0", bob); got != false {
			t.Fatalf("after a comment-only rebuild: configOutdated %v", got)
		}
	})

	t.Run("group member", func(t *testing.T) {
		call("DELETE", "/api/devpods/rb-bob-g-0", bob, "", http.StatusNoContent)
		rebuild("rb-bob-g-1", bob, `{}`, http.StatusOK)
		if dp := waitDevPod(t, "rb-bob-g-1"); env(dp, "LEADER") != "rb-bob-g-0" || dp.Spec.Pod.Spec.Containers[0].Image != "debian:12" {
			t.Fatalf("LEADER %q image %s", env(dp, "LEADER"), dp.Spec.Pod.Spec.Containers[0].Image)
		}
	})

	t.Run("vars changed", func(t *testing.T) {
		vtext := text("${NAME}", "${IMAGE}", "1")
		save("v", vtext)
		launch("v", map[string]string{"NAME": "v", "IMAGE": "debian:12"}, 1)
		// the value given is no longer one of the choices
		edit("v", vtext, map[string]any{"name": "IMAGE", "type": "choice", "options": []string{"ubuntu:24.04"}})
		v := rebuild("rb-bob-v", bob, `{}`, http.StatusConflict)
		if v["code"] != "CONFIG_VARS" || !strings.Contains(jsonBody(v["detail"]), "IMAGE") {
			t.Fatalf("choice: %+v", v)
		}
		// a variable it needs that was never given
		edit("v", strings.Replace(vtext, "running: true", "running: true\n  shell: ${MODE}", 1))
		v = rebuild("rb-bob-v", bob, `{}`, http.StatusConflict)
		if v["code"] != "CONFIG_VARS" || !strings.Contains(jsonBody(v["detail"]), "MODE") {
			t.Fatalf("new variable: %+v", v)
		}
	})

	t.Run("renames", func(t *testing.T) {
		save("rn", text("${NAME}", "ubuntu:24.04", "1"))
		launch("rn", map[string]string{"NAME": "rn"}, 1)
		edit("rn", text("${NAME}-x", "ubuntu:24.04", "1"))
		if v := rebuild("rb-bob-rn", bob, `{}`, http.StatusConflict); v["code"] != "CONFIG_RENAMES" {
			t.Fatalf("%+v", v)
		}
	})

	t.Run("quota", func(t *testing.T) {
		save("q", text("${NAME}", "ubuntu:24.04", "${CPU:-1}"))
		launch("q", map[string]string{"NAME": "q"}, 1)
		edit("q", text("${NAME}", "ubuntu:24.04", "${CPU:-64}"))
		if v := rebuild("rb-bob-q", bob, `{}`, http.StatusConflict); v["code"] != "QUOTA_EXCEEDED" {
			t.Fatalf("%+v", v)
		}
	})

	t.Run("gone", func(t *testing.T) {
		call("DELETE", "/api/configs/rb-alice-rn", alice, "", http.StatusNoContent)
		if v := rebuild("rb-bob-rn", bob, `{}`, http.StatusConflict); v["code"] != "CONFIG_GONE" {
			t.Fatalf("deleted config: %+v", v)
		}
		call("POST", "/api/devpods", bob, jsonBody(map[string]any{"name": "plain", "image": "ubuntu:24.04", "cpu": "1", "memory": "1Gi"}), http.StatusCreated)
		waitDevPod(t, "rb-bob-plain")
		if v := rebuild("rb-bob-plain", bob, `{}`, http.StatusConflict); v["code"] != "CONFIG_GONE" {
			t.Fatalf("no config: %+v", v)
		}
	})

	t.Run("forged", func(t *testing.T) {
		// carol marks her own DevPod as made from a config she may not see
		forged := strings.Replace(text("rb-carol-f", "ubuntu:24.04", "1"), "metadata:\n", "metadata:\n  labels:\n    devpod.io/config: rb-alice-g\n", 1)
		forged = strings.Replace(forged, "${LEADER}", "x", 1)
		call("POST", "/api/devpods", carol, jsonBody(map[string]any{"yaml": forged}), http.StatusCreated)
		waitDevPod(t, "rb-carol-f")
		if got := call("GET", "/api/devpods/rb-carol-f", carol, "", http.StatusOK); got["configOutdated"] != nil {
			t.Fatalf("configOutdated for a config she cannot see: %v", got["configOutdated"])
		}
		if v := rebuild("rb-carol-f", carol, `{}`, http.StatusConflict); v["code"] != "CONFIG_GONE" {
			t.Fatalf("%+v", v)
		}
	})

	t.Run("a token needs both scopes", func(t *testing.T) {
		mint := func(scopes ...string) string {
			t.Helper()
			return call("POST", "/api/me/tokens", bob, jsonBody(map[string]any{"name": strings.Join(scopes, "+"), "scopes": scopes}), http.StatusCreated)["token"].(string)
		}
		rebuild("rb-bob-g-1", mint("devpods:update"), `{"dryRun":true}`, http.StatusForbidden)
		rebuild("rb-bob-g-1", mint("devpods:update", "configs:read"), `{"dryRun":true}`, http.StatusOK)
	})
}
