// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const checkYAML = `apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: ${NAME}
spec:
  running: true
  pod:
    spec:
      containers:
        - name: dev
          image: ubuntu:24.04
          resources:
            limits:
              cpu: "64"
              memory: 4Gi
              nvidia.com/gpu: ${GPU:-1}
`

func problems(t *testing.T, v map[string]any) []map[string]any {
	t.Helper()
	raw, ok := v["problems"].([]any)
	if !ok {
		t.Fatalf("no problems list: %+v", v)
	}
	var out []map[string]any
	for _, p := range raw {
		out = append(out, p.(map[string]any))
	}
	return out
}

func TestConfigCheck(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice, root := forge(sm, "cc-alice", false), forge(sm, "cc-root", true)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	check := func(auth any, body map[string]any) []map[string]any {
		t.Helper()
		return problems(t, apiCall(t, srv, "POST", "/api/configs:check", auth, jsonBody(body), http.StatusOK))
	}
	one := func(ps []map[string]any, code, says string) {
		t.Helper()
		if len(ps) != 1 || ps[0]["code"] != code || !strings.Contains(ps[0]["message"].(string), says) {
			t.Fatalf("want one %s saying %q, got %+v", code, says, ps)
		}
	}

	// fine — a quota it exceeds is not the check's business
	if ps := check(alice, map[string]any{"content": checkYAML}); len(ps) != 0 {
		t.Fatalf("problems in a good config: %+v", ps)
	}
	// a field that does not exist
	one(check(alice, map[string]any{"content": strings.Replace(checkYAML, "running: true", "runnning: true", 1)}), "BAD_YAML", "runnning")
	// what a user may not write; an admin may
	secret := checkYAML + "      volumes:\n        - name: s\n          secret:\n            secretName: x\n"
	if ps := check(alice, map[string]any{"content": secret}); len(ps) != 1 || ps[0]["code"] != "FORBIDDEN_CONTENT" {
		t.Fatalf("secret volume: %+v", ps)
	}
	if ps := check(root, map[string]any{"content": secret}); len(ps) != 0 {
		t.Fatalf("an admin's secret volume: %+v", ps)
	}
	// a name too long
	if ps := check(alice, map[string]any{"content": strings.Replace(checkYAML, "name: ${NAME}", "name: ${NAME}-this-name-is-far-too-long", 1)}); len(ps) != 1 || ps[0]["code"] != "BAD_NAME" {
		t.Fatalf("long name: %+v", ps)
	}
	// declarations that do not hold
	if ps := check(alice, map[string]any{"content": checkYAML, "variables": []map[string]any{{"name": "GPU", "type": "choice"}}}); len(ps) != 1 || ps[0]["code"] != "BAD_VARIABLES" {
		t.Fatalf("bad declaration: %+v", ps)
	}
	// a typed variable without a value takes an example of its type
	if ps := check(alice, map[string]any{"content": strings.Replace(checkYAML, "${GPU:-1}", "${GPU}", 1), "variables": []map[string]any{{"name": "GPU", "type": "int"}}}); len(ps) != 0 {
		t.Fatalf("int example: %+v", ps)
	}

	// it is a write: a token that only reads configs may not
	tok := apiCall(t, srv, "POST", "/api/me/tokens", alice, jsonBody(map[string]any{"name": "r", "scopes": []string{"configs:read"}}), http.StatusCreated)["token"].(string)
	apiCall(t, srv, "POST", "/api/configs:check", tok, jsonBody(map[string]any{"content": checkYAML}), http.StatusForbidden)
}
