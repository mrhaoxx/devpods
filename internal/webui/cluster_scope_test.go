// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// world is a console over two clusters, "a" and "b". The suite's API
// server is cluster a and also the console's own cluster (users, Cluster
// objects); b is a second API server. Each is reached through an
// in-process agent, as it would be from elsewhere.
type world struct {
	t     *testing.T
	s     *webui.Server
	sm    *webui.SessionManager
	h     http.Handler
	a, b  client.Client // straight to each cluster, around the console
	stopB context.CancelFunc
	// startB brings cluster b's agent back.
	startB func()
}

func setupWorld(t *testing.T) *world {
	t.Helper()
	setupSuite(t)
	envB := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfgB, err := envB.Start()
	if err != nil {
		t.Fatalf("envtest b: %v", err)
	}
	t.Cleanup(func() { _ = envB.Stop() })
	direct := func(cfg *rest.Config) client.Client {
		c, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	w := &world{t: t, a: direct(envTestEnv.Config), b: direct(cfgB)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.b.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "devpods"}}); err != nil {
		t.Fatal(err)
	}

	w.s, w.sm = newServer(t)
	reg := clusters.New(clusters.Options{
		Hub: k8sClient, Reader: k8sManager.GetAPIReader(), Instance: "console-0", StatusInterval: time.Hour,
		Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond,
		Scheme: scheme, Indexes: webui.RegisterIndexes,
		Warm: []client.Object{&devpodv1alpha1.DevPod{}, &corev1.Pod{}, &corev1.Node{}},
	})
	go reg.Run(ctx)
	w.s.Clusters = reg
	w.h = w.s.Routes()

	run := func(name string, id *agent.Identity, api *rest.Config) context.CancelFunc {
		a, err := agent.New(agent.Options{Cluster: name, Identity: id, API: api})
		if err != nil {
			t.Fatal(err)
		}
		actx, stop := context.WithCancel(ctx)
		go func() {
			_ = a.Run(actx, tunnel.ClientOptions{
				Idle: 4, Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond,
				BackoffMin: 5 * time.Millisecond, BackoffMax: 50 * time.Millisecond,
				Dial: func(context.Context) (net.Conn, error) {
					x, y := net.Pipe()
					go reg.Accept(actx, y)
					return x, nil
				},
			})
		}()
		return stop
	}
	ids := map[string]*agent.Identity{}
	for name, cfg := range map[string]*rest.Config{"a": envTestEnv.Config, "b": cfgB} {
		c := &devpodv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name}}
		token := clusters.IssueToken(c, time.Now())
		if err := w.a.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = w.a.Delete(context.Background(), &devpodv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})
		id, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
		if err := reg.Join(ctx, name, token, id.PublicKey()); err != nil {
			t.Fatal(err)
		}
		stop := run(name, id, cfg)
		if name == "b" {
			w.stopB = stop
			w.startB = func() { w.stopB = run("b", ids["b"], cfgB) }
		}
	}
	w.until("both clusters are there", func() bool {
		_, errA := reg.Clients("a")
		_, errB := reg.Clients("b")
		return errA == nil && errB == nil
	})
	return w
}

func (w *world) until(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("never: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// call makes a request to the console as user (admin when the name is
// "root") and returns the status, the decoded body and the headers.
func (w *world) call(user, method, path, body string) (int, map[string]any, http.Header) {
	w.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(forge(w.sm, user, user == "root"))
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

// items returns "cluster/name" of a list answer, sorted as listed.
func items(out map[string]any) []string {
	var got []string
	list, _ := out["items"].([]any)
	for _, it := range list {
		m, _ := it.(map[string]any)
		meta, _ := m["metadata"].(map[string]any)
		got = append(got, m["cluster"].(string)+"/"+meta["name"].(string))
	}
	return got
}

func (w *world) running(c client.Client, name string) bool {
	w.t.Helper()
	var dp devpodv1alpha1.DevPod
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "devpods"}, &dp); err != nil {
		w.t.Fatal(err)
	}
	return dp.Spec.Running
}

func (w *world) exists(c client.Client, name string) bool {
	var dp devpodv1alpha1.DevPod
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "devpods"}, &dp)
	if err != nil && !apierrors.IsNotFound(err) {
		w.t.Fatal(err)
	}
	return err == nil
}

func TestClusterScope(t *testing.T) {
	w := setupWorld(t)
	const mk = `{"name":"x","image":"ubuntu:24.04"}`

	// with two clusters, a new DevPod has to say where
	if code, out, _ := w.call("gl-alice", "POST", "/api/devpods", mk); code != http.StatusBadRequest || out["code"] != "CLUSTER_REQUIRED" {
		t.Fatalf("create without a cluster: %d %v", code, out)
	}
	if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/nope/devpods", mk); code != http.StatusNotFound || out["code"] != "UNKNOWN_CLUSTER" {
		t.Fatalf("create in a cluster that does not exist: %d %v", code, out)
	}
	// the same name in both
	for _, c := range []string{"a", "b"} {
		if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/"+c+"/devpods", mk); code != http.StatusCreated {
			t.Fatalf("create in %s: %d %v", c, code, out)
		}
	}
	if !w.exists(w.a, "gl-alice-x") || !w.exists(w.b, "gl-alice-x") {
		t.Fatal("the DevPods are not where they were created")
	}
	// ?cluster= says it as well as the path
	if code, out, _ := w.call("gl-alice", "POST", "/api/devpods?cluster=b", `{"name":"only","image":"ubuntu:24.04"}`); code != http.StatusCreated {
		t.Fatalf("create with ?cluster=: %d %v", code, out)
	}
	if w.exists(w.a, "gl-alice-only") || !w.exists(w.b, "gl-alice-only") {
		t.Fatal("?cluster=b did not create in b alone")
	}
	w.until("the caches have them", func() bool {
		_, out, _ := w.call("gl-alice", "GET", "/api/devpods", "")
		return len(items(out)) == 3
	})

	// the list covers every cluster, each item saying which
	_, out, hdr := w.call("gl-alice", "GET", "/api/devpods", "")
	if got := strings.Join(items(out), " "); got != "a/gl-alice-x b/gl-alice-only b/gl-alice-x" {
		t.Fatalf("aggregated list: %s", got)
	}
	if hdr.Get("X-DevPod-Stale-Since") != "" {
		t.Fatal("stale header while every cluster is connected")
	}
	_, out, _ = w.call("gl-alice", "GET", "/api/devpods?cluster=b", "")
	if got := strings.Join(items(out), " "); got != "b/gl-alice-only b/gl-alice-x" {
		t.Fatalf("?cluster=b: %s", got)
	}
	_, out, _ = w.call("gl-alice", "GET", "/api/clusters/a/devpods", "")
	if got := strings.Join(items(out), " "); got != "a/gl-alice-x" {
		t.Fatalf("/api/clusters/a/devpods: %s", got)
	}
	// someone else sees none of it; an admin sees all
	if _, out, _ := w.call("gl-bob", "GET", "/api/devpods", ""); len(items(out)) != 0 {
		t.Fatalf("another user's list: %v", items(out))
	}
	if _, out, _ := w.call("root", "GET", "/api/devpods?scope=all", ""); len(items(out)) != 3 {
		t.Fatalf("an admin's list of all: %v", items(out))
	}

	// what is about me, not about a cluster, needs no cluster
	if code, out, _ := w.call("gl-alice", "GET", "/api/me", ""); code != http.StatusOK {
		t.Fatalf("/api/me with two clusters: %d %v", code, out)
	} else if usage, _ := out["usage"].(map[string]any); usage["devpods"] != float64(3) {
		t.Fatalf("my usage over both clusters: %v", out["usage"])
	}
	// what is a cluster's has to say which
	if code, out, _ := w.call("gl-alice", "GET", "/api/templates", ""); code != http.StatusBadRequest || out["code"] != "CLUSTER_REQUIRED" {
		t.Fatalf("templates without a cluster: %d %v", code, out)
	} else if names, _ := out["detail"].([]any); len(names) != 2 {
		t.Fatalf("CLUSTER_REQUIRED names the clusters: %v", out)
	}
	if code, out, _ := w.call("gl-alice", "GET", "/api/clusters/b/templates", ""); code != http.StatusOK {
		t.Fatalf("templates of b: %d %v", code, out)
	}

	// a config is launched into a cluster: its names are checked there
	cfg, _ := json.Marshal(map[string]any{"id": "c1", "title": "t", "content": launchYAML("${NAME}", "1", "")})
	if code, out, _ := w.call("gl-alice", "POST", "/api/configs", string(cfg)); code != http.StatusCreated {
		t.Fatalf("save a config: %d %v", code, out)
	}
	t.Cleanup(func() {
		_ = w.a.Delete(context.Background(), &devpodv1alpha1.DevPodConfig{ObjectMeta: metav1.ObjectMeta{Name: "gl-alice-c1", Namespace: "devpods"}})
	})
	if code, out, _ := w.call("gl-alice", "POST", "/api/configs/gl-alice-c1/launch", `{"vars":{"NAME":"only"}}`); code != http.StatusBadRequest || out["code"] != "CLUSTER_REQUIRED" {
		t.Fatalf("launch without a cluster: %d %v", code, out)
	}
	if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/b/configs/gl-alice-c1/launch", `{"vars":{"NAME":"only"}}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("launch into b a name b already has: %d %v", code, out)
	}
	if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/a/configs/gl-alice-c1/launch", `{"vars":{"NAME":"only"},"dryRun":true}`); code != http.StatusOK {
		t.Fatalf("the same name is free in a: %d %v", code, out)
	}

	// by name alone: fine when one cluster has it, a question when two do
	code, out, _ := w.call("gl-alice", "GET", "/api/devpods/gl-alice-only", "")
	if code != http.StatusOK || out["cluster"] != "b" {
		t.Fatalf("a name only b has: %d %v", code, out)
	}
	code, out, _ = w.call("gl-alice", "GET", "/api/devpods/gl-alice-x", "")
	cands, _ := out["detail"].([]any)
	if code != http.StatusConflict || out["code"] != "AMBIGUOUS_CLUSTER" || len(cands) != 2 {
		t.Fatalf("a name both have: %d %v", code, out)
	}
	if first, _ := cands[0].(map[string]any); first["cluster"] != "a" || first["name"] != "gl-alice-x" {
		t.Fatalf("candidates: %v", cands)
	}
	if code, _, _ := w.call("gl-alice", "PATCH", "/api/devpods/gl-alice-x", `{"running":false}`); code != http.StatusConflict {
		t.Fatalf("a write by an ambiguous name: %d", code)
	}
	if !w.running(w.a, "gl-alice-x") || !w.running(w.b, "gl-alice-x") {
		t.Fatal("an ambiguous write changed something")
	}
	// someone else's name is not found, in whichever cluster
	if code, _, _ := w.call("gl-bob", "GET", "/api/devpods/gl-alice-x", ""); code != http.StatusNotFound {
		t.Fatalf("another user's DevPod by name: %d", code)
	}
	if code, _, _ := w.call("gl-bob", "GET", "/api/clusters/b/devpods/gl-alice-x", ""); code != http.StatusNotFound {
		t.Fatalf("another user's DevPod by cluster and name: %d", code)
	}

	// a write through one cluster's path touches that cluster alone
	if code, out, _ := w.call("gl-alice", "PATCH", "/api/clusters/b/devpods/gl-alice-x", `{"running":false}`); code != http.StatusOK {
		t.Fatalf("stop in b: %d %v", code, out)
	}
	if !w.running(w.a, "gl-alice-x") || w.running(w.b, "gl-alice-x") {
		t.Fatalf("after stopping b's: a running=%v b running=%v", w.running(w.a, "gl-alice-x"), w.running(w.b, "gl-alice-x"))
	}
	if code, _, _ := w.call("gl-alice", "GET", "/api/clusters/a/devpods/gl-alice-only", ""); code != http.StatusNotFound {
		t.Fatalf("b's DevPod asked of a: %d", code)
	}

	// quota is counted per cluster: b allows two, the console's default five
	var cb devpodv1alpha1.Cluster
	for i := 0; ; i++ {
		if err := w.a.Get(context.Background(), types.NamespacedName{Name: "b"}, &cb); err != nil {
			t.Fatal(err)
		}
		cb.Spec.DefaultQuota = &devpodv1alpha1.UserQuota{MaxDevPods: ptr.To(int32(2))}
		err := w.a.Update(context.Background(), &cb)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || i > 20 {
			t.Fatal(err)
		}
	}
	w.until("the console knows b's quota", func() bool {
		var c devpodv1alpha1.Cluster
		return k8sClient.Get(context.Background(), types.NamespacedName{Name: "b"}, &c) == nil && c.Spec.DefaultQuota != nil
	})
	if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/b/devpods", `{"name":"third","image":"ubuntu:24.04"}`); code != http.StatusConflict || out["code"] != "QUOTA_EXCEEDED" {
		t.Fatalf("a third DevPod in a cluster that allows two: %d %v", code, out)
	}
	if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/a/devpods", `{"name":"second","image":"ubuntu:24.04"}`); code != http.StatusCreated {
		t.Fatalf("a second DevPod in a, which allows five: %d %v", code, out)
	}

	// cluster b goes away: what was known of it is still shown, marked;
	// changes are refused; cluster a carries on
	w.stopB()
	w.until("b is stale", func() bool { _, stale := w.s.Clusters.Stale("b"); return stale })
	_, out, hdr = w.call("gl-alice", "GET", "/api/devpods", "")
	if got := strings.Join(items(out), " "); got != "a/gl-alice-second a/gl-alice-x b/gl-alice-only b/gl-alice-x" {
		t.Fatalf("the list while b is away: %s", got)
	}
	if _, err := time.Parse(time.RFC3339, hdr.Get("X-DevPod-Stale-Since")); err != nil {
		t.Fatalf("stale header %q", hdr.Get("X-DevPod-Stale-Since"))
	}
	if stale, _ := out["stale"].(map[string]any); stale["b"] == nil || stale["a"] != nil {
		t.Fatalf("which clusters are stale: %v", out["stale"])
	}
	if code, _, hdr := w.call("gl-alice", "GET", "/api/clusters/b/devpods/gl-alice-x", ""); code != http.StatusOK || hdr.Get("X-DevPod-Stale-Since") == "" {
		t.Fatalf("one DevPod of an away cluster: %d %q", code, hdr.Get("X-DevPod-Stale-Since"))
	}
	if _, _, hdr := w.call("gl-alice", "GET", "/api/clusters/a/devpods", ""); hdr.Get("X-DevPod-Stale-Since") != "" {
		t.Fatal("cluster a's list is marked stale")
	}
	for _, req := range [][2]string{
		{"PATCH", "/api/clusters/b/devpods/gl-alice-x"}, {"PATCH", "/api/devpods/gl-alice-only"},
		{"DELETE", "/api/clusters/b/devpods/gl-alice-x"}, {"POST", "/api/clusters/b/devpods"},
	} {
		start := time.Now()
		code, out, _ := w.call("gl-alice", req[0], req[1], `{"running":true,"name":"z","image":"ubuntu:24.04"}`)
		if code != http.StatusServiceUnavailable || out["code"] != "CLUSTER_OFFLINE" || time.Since(start) > time.Second {
			t.Fatalf("%s %s while b is away: %d %v after %v", req[0], req[1], code, out, time.Since(start))
		}
	}
	if code, _, _ := w.call("gl-alice", "PATCH", "/api/clusters/a/devpods/gl-alice-x", `{"running":false}`); code != http.StatusOK {
		t.Fatalf("a write to a while b is away: %d", code)
	}
	if w.running(w.a, "gl-alice-x") {
		t.Fatal("the write to a did not land")
	}

	// b returns
	w.startB()
	w.until("b takes writes again", func() bool {
		code, _, _ := w.call("gl-alice", "DELETE", "/api/clusters/b/devpods/gl-alice-only", "")
		return code == http.StatusNoContent
	})
	if w.exists(w.b, "gl-alice-only") {
		t.Fatal("the delete did not reach b")
	}
}
