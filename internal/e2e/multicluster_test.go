// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package e2e runs the real programs against private API servers: the
// console (devpod-webui --clusters) and two agents (devpod-agent), built
// from this tree, over real HTTP and WebSockets. It needs nothing of the
// cluster it happens to run in.
//
//	DEVPOD_E2E=1 go test ./internal/e2e/ -run MultiCluster -v
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

type apiServer struct {
	c          client.Client
	kubeconfig string // an administrator's
	env        *envtest.Environment
}

// restrictedAgent gives the cluster an agent identity with exactly what
// the chart grants (testdata/agent-rbac.yaml, kept equal to the chart's
// render by hack/test-chart.sh) and returns a kubeconfig for it.
func restrictedAgent(t *testing.T, env *envtest.Environment, c client.Client, dir, name string) string {
	t.Helper()
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "devpod-system"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "agent-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, doc := range strings.Split(string(raw), "\n---") {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			t.Fatalf("agent-rbac.yaml: %v", err)
		}
		if obj.GetKind() == "" {
			continue
		}
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("apply %s/%s: %v", obj.GetKind(), obj.GetName(), err)
		}
		n++
	}
	if n < 7 {
		t.Fatalf("agent-rbac.yaml holds %d objects", n)
	}
	user, err := env.AddUser(envtest.User{Name: "system:serviceaccount:devpod-system:devpod-agent", Groups: []string{"system:serviceaccounts", "system:authenticated"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+"-agent.kubeconfig")
	if err := os.WriteFile(path, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startAPIServer(t *testing.T, scheme *runtime.Scheme, dir, name string) *apiServer {
	t.Helper()
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest %s: %v", name, err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	user, err := env.AddUser(envtest.User{Name: "e2e", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".kubeconfig")
	if err := os.WriteFile(path, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "devpods"}}); err != nil {
		t.Fatal(err)
	}
	return &apiServer{c: c, kubeconfig: path, env: env}
}

// proc is a running program whose output is kept for the failure report.
type proc struct {
	name string
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  bytes.Buffer
}

func (p *proc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.Write(b)
}

func (p *proc) log() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.out.String()
	if len(s) > 4000 {
		s = "…" + s[len(s)-4000:]
	}
	return s
}

func (p *proc) stop() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = p.cmd.Process.Kill()
		}
	}
}

func start(t *testing.T, name, bin string, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{name: name, cmd: exec.Command(bin, args...)}
	p.cmd.Env = append(os.Environ(), env...)
	p.cmd.Stdout, p.cmd.Stderr = p, p
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			t.Logf("---- %s ----\n%s", name, p.log())
		}
	})
	return p
}

func TestMultiCluster(t *testing.T) {
	if os.Getenv("DEVPOD_E2E") == "" {
		t.Skip("set DEVPOD_E2E=1 to build and run the console and two agents")
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
	dir := t.TempDir()
	ctx := context.Background()

	// the programs, from this tree
	for _, prog := range []string{"webui", "agent"} {
		build := exec.Command("go", "build", "-o", filepath.Join(dir, prog), "./cmd/"+prog)
		build.Dir = filepath.Join("..", "..")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", prog, err, out)
		}
	}

	// two clusters; a is also where the console keeps its own objects
	a, b := startAPIServer(t, scheme, dir, "a"), startAPIServer(t, scheme, dir, "b")

	// cluster b's agent is given what the chart gives it, and no more
	b.kubeconfig = restrictedAgent(t, b.env, b.c, dir, "b")

	// an admin who signs in with a password
	hash, err := webui.HashPassword("correct horse battery", 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.c.Create(ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: devpodv1alpha1.UserSpec{PasswordHash: hash}}); err != nil {
		t.Fatal(err)
	}

	key := make([]byte, 48)
	_, _ = rand.Read(key)
	keyFile := filepath.Join(dir, "session-key")
	if err := os.WriteFile(keyFile, []byte(fmt.Sprintf("%x", key)), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	base := "http://" + addr
	startConsole := func() *proc {
		return start(t, "console", filepath.Join(dir, "webui"), []string{"KUBECONFIG=" + a.kubeconfig},
			"--listen="+addr, "--session-key-file="+keyFile, "--password-auth=true", "--admins=root",
			"--devpod-namespace=devpods", "--kore=off", "--clusters=true", "--instance=console-0")
	}
	console := startConsole()

	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	call := func(method, path, body string) (int, map[string]any, http.Header) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := hc.Do(req)
		if err != nil {
			return 0, map[string]any{"error": err.Error()}, nil
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out, resp.Header
	}
	until := func(what string, d time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s", what)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	login := func() {
		t.Helper()
		until("the console is up and signs the admin in", 90*time.Second, func() bool {
			code, _, _ := call("POST", "/api/auth/password", `{"username":"root","password":"correct horse battery"}`)
			return code == http.StatusOK || code == http.StatusNoContent
		})
	}
	login()
	state := func(name string) map[string]any {
		_, out, _ := call("GET", "/api/admin/clusters", "")
		list, _ := out["items"].([]any)
		for _, it := range list {
			if m, _ := it.(map[string]any); m["name"] == name {
				return m
			}
		}
		return nil
	}
	connected := func(name string) bool { m := state(name); return m != nil && m["connected"] == true }

	// register both clusters; each answers a join token once
	tokens := map[string]string{}
	for _, name := range []string{"a", "b"} {
		code, out, _ := call("POST", "/api/admin/clusters", `{"name":"`+name+`"}`)
		tok, _ := out["joinToken"].(string)
		if code != http.StatusCreated || tok == "" {
			t.Fatalf("register %s: %d %v", name, code, out)
		}
		tokens[name] = filepath.Join(dir, name+".token")
		if err := os.WriteFile(tokens[name], []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	startAgent := func(name string, srv *apiServer, withToken bool) *proc {
		args := []string{"--cluster=" + name, "--console=" + base, "--kubeconfig=" + srv.kubeconfig,
			"--identity-file=" + filepath.Join(dir, name+".identity"), "--namespace=devpod-system", "--capabilities=snapshots"}
		if withToken {
			args = append(args, "--join-token-file="+tokens[name])
		}
		return start(t, "agent-"+name, filepath.Join(dir, "agent"), nil, args...)
	}
	startAgent("a", a, true)
	agentB := startAgent("b", b, true)
	until("both agents joined and connected", 60*time.Second, func() bool { return connected("a") && connected("b") })
	if m := state("b"); m["joined"] != true || m["kubernetesVersion"] == nil || m["kubernetesVersion"] == "" {
		t.Fatalf("what the console knows of b: %v", m)
	}
	// the token is spent: nobody else joins with it
	spent, _ := os.ReadFile(tokens["b"])
	body, _ := json.Marshal(map[string]string{"cluster": "b", "token": strings.TrimSpace(string(spent)), "publicKey": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFzmR0Dk5Lk5gGk4Sx0dH5ZyqVQk1o3o8o4S0r9o6m0f x"})
	if code, out, _ := call("POST", "/agent/join", string(body)); code != http.StatusForbidden {
		t.Fatalf("a second join with a spent token: %d %v", code, out)
	}

	// with the chart's rights the console can do its work in b: its cache
	// of b syncs (every kind it keeps), and what it reads there answers
	until("the console has b's cache", 90*time.Second, func() bool {
		code, _, _ := call("GET", "/api/clusters/b/devpods", "")
		return code == http.StatusOK
	})
	for _, path := range []string{"/api/clusters/b/templates", "/api/clusters/b/nodes", "/api/clusters/b/admin/nodes", "/api/clusters/b/admin/devpods"} {
		if code, out, _ := call("GET", path, ""); code != http.StatusOK {
			t.Fatalf("GET %s with the chart's rights: %d %v", path, code, out)
		}
	}
	// a registry credential lands in b as a User the agent may write
	if code, out, _ := call("PUT", "/api/me/registries/reg.example.com", `{"username":"root","password":"s3cret"}`); code != http.StatusOK {
		t.Fatalf("save a credential: %d %v", code, out)
	} else if cl, _ := out["clusters"].(map[string]any); cl["b"] != "ok" {
		t.Fatalf("the credential did not reach b: %v", out)
	}
	// and no further: with the agent's rights there are no Secrets to read,
	// and no Pods outside the DevPod namespace
	kc, err := os.ReadFile(b.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		t.Fatal(err)
	}
	asAgent, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := asAgent.List(ctx, &corev1.SecretList{}, client.InNamespace("devpods")); !apierrors.IsForbidden(err) {
		t.Fatalf("the agent listing Secrets in the DevPod namespace: %v", err)
	}
	if err := asAgent.List(ctx, &corev1.PodList{}, client.InNamespace("kube-system")); !apierrors.IsForbidden(err) {
		t.Fatalf("the agent listing Pods in another namespace: %v", err)
	}
	if err := asAgent.List(ctx, &corev1.PodList{}, client.InNamespace("devpods")); err != nil {
		t.Fatalf("the agent listing Pods in the DevPod namespace: %v", err)
	}

	// the same DevPod name in both clusters
	const mk = `{"name":"x","image":"ubuntu:24.04"}`
	for _, name := range []string{"a", "b"} {
		until("create in "+name, 60*time.Second, func() bool {
			code, out, _ := call("POST", "/api/clusters/"+name+"/devpods", mk)
			if code != http.StatusCreated && code != http.StatusServiceUnavailable && code != http.StatusConflict {
				t.Fatalf("create in %s: %d %v", name, code, out)
			}
			return code == http.StatusCreated
		})
	}
	for name, srv := range map[string]*apiServer{"a": a, "b": b} {
		var dp devpodv1alpha1.DevPod
		if err := srv.c.Get(ctx, types.NamespacedName{Name: "root-x", Namespace: "devpods"}, &dp); err != nil {
			t.Fatalf("the DevPod is not in cluster %s's API server: %v", name, err)
		}
	}
	until("the list covers both", 30*time.Second, func() bool {
		_, out, _ := call("GET", "/api/devpods", "")
		list, _ := out["items"].([]any)
		return len(list) == 2
	})
	if code, out, _ := call("GET", "/api/devpods/root-x", ""); code != http.StatusConflict || out["code"] != "AMBIGUOUS_CLUSTER" {
		t.Fatalf("a name both clusters have: %d %v", code, out)
	}

	// agent b stops: last known state, marked; no changes
	agentB.stop()
	until("b is shown as away", 60*time.Second, func() bool { return !connected("b") })
	code, out, hdr := call("GET", "/api/clusters/b/devpods", "")
	if list, _ := out["items"].([]any); code != http.StatusOK || len(list) != 1 || hdr.Get("X-DevPod-Stale-Since") == "" {
		t.Fatalf("b's DevPods while its agent is away: %d %v stale=%q", code, out, hdr.Get("X-DevPod-Stale-Since"))
	}
	if code, out, _ := call("PATCH", "/api/clusters/b/devpods/root-x", `{"running":false}`); code != http.StatusServiceUnavailable || out["code"] != "CLUSTER_OFFLINE" {
		t.Fatalf("a change to b while away: %d %v", code, out)
	}
	if code, out, _ := call("PATCH", "/api/clusters/a/devpods/root-x", `{"running":false}`); code != http.StatusOK {
		t.Fatalf("a change to a meanwhile: %d %v", code, out)
	}

	// it comes back with its own key, no token
	agentB = startAgent("b", b, false)
	until("b reconnects by its key", 60*time.Second, func() bool { return connected("b") })
	until("b takes changes again", 60*time.Second, func() bool {
		code, _, _ := call("PATCH", "/api/clusters/b/devpods/root-x", `{"running":false}`)
		return code == http.StatusOK
	})
	var dp devpodv1alpha1.DevPod
	if err := b.c.Get(ctx, types.NamespacedName{Name: "root-x", Namespace: "devpods"}, &dp); err != nil || dp.Spec.Running {
		t.Fatalf("the change did not reach cluster b: running=%v err=%v", dp.Spec.Running, err)
	}

	// the console restarts: the agents find it again by themselves
	console.stop()
	console = startConsole()
	login()
	until("both agents are back after the console restarted", 90*time.Second, func() bool { return connected("a") && connected("b") })
	until("and the list is whole again", 60*time.Second, func() bool {
		_, out, _ := call("GET", "/api/devpods", "")
		list, _ := out["items"].([]any)
		return len(list) == 2
	})

	// pausing a cluster drops its agent and keeps it out
	if code, out, _ := call("PATCH", "/api/admin/clusters/b", `{"paused":true}`); code != http.StatusOK {
		t.Fatalf("pause b: %d %v", code, out)
	}
	until("b is disconnected", 30*time.Second, func() bool { return !connected("b") })
	time.Sleep(3 * time.Second) // the agent keeps trying
	if connected("b") {
		t.Fatal("a paused cluster's agent got back in")
	}
	if !strings.Contains(agentB.log(), "the cluster is paused") {
		t.Fatalf("the agent's log does not say why it is refused:\n%s", agentB.log())
	}
	if code, _, _ := call("PATCH", "/api/admin/clusters/b", `{"paused":false}`); code != http.StatusOK {
		t.Fatal("resume b")
	}
	until("b returns once resumed", 90*time.Second, func() bool { return connected("b") })
	_ = console
}
