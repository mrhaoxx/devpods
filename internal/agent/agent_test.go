// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// hub is the console's end, in this process: one tunnel instance that
// accepts the agent under test.
type hub struct {
	srv *tunnel.Server
	ctx context.Context
}

func newHub(t *testing.T, ctx context.Context, cluster string, id *agent.Identity) *hub {
	t.Helper()
	h := &hub{ctx: ctx}
	h.srv = tunnel.NewServer(tunnel.ServerOptions{
		Instance: "hub-0", Instances: func() []string { return []string{"hub-0"} },
		Authorize: func(_ context.Context, hello tunnel.Hello, nonce, proof []byte) (string, error) {
			if hello.Kind != agent.Kind || hello.Name != cluster {
				return "", context.Canceled
			}
			if err := agent.Verify(id.PublicKey(), nonce, proof); err != nil {
				return "", err
			}
			return hello.Name, nil
		},
		Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond,
	})
	return h
}

func (h *hub) dial(context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	go h.srv.Accept(h.ctx, b)
	return a, nil
}

// restConfig is how the console reaches the cluster: every connection of
// the Kubernetes client is a kube stream through the agent.
func (h *hub) restConfig(cluster string) *rest.Config {
	return &rest.Config{
		Host: "http://" + cluster,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return h.srv.Dial(ctx, cluster, agent.PurposeKube)
		},
	}
}

func waitPeer(t *testing.T, h *hub, cluster string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, ok := h.srv.Peers()[cluster]; ok && p.Idle > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never connected")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startAgent(t *testing.T, ctx context.Context, h *hub, cluster string, id *agent.Identity, api *rest.Config, info func(context.Context) agent.Info) {
	t.Helper()
	a, err := agent.New(agent.Options{Cluster: cluster, Identity: id, API: api, Info: info, InfoInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = a.Run(ctx, tunnel.ClientOptions{Dial: h.dial, Idle: 4, Heartbeat: 50 * time.Millisecond, Dead: 500 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 50 * time.Millisecond})
	}()
	waitPeer(t, h, cluster)
}

func TestIdentity(t *testing.T) {
	dir := t.TempDir()
	first, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(dir, "id")))
	if err != nil {
		t.Fatal(err)
	}
	again, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(dir, "id")))
	if err != nil || again.PublicKey() != first.PublicKey() {
		t.Fatalf("the second start has another key: %v", err)
	}
	other, _ := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(dir, "other")))
	nonce := []byte("nonce-1")
	if err := agent.Verify(first.PublicKey(), nonce, first.Sign(nonce)); err != nil {
		t.Fatalf("own signature: %v", err)
	}
	if err := agent.Verify(first.PublicKey(), nonce, other.Sign(nonce)); err == nil {
		t.Fatal("another key's signature accepted")
	}
	if err := agent.Verify(first.PublicKey(), []byte("nonce-2"), first.Sign(nonce)); err == nil {
		t.Fatal("a signature over another nonce accepted")
	}
	if err := agent.Verify("ssh-rsa AAAA", nonce, first.Sign(nonce)); err == nil {
		t.Fatal("a key that is not an Ed25519 key accepted")
	}
}

// What the console sends as identity does not reach the API server: in
// the cluster, the console is the agent's service account.
func TestKube_IdentityHeadersAreDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var seen []http.Header
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Success"}`))
	}))
	defer api.Close()
	id, _ := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	h := newHub(t, ctx, "c1", id)
	startAgent(t, ctx, h, "c1", id, &rest.Config{Host: api.URL, BearerToken: "the-agents-own-token"}, nil)

	hc := &http.Client{Transport: &http.Transport{DialContext: h.restConfig("c1").Dial}}
	req, _ := http.NewRequest("GET", "http://c1/api/v1/namespaces", nil)
	req.Header.Set("Authorization", "Bearer smuggled")
	req.Header.Set("Impersonate-User", "system:admin")
	req.Header.Add("Impersonate-Group", "system:masters")
	req.Header.Set("Impersonate-Extra-Scopes", "x")
	req.Header.Set("X-Kept", "yes")
	resp, err := hc.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("request through the agent: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("%d requests reached the API", len(seen))
	}
	got := seen[0]
	if got.Get("Authorization") != "Bearer the-agents-own-token" {
		t.Fatalf("Authorization at the API: %q", got.Get("Authorization"))
	}
	for name := range got {
		if len(name) >= 11 && name[:11] == "Impersonate" {
			t.Fatalf("%s reached the API: %v", name, got[name])
		}
	}
	if got.Get("X-Kept") != "yes" {
		t.Fatal("an ordinary header was dropped")
	}
}

func TestKube_AClientThroughTheAgent(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, _ := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	h := newHub(t, ctx, "c1", id)
	startAgent(t, ctx, h, "c1", id, cfg, nil)

	c, err := client.NewWithWatch(h.restConfig("c1"), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "devpods"}}); err != nil {
		t.Fatalf("create a namespace through the agent: %v", err)
	}
	dp := func(name string) *devpodv1alpha1.DevPod {
		return &devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
			Spec: devpodv1alpha1.DevPodSpec{Owner: "alice", Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04"}},
			}}},
		}
	}
	if err := c.Create(ctx, dp("alice-a")); err != nil {
		t.Fatalf("create a DevPod: %v", err)
	}
	var list devpodv1alpha1.DevPodList
	if err := c.List(ctx, &list, client.InNamespace("devpods")); err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %d %v", len(list.Items), err)
	}
	// a watch stays open and delivers what happens afterwards
	w, err := c.Watch(ctx, &devpodv1alpha1.DevPodList{}, client.InNamespace("devpods"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if err := c.Create(ctx, dp("alice-b")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-w.ResultChan():
			if d, ok := ev.Object.(*devpodv1alpha1.DevPod); ok && d.Name == "alice-b" {
				return
			}
		case <-deadline:
			t.Fatal("the watch never delivered the new DevPod")
		}
	}
}

func TestInfo_SaidAtOnceAndAgainWhenItChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()
	var mu sync.Mutex
	caps := []string{"snapshots"}
	info := func(context.Context) agent.Info {
		mu.Lock()
		defer mu.Unlock()
		return agent.Info{AgentVersion: "v9", ControllerVersion: "v8", KubernetesVersion: "v1.36.4", Capabilities: append([]string(nil), caps...)}
	}
	id, _ := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	h := newHub(t, ctx, "c1", id)
	startAgent(t, ctx, h, "c1", id, &rest.Config{Host: api.URL}, info)

	conn, err := h.srv.Dial(ctx, "c1", agent.PurposeInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	dec := json.NewDecoder(conn)
	var got agent.Info
	if err := dec.Decode(&got); err != nil || got.Cluster != "c1" || got.AgentVersion != "v9" || got.ControllerVersion != "v8" || len(got.Capabilities) != 1 {
		t.Fatalf("first info %+v %v", got, err)
	}
	mu.Lock()
	caps = []string{"snapshots", "reservations"}
	mu.Unlock()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := dec.Decode(&got); err != nil || len(got.Capabilities) != 2 {
		t.Fatalf("info after a change %+v %v", got, err)
	}
	// nothing more while nothing changes
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if err := dec.Decode(&got); err == nil {
		t.Fatalf("info repeated without a change: %+v", got)
	}
}

func TestUnknownPurposeIsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()
	id, _ := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	h := newHub(t, ctx, "c1", id)
	startAgent(t, ctx, h, "c1", id, &rest.Config{Host: api.URL}, nil)
	conn, err := h.srv.Dial(ctx, "c1", []byte(`{"kind":"shell"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a stream of an unknown kind was served")
	}
}
