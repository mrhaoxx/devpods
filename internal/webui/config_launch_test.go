// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// launchYAML is a config body: name and extra are spliced in.
func launchYAML(name, cpu, extra string) string {
	return "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: " + name + "\nspec:\n  owner: ${OWNER}\n  running: true\n  pod:\n    spec:\n" + extra +
		"      containers:\n        - name: dev\n          image: ${IMAGE:-ubuntu:24.04}\n          resources:\n            limits: {cpu: '" + cpu + "', memory: 1Gi}\n"
}

const pinExtra = "      nodeSelector:\n        kubernetes.io/hostname: ${NODE}\n"

type launchItem struct {
	Index         int
	Name, Node    string
	OK            bool
	Code, Message string
	YAML          string
}

func launchItems(t *testing.T, body map[string]any) []launchItem {
	t.Helper()
	raw, _ := body["items"].([]any)
	var out []launchItem
	for _, x := range raw {
		m := x.(map[string]any)
		it := launchItem{Index: int(m["index"].(float64)), OK: m["ok"].(bool)}
		it.Name, _ = m["name"].(string)
		it.Node, _ = m["node"].(string)
		it.Code, _ = m["code"].(string)
		it.Message, _ = m["message"].(string)
		it.YAML, _ = m["yaml"].(string)
		out = append(out, it)
	}
	return out
}

func devpodExists(t *testing.T, name string) bool {
	t.Helper()
	var dp devpodv1alpha1.DevPod
	err := k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Name: name, Namespace: "devpods"}, &dp)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestConfigLaunch(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	for _, n := range []string{"cl-node-a", "cl-node-b"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}}
		if err := k8sClient.Create(ctx, node); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })
	}
	user := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "cl-bob"}}
	if err := k8sClient.Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, user); cleanupDevPods(t); cleanupConfigs(t) })
	alice, bob, carol := forge(sm, "cl-alice", false), forge(sm, "cl-bob", false), forge(sm, "cl-carol", false)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	call := func(method, path string, auth any, body string, want int) map[string]any {
		t.Helper()
		return apiCall(t, srv, method, path, auth, body, want)
	}
	save := func(id, content string) {
		t.Helper()
		call("POST", "/api/configs", alice, jsonBody(map[string]any{"id": id, "title": id, "content": content, "sharedWith": []string{"cl-bob"}}), http.StatusCreated)
	}
	launch := func(id string, auth any, req map[string]any, want int) []launchItem {
		t.Helper()
		return launchItems(t, call("POST", "/api/configs/cl-alice-"+id+"/launch", auth, jsonBody(req), want))
	}
	// created waits until the cache has the launched DevPods: the
	// cleanup that follows a subtest only deletes what the cache lists
	created := func(items []launchItem) {
		t.Helper()
		for _, it := range items {
			waitDevPod(t, it.Name)
		}
	}
	vars := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	save("plain", launchYAML("${NAME}", "1", ""))
	save("pinned", launchYAML("${NAME}", "1", pinExtra))
	save("indexed", launchYAML("${NAME}-n${INDEX}", "1", ""))
	save("literal", launchYAML("cl-bob-fixed", "1", ""))
	save("big", launchYAML("${NAME}", "3", ""))
	save("cpu", launchYAML("${NAME}", "${CPU}", ""))

	t.Run("one DevPod, owned by whoever launches", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		items := launch("plain", bob, map[string]any{"vars": vars("NAME", "sg")}, http.StatusOK)
		if len(items) != 1 || !items[0].OK || items[0].Name != "cl-bob-sg" || items[0].Index != 1 || items[0].YAML != "" {
			t.Fatalf("items %+v", items)
		}
		dp := waitDevPod(t, "cl-bob-sg")
		if dp.Spec.Owner != "cl-bob" || dp.Spec.Pod.Spec.Containers[0].Image != "ubuntu:24.04" {
			t.Fatalf("created %+v", dp.Spec)
		}
		// the same name again is refused before anything is written
		items = launch("plain", bob, map[string]any{"vars": vars("NAME", "sg")}, http.StatusUnprocessableEntity)
		if len(items) != 1 || items[0].OK || items[0].Code != "NAME_TAKEN" {
			t.Fatalf("second launch %+v", items)
		}
	})

	t.Run("a count: names get the index", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		items := launch("plain", bob, map[string]any{"vars": vars("NAME", "w", "IMAGE", "debian:12"), "count": 3}, http.StatusOK)
		if len(items) != 3 || items[0].Name != "cl-bob-w-1" || items[1].Name != "cl-bob-w-2" || items[2].Name != "cl-bob-w-3" || items[2].Index != 3 {
			t.Fatalf("items %+v", items)
		}
		created(items)
		if dp := waitDevPod(t, "cl-bob-w-2"); dp.Spec.Pod.Spec.Containers[0].Image != "debian:12" {
			t.Fatalf("image %s", dp.Spec.Pod.Spec.Containers[0].Image)
		}
	})

	t.Run("a config that places the index itself", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		items := launch("indexed", bob, map[string]any{"vars": vars("NAME", "x"), "count": 2}, http.StatusOK)
		if len(items) != 2 || items[0].Name != "cl-bob-x-n1" || items[1].Name != "cl-bob-x-n2" {
			t.Fatalf("items %+v", items)
		}
		created(items)
	})

	t.Run("a literal name cannot be launched twice at once", func(t *testing.T) {
		items := launch("literal", bob, map[string]any{"count": 2}, http.StatusUnprocessableEntity)
		if len(items) != 2 || items[0].Code != "DUPLICATE_NAME" || items[1].Code != "DUPLICATE_NAME" {
			t.Fatalf("items %+v", items)
		}
		if devpodExists(t, "cl-bob-fixed") {
			t.Fatal("a rejected launch created a DevPod")
		}
	})

	t.Run("nodes: one DevPod each, in the order given", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		items := launch("pinned", bob, map[string]any{"vars": vars("NAME", "p"), "nodes": []string{"cl-node-b", "cl-node-a"}}, http.StatusOK)
		if len(items) != 2 || items[0].Node != "cl-node-b" || items[1].Node != "cl-node-a" || items[0].Name != "cl-bob-p-1" {
			t.Fatalf("items %+v", items)
		}
		created(items)
		if dp := waitDevPod(t, "cl-bob-p-2"); dp.Spec.Pod.Spec.NodeSelector["kubernetes.io/hostname"] != "cl-node-a" {
			t.Fatalf("pin %v", dp.Spec.Pod.Spec.NodeSelector)
		}
		// a node that is not there fails that instance, and so the launch
		items = launch("pinned", bob, map[string]any{"vars": vars("NAME", "q"), "nodes": []string{"cl-node-a", "cl-node-zz"}}, http.StatusUnprocessableEntity)
		if len(items) != 2 || !items[0].OK || items[1].OK || !strings.Contains(items[1].Message, "cl-node-zz") {
			t.Fatalf("items %+v", items)
		}
		if devpodExists(t, "cl-bob-q-1") {
			t.Fatal("a rejected launch created a DevPod")
		}
	})

	t.Run("what a request may not say", func(t *testing.T) {
		bad := func(id string, req map[string]any) {
			t.Helper()
			call("POST", "/api/configs/cl-alice-"+id+"/launch", bob, jsonBody(req), http.StatusBadRequest)
		}
		bad("pinned", map[string]any{"vars": vars("NAME", "p"), "count": 2})                    // nodes say how many
		bad("pinned", map[string]any{"vars": vars("NAME", "p")})                                // no node
		bad("plain", map[string]any{"vars": vars("NAME", "p"), "nodes": []string{"cl-node-a"}}) // no ${NODE} to fill
		bad("plain", map[string]any{"vars": vars("NAME", "p"), "count": 33})                    // past 32
		bad("plain", map[string]any{"vars": vars("NAME", "p"), "count": -1})
		bad("plain", map[string]any{"vars": vars("NAME", "p", "OWNER", "cl-alice")}) // filled by the server
		bad("plain", map[string]any{"vars": vars("NAME", "p", "INDEX", "7")})
		bad("pinned", map[string]any{"vars": vars("NAME", "p", "NODE", "cl-node-a"), "nodes": []string{"cl-node-a"}})
		bad("plain", map[string]any{"vars": vars()})                             // NAME missing
		bad("plain", map[string]any{"vars": vars("NAME", "p", "NOPE", "1")})     // not in the config
		bad("literal", map[string]any{"vars": vars("NAME", "p")})                // the config has no ${NAME}
		bad("plain", map[string]any{"vars": vars("NAME", "p", "IMAGE", "a\nb")}) // a line break
		bad("plain", map[string]any{"vars": vars("NAME", "p"), "bogus": true})
		got := call("POST", "/api/configs/cl-alice-cpu/launch", bob, jsonBody(map[string]any{"vars": vars("NAME", "p")}), http.StatusBadRequest)
		if b := jsonBody(got); !strings.Contains(b, "CPU") {
			t.Fatalf("the missing variable is not named: %s", b)
		}
	})

	t.Run("a name the cluster would refuse is caught before anything is written", func(t *testing.T) {
		items := launch("plain", bob, map[string]any{"vars": vars("NAME", "Bad_Name")}, http.StatusUnprocessableEntity)
		if len(items) != 1 || items[0].Code != "BAD_NAME" {
			t.Fatalf("items %+v", items)
		}
		items = launch("plain", bob, map[string]any{"vars": vars("NAME", strings.Repeat("a", 30))}, http.StatusUnprocessableEntity)
		if len(items) != 1 || items[0].Code != "BAD_NAME" {
			t.Fatalf("long name %+v", items)
		}
	})

	t.Run("the batch is checked against the quota as a whole", func(t *testing.T) {
		// the default quota is 8 CPUs: three of 3 do not fit
		items := launch("big", bob, map[string]any{"vars": vars("NAME", "b"), "count": 3}, http.StatusUnprocessableEntity)
		if len(items) != 3 || !items[0].OK || !items[1].OK || items[2].Code != "QUOTA_EXCEEDED" {
			t.Fatalf("items %+v", items)
		}
		if devpodExists(t, "cl-bob-b-1") {
			t.Fatal("a rejected launch created a DevPod")
		}
	})

	t.Run("a dry run shows the YAML and creates nothing", func(t *testing.T) {
		items := launch("pinned", bob, map[string]any{"vars": vars("NAME", "d"), "nodes": []string{"cl-node-a"}, "dryRun": true}, http.StatusOK)
		if len(items) != 1 || !items[0].OK || items[0].Name != "cl-bob-d" ||
			!strings.Contains(items[0].YAML, "name: cl-bob-d\n") || !strings.Contains(items[0].YAML, "owner: cl-bob\n") || !strings.Contains(items[0].YAML, "hostname: cl-node-a\n") {
			t.Fatalf("items %+v", items)
		}
		if devpodExists(t, "cl-bob-d") {
			t.Fatal("a dry run created a DevPod")
		}
	})

	t.Run("a value that breaks the YAML names the instance and the parser's complaint", func(t *testing.T) {
		items := launch("cpu", bob, map[string]any{"vars": vars("NAME", "y", "CPU", "1', oops: [")}, http.StatusUnprocessableEntity)
		if len(items) != 1 || items[0].OK || items[0].Code != "BAD_REQUEST" || !strings.Contains(items[0].Message, "YAML") || items[0].YAML == "" {
			t.Fatalf("items %+v", items)
		}
		if devpodExists(t, "cl-bob-y") {
			t.Fatal("created")
		}
		// a value the DevPod rules refuse: not a quantity
		items = launch("cpu", bob, map[string]any{"vars": vars("NAME", "y", "CPU", "abc")}, http.StatusUnprocessableEntity)
		if len(items) != 1 || items[0].OK {
			t.Fatalf("items %+v", items)
		}
	})

	t.Run("a name taken between the check and the write fails that one only", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		r := httptest.NewRequest("POST", "/api/configs/cl-alice-plain/launch", nil)
		plan, code := s.PlanLaunchForTest(r, "cl-bob", "cl-alice-plain", webui.LaunchRequest{Vars: vars("NAME", "race"), Count: 3})
		if code != "" {
			t.Fatalf("plan: %s", code)
		}
		// someone creates the second name meanwhile
		items := launch("plain", bob, map[string]any{"vars": vars("NAME", "race-2")}, http.StatusOK)
		if len(items) != 1 || items[0].Name != "cl-bob-race-2" {
			t.Fatalf("the intruder %+v", items)
		}
		created(items)
		got, status := s.RunLaunchForTest(r, plan)
		if status != http.StatusMultiStatus || len(got) != 3 || !got[0].OK || got[1].OK || got[1].Code != "NAME_TAKEN" || !got[2].OK {
			t.Fatalf("status %d items %+v", status, got)
		}
		waitDevPod(t, "cl-bob-race-1")
		waitDevPod(t, "cl-bob-race-3")
	})

	t.Run("a config taken away is not there to launch", func(t *testing.T) {
		call("POST", "/api/configs/cl-alice-plain/launch", carol, jsonBody(map[string]any{"vars": vars("NAME", "c")}), http.StatusNotFound)
		call("PATCH", "/api/configs/cl-alice-plain", alice, `{"sharedWith":[]}`, http.StatusOK)
		call("POST", "/api/configs/cl-alice-plain/launch", bob, jsonBody(map[string]any{"vars": vars("NAME", "c")}), http.StatusNotFound)
		if devpodExists(t, "cl-bob-c") {
			t.Fatal("created")
		}
		call("PATCH", "/api/configs/cl-alice-plain", alice, `{"sharedWith":["cl-bob"]}`, http.StatusOK)
	})

	t.Run("a token needs both scopes", func(t *testing.T) {
		t.Cleanup(func() { cleanupDevPods(t) })
		mint := func(scopes ...string) string {
			t.Helper()
			return call("POST", "/api/me/tokens", bob, jsonBody(map[string]any{"name": strings.Join(scopes, "+"), "scopes": scopes}), http.StatusCreated)["token"].(string)
		}
		req := jsonBody(map[string]any{"vars": vars("NAME", "tk")})
		call("POST", "/api/configs/cl-alice-plain/launch", mint("configs:read"), req, http.StatusForbidden)
		call("POST", "/api/configs/cl-alice-plain/launch", mint("devpods:create"), req, http.StatusForbidden)
		call("POST", "/api/configs/cl-alice-plain/launch", mint("configs:read", "devpods:create"), req, http.StatusOK)
		waitDevPod(t, "cl-bob-tk")
	})
}
