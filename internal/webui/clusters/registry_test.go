// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package clusters_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// hubEnv is the console's own cluster: an API server holding the Cluster
// objects.
type hubEnv struct {
	c   client.Client
	cfg *rest.Config
}

func startHub(t *testing.T) *hubEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return &hubEnv{c: c, cfg: cfg}
}

func newRegistry(t *testing.T, ctx context.Context, h *hubEnv) *clusters.Registry {
	t.Helper()
	r := clusters.New(clusters.Options{
		Hub: h.c, Reader: h.c, Instance: "console-0",
		StatusInterval: time.Hour, // the tests sweep by hand
		Heartbeat:      30 * time.Millisecond, Dead: 300 * time.Millisecond,
	})
	go r.Run(ctx)
	return r
}

// register creates a Cluster with a join token, as an admin would.
func register(t *testing.T, ctx context.Context, h *hubEnv, name string) string {
	t.Helper()
	c := &devpodv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name}}
	token := clusters.IssueToken(c, time.Now())
	if err := h.c.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	return token
}

func newIdentity(t *testing.T) *agent.Identity {
	t.Helper()
	id, err := agent.LoadOrCreateIdentity(agent.FileStore(filepath.Join(t.TempDir(), "id")))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// runAgent starts an in-process agent for cluster name; the returned
// function stops it.
func runAgent(t *testing.T, ctx context.Context, r *clusters.Registry, name string, id *agent.Identity) context.CancelFunc {
	t.Helper()
	api := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(api.Close)
	a, err := agent.New(agent.Options{
		Cluster: name, Identity: id, API: &rest.Config{Host: api.URL},
		Info: func(context.Context) agent.Info {
			return agent.Info{AgentVersion: "v1.2.3", ControllerVersion: "v1.2.0", KubernetesVersion: "v1.36.4", Capabilities: []string{"snapshots"}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	actx, stop := context.WithCancel(ctx)
	go func() {
		_ = a.Run(actx, tunnel.ClientOptions{
			Idle: 2, Heartbeat: 30 * time.Millisecond, Dead: 300 * time.Millisecond,
			BackoffMin: 5 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
			Dial: func(context.Context) (net.Conn, error) {
				x, y := net.Pipe()
				go r.Accept(actx, y)
				return x, nil
			},
		})
	}()
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, ctx context.Context, h *hubEnv, name string) *devpodv1alpha1.Cluster {
	t.Helper()
	var c devpodv1alpha1.Cluster
	if err := h.c.Get(ctx, types.NamespacedName{Name: name}, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// change edits a Cluster as an admin would, again if the registry wrote
// its status in between.
func change(t *testing.T, ctx context.Context, h *hubEnv, name string, edit func(*devpodv1alpha1.Cluster)) {
	t.Helper()
	for i := 0; ; i++ {
		c := get(t, ctx, h, name)
		edit(c)
		err := h.c.Update(ctx, c)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) || i > 20 {
			t.Fatal(err)
		}
	}
}

func connected(r *clusters.Registry, name string) bool {
	ok, _ := r.Connected(name)
	return ok
}

func TestRegistry(t *testing.T) {
	h := startHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRegistry(t, ctx, h)

	t.Run("a token works once", func(t *testing.T) {
		token := register(t, ctx, h, "once")
		first, second := newIdentity(t), newIdentity(t)
		if err := r.Join(ctx, "once", "dpj_wrong", first.PublicKey()); !errors.Is(err, clusters.ErrJoinRefused) {
			t.Fatalf("a wrong token: %v", err)
		}
		if err := r.Join(ctx, "once", token, "ssh-rsa AAAA not-a-key"); !errors.Is(err, clusters.ErrJoinRefused) {
			t.Fatalf("a key that is not Ed25519: %v", err)
		}
		if err := r.Join(ctx, "once", token, first.PublicKey()); err != nil {
			t.Fatalf("join: %v", err)
		}
		c := get(t, ctx, h, "once")
		if c.Spec.AgentPublicKey != first.PublicKey() {
			t.Fatalf("recorded key %q", c.Spec.AgentPublicKey)
		}
		for k := range c.Annotations {
			if strings.Contains(k, "join-token") {
				t.Fatalf("the token is still on the object: %s", k)
			}
		}
		// the same token again, for another agent: refused, the first stays
		if err := r.Join(ctx, "once", token, second.PublicKey()); !errors.Is(err, clusters.ErrJoinRefused) {
			t.Fatalf("second join: %v", err)
		}
		if got := get(t, ctx, h, "once").Spec.AgentPublicKey; got != first.PublicKey() {
			t.Fatal("the second join replaced the first agent's key")
		}
		// the agent that joined, starting again with its token file still there
		if err := r.Join(ctx, "once", token, first.PublicKey()); err != nil {
			t.Fatalf("the same agent again: %v", err)
		}
		if err := r.Join(ctx, "nowhere", token, first.PublicKey()); !errors.Is(err, clusters.ErrJoinRefused) {
			t.Fatalf("a cluster that does not exist: %v", err)
		}
	})

	t.Run("an expired token is refused", func(t *testing.T) {
		c := &devpodv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "late"}}
		token := clusters.IssueToken(c, time.Now().Add(-clusters.TokenLifetime-time.Minute))
		if err := h.c.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := r.Join(ctx, "late", token, newIdentity(t).PublicKey()); !errors.Is(err, clusters.ErrJoinRefused) {
			t.Fatalf("expired: %v", err)
		}
	})

	t.Run("status follows the agent", func(t *testing.T) {
		token := register(t, ctx, h, "follows")
		id := newIdentity(t)
		if err := r.Join(ctx, "follows", token, id.PublicKey()); err != nil {
			t.Fatal(err)
		}
		stop := runAgent(t, ctx, r, "follows", id)
		eventually(t, "connected, with what the agent says", func() bool {
			st := get(t, ctx, h, "follows").Status
			return st.Connected && st.AgentVersion == "v1.2.3" && st.ControllerVersion == "v1.2.0" &&
				st.KubernetesVersion == "v1.36.4" && len(st.Capabilities) == 1 && st.LastSeen != nil
		})
		if in, ok := r.Info("follows"); !ok || in.Cluster != "follows" {
			t.Fatalf("info %+v %v", in, ok)
		}
		stop()
		eventually(t, "disconnected, the versions kept", func() bool {
			st := get(t, ctx, h, "follows").Status
			return !st.Connected && st.AgentVersion == "v1.2.3" && st.LastSeen != nil
		})
		if _, err := r.Dial(ctx, "follows", agent.PurposeKube); !errors.Is(err, clusters.ErrClusterOffline) {
			t.Fatalf("dial when away: %v", err)
		}
	})

	t.Run("one cluster's key does not open another", func(t *testing.T) {
		ta, tb := register(t, ctx, h, "key-a"), register(t, ctx, h, "key-b")
		ida, idb := newIdentity(t), newIdentity(t)
		if err := r.Join(ctx, "key-a", ta, ida.PublicKey()); err != nil {
			t.Fatal(err)
		}
		if err := r.Join(ctx, "key-b", tb, idb.PublicKey()); err != nil {
			t.Fatal(err)
		}
		runAgent(t, ctx, r, "key-b", ida) // A's agent saying it is B
		time.Sleep(200 * time.Millisecond)
		if connected(r, "key-b") {
			t.Fatal("cluster A's key connected as cluster B")
		}
		// a cluster that never joined, and one nobody registered
		register(t, ctx, h, "unjoined")
		runAgent(t, ctx, r, "unjoined", ida)
		runAgent(t, ctx, r, "unregistered", ida)
		time.Sleep(200 * time.Millisecond)
		if connected(r, "unjoined") || connected(r, "unregistered") {
			t.Fatal("an agent without a registration connected")
		}
		runAgent(t, ctx, r, "key-b", idb)
		eventually(t, "the right agent connects", func() bool { return connected(r, "key-b") })
	})

	t.Run("pausing and deleting drop the agent", func(t *testing.T) {
		for _, how := range []string{"pause", "delete"} {
			name := "drop-" + how
			token := register(t, ctx, h, name)
			id := newIdentity(t)
			if err := r.Join(ctx, name, token, id.PublicKey()); err != nil {
				t.Fatal(err)
			}
			runAgent(t, ctx, r, name, id) // keeps trying for the whole test
			eventually(t, "connected", func() bool { return connected(r, name) })
			if how == "pause" {
				change(t, ctx, h, name, func(c *devpodv1alpha1.Cluster) { c.Spec.Paused = true })
			} else if err := h.c.Delete(ctx, &devpodv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
				t.Fatal(err)
			}
			r.Sweep(ctx)
			// dropped, and not let back in although the agent keeps dialling
			time.Sleep(200 * time.Millisecond)
			if connected(r, name) {
				t.Fatalf("%s: the agent is still connected", how)
			}
			if how == "pause" {
				if st := get(t, ctx, h, name).Status; st.Connected {
					t.Fatal("a paused cluster is still shown as connected")
				}
				change(t, ctx, h, name, func(c *devpodv1alpha1.Cluster) { c.Spec.Paused = false })
				eventually(t, "back once resumed", func() bool { return connected(r, name) })
			}
		}
	})

	t.Run("a new token replaces the agent", func(t *testing.T) {
		token := register(t, ctx, h, "rekey")
		old, fresh := newIdentity(t), newIdentity(t)
		if err := r.Join(ctx, "rekey", token, old.PublicKey()); err != nil {
			t.Fatal(err)
		}
		runAgent(t, ctx, r, "rekey", old)
		eventually(t, "connected", func() bool { return connected(r, "rekey") })
		var token2 string
		change(t, ctx, h, "rekey", func(c *devpodv1alpha1.Cluster) { token2 = clusters.IssueToken(c, time.Now()) })
		r.Sweep(ctx)
		time.Sleep(200 * time.Millisecond)
		if connected(r, "rekey") {
			t.Fatal("the old agent is still connected after its key was withdrawn")
		}
		if err := r.Join(ctx, "rekey", token2, fresh.PublicKey()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		if connected(r, "rekey") {
			t.Fatal("the old agent got back in with the old key")
		}
		runAgent(t, ctx, r, "rekey", fresh)
		eventually(t, "the new agent connects", func() bool { return connected(r, "rekey") })
	})
}

// The console tells every connected agent which gateways there are —
// those that joined and are not paused — and again when that changes.
func TestRegistry_TellsAgentsOfGateways(t *testing.T) {
	h := startHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := clusters.New(clusters.Options{
		Hub: h.c, Reader: h.c, Instance: "console-0", StatusInterval: time.Hour,
		Heartbeat: 30 * time.Millisecond, Dead: 300 * time.Millisecond, GatewaysInterval: 30 * time.Millisecond,
	})
	go r.Run(ctx)
	mk := func(name, address, key string, paused bool) {
		t.Helper()
		g := &devpodv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: devpodv1alpha1.GatewaySpec{AgentAddress: address, PublicKey: key, Paused: paused}}
		if err := h.c.Create(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	gwKey := newIdentity(t).PublicKey()
	mk("b300", "https://gw-b300.example", gwKey, false)
	mk("unjoined", "https://gw-unjoined.example", "", false)
	mk("paused", "https://gw-paused.example", gwKey, true)
	mk("nowhere", "", gwKey, false)

	token := register(t, ctx, h, "c1")
	id := newIdentity(t)
	if err := r.Join(ctx, "c1", token, id.PublicKey()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	dialled := map[string]int{}
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()
	a, err := agent.New(agent.Options{
		Cluster: "c1", Identity: id, API: &rest.Config{Host: api.URL},
		DialGateway: func(_ context.Context, address string) (net.Conn, error) {
			mu.Lock()
			dialled[address]++
			mu.Unlock()
			return nil, errors.New("not in this test")
		},
		GatewayTunnel: tunnel.ClientOptions{BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = a.Run(ctx, tunnel.ClientOptions{
			Idle: 2, Heartbeat: 30 * time.Millisecond, Dead: 300 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
			Dial: func(context.Context) (net.Conn, error) {
				x, y := net.Pipe()
				go r.Accept(ctx, y)
				return x, nil
			},
		})
	}()
	tried := func(address string) bool {
		mu.Lock()
		defer mu.Unlock()
		return dialled[address] > 0
	}
	eventually(t, "the agent goes for the joined gateway", func() bool { return tried("https://gw-b300.example") })
	if tried("https://gw-unjoined.example") || tried("https://gw-paused.example") || tried("") {
		t.Fatalf("the agent was told of a gateway it should not be: %v", dialled)
	}
	// one more joins
	mk("mulan", "https://gw-mulan.example", gwKey, false)
	eventually(t, "the agent goes for the new one too", func() bool { return tried("https://gw-mulan.example") })
	// one is paused: the agent stops going for it
	change := func() {
		for i := 0; ; i++ {
			var g devpodv1alpha1.Gateway
			if err := h.c.Get(ctx, types.NamespacedName{Name: "b300"}, &g); err != nil {
				t.Fatal(err)
			}
			g.Spec.Paused = true
			if err := h.c.Update(ctx, &g); err == nil {
				return
			} else if !apierrors.IsConflict(err) || i > 20 {
				t.Fatal(err)
			}
		}
	}
	change()
	eventually(t, "the agent leaves the paused gateway alone", func() bool {
		mu.Lock()
		before := dialled["https://gw-b300.example"]
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return dialled["https://gw-b300.example"] == before
	})
}
