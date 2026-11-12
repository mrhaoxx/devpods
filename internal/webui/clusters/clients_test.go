// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package clusters_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/tunnel"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(devpodv1alpha1.AddToScheme(s))
	return s
}

// runRealAgent starts an in-process agent in front of a real API server.
func runRealAgent(t *testing.T, ctx context.Context, r *clusters.Registry, name string, id *agent.Identity, api *rest.Config) context.CancelFunc {
	t.Helper()
	a, err := agent.New(agent.Options{Cluster: name, Identity: id, API: api})
	if err != nil {
		t.Fatal(err)
	}
	actx, stop := context.WithCancel(ctx)
	go func() {
		_ = a.Run(actx, tunnel.ClientOptions{
			Idle: 4, Heartbeat: 30 * time.Millisecond, Dead: 300 * time.Millisecond,
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

func devpod(name string) *devpodv1alpha1.DevPod {
	return &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{Owner: "alice", Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:24.04"}},
		}}},
	}
}

func names(t *testing.T, ctx context.Context, c client.Reader) []string {
	t.Helper()
	var list devpodv1alpha1.DevPodList
	if err := c.List(ctx, &list, client.InNamespace("devpods")); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := make([]string, 0, len(list.Items))
	for _, d := range list.Items {
		out = append(out, d.Name)
	}
	return out
}

func has(list []string, want ...string) bool {
	if len(list) != len(want) {
		return false
	}
	for _, w := range want {
		found := false
		for _, l := range list {
			found = found || l == w
		}
		if !found {
			return false
		}
	}
	return true
}

func TestClients(t *testing.T) {
	a, b := startHub(t), startHub(t) // a is also where the Cluster objects live
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var indexed atomic.Int32
	r := clusters.New(clusters.Options{
		Hub: a.c, Reader: a.c, Instance: "console-0", StatusInterval: time.Hour,
		Heartbeat: 30 * time.Millisecond, Dead: 300 * time.Millisecond,
		Scheme: newScheme(), Warm: []client.Object{&devpodv1alpha1.DevPod{}},
		Indexes: func(ctx context.Context, ix client.FieldIndexer) error {
			indexed.Add(1)
			return ix.IndexField(ctx, &devpodv1alpha1.DevPod{}, "spec.owner", func(o client.Object) []string {
				return []string{o.(*devpodv1alpha1.DevPod).Spec.Owner}
			})
		},
	})
	go r.Run(ctx)

	ids := map[string]*agent.Identity{}
	for name, env := range map[string]*hubEnv{"a": a, "b": b} {
		token := register(t, ctx, a, name)
		ids[name] = newIdentity(t)
		if err := r.Join(ctx, name, token, ids[name].PublicKey()); err != nil {
			t.Fatal(err)
		}
		if err := env.c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "devpods"}}); err != nil {
			t.Fatal(err)
		}
		if err := env.c.Create(ctx, devpod("in-"+name)); err != nil {
			t.Fatal(err)
		}
	}

	// before any agent: a registered cluster is offline, an unregistered one unknown
	if _, err := r.Clients("a"); !errors.Is(err, clusters.ErrClusterOffline) {
		t.Fatalf("registered, never connected: %v", err)
	}
	if _, err := r.Clients("nope"); !errors.Is(err, clusters.ErrUnknownCluster) {
		t.Fatalf("unregistered: %v", err)
	}

	runRealAgent(t, ctx, r, "a", ids["a"], a.cfg)
	stopB := runRealAgent(t, ctx, r, "b", ids["b"], b.cfg)
	var ca, cb *clusters.ClusterClients
	eventually(t, "both clusters have clients", func() bool {
		var errA, errB error
		ca, errA = r.Clients("a")
		cb, errB = r.Clients("b")
		return errA == nil && errB == nil
	})
	// each cluster's own DevPods, cached and uncached
	if got := names(t, ctx, ca.Client); !has(got, "in-a") {
		t.Fatalf("cluster a, cached: %v", got)
	}
	if got := names(t, ctx, cb.Client); !has(got, "in-b") {
		t.Fatalf("cluster b, cached: %v", got)
	}
	if got := names(t, ctx, cb.Reader); !has(got, "in-b") {
		t.Fatalf("cluster b, uncached: %v", got)
	}
	// the indexes were registered on each cluster's cache
	var byOwner devpodv1alpha1.DevPodList
	if err := cb.Client.List(ctx, &byOwner, client.MatchingFields{"spec.owner": "alice"}); err != nil || len(byOwner.Items) != 1 {
		t.Fatalf("indexed list: %d %v", len(byOwner.Items), err)
	}
	if _, stale := r.Stale("b"); stale {
		t.Fatal("a connected cluster is stale")
	}
	var seen []string
	r.Each(func(c *clusters.ClusterClients) { seen = append(seen, c.Name) })
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Fatalf("Each: %v", seen)
	}
	// a write through the tunnel lands in that cluster and no other
	if err := cb.Client.Create(ctx, devpod("made-in-b")); err != nil {
		t.Fatalf("create through the tunnel: %v", err)
	}
	if got := names(t, ctx, b.c); !has(got, "in-b", "made-in-b") {
		t.Fatalf("cluster b itself: %v", got)
	}
	if got := names(t, ctx, a.c); !has(got, "in-a") {
		t.Fatalf("cluster a got another cluster's DevPod: %v", got)
	}
	eventually(t, "b's cache sees it", func() bool { return has(names(t, ctx, cb.Client), "in-b", "made-in-b") })

	// cluster b goes away
	stopB()
	eventually(t, "b is stale", func() bool { _, stale := r.Stale("b"); return stale })
	since, _ := r.Stale("b")
	if time.Since(since) > 5*time.Second || since.IsZero() {
		t.Fatalf("stale since %v", since)
	}
	again, err := r.Clients("b")
	if err != nil || again != cb {
		t.Fatalf("an offline cluster's clients: %v (same: %v)", err, again == cb)
	}
	if got := names(t, ctx, cb.Client); !has(got, "in-b", "made-in-b") {
		t.Fatalf("cached rows of an offline cluster: %v", got)
	}
	start := time.Now()
	err = cb.Client.Create(ctx, devpod("never"))
	if !errors.Is(err, clusters.ErrClusterOffline) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("a write to an offline cluster: %v after %v", err, time.Since(start))
	}
	start = time.Now()
	var l devpodv1alpha1.DevPodList
	if err := cb.Reader.List(ctx, &l); !errors.Is(err, clusters.ErrClusterOffline) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("an uncached read of an offline cluster: %v after %v", err, time.Since(start))
	}
	// the other cluster is not affected
	if err := ca.Client.Create(ctx, devpod("made-in-a")); err != nil {
		t.Fatalf("cluster a while b is away: %v", err)
	}
	if _, stale := r.Stale("a"); stale {
		t.Fatal("cluster a is stale because b went away")
	}

	// something happens in b meanwhile; the agent returns; the same clients catch up
	if err := b.c.Create(ctx, devpod("while-away")); err != nil {
		t.Fatal(err)
	}
	runRealAgent(t, ctx, r, "b", ids["b"], b.cfg)
	deadline := time.Now().Add(90 * time.Second)
	for !has(names(t, ctx, cb.Client), "in-b", "made-in-b", "while-away") {
		if time.Now().After(deadline) {
			t.Fatalf("the cache never caught up after the agent returned: %v", names(t, ctx, cb.Client))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, stale := r.Stale("b"); stale {
		t.Fatal("still stale after the agent returned")
	}
	if now, _ := r.Clients("b"); now != cb {
		t.Fatal("the clients were rebuilt")
	}
	if indexed.Load() != 2 {
		t.Fatalf("indexes registered %d times for 2 clusters", indexed.Load())
	}

	// a cluster that is removed loses its clients
	if err := a.c.Delete(ctx, &devpodv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "b"}}); err != nil {
		t.Fatal(err)
	}
	r.Sweep(ctx)
	if _, err := r.Clients("b"); !errors.Is(err, clusters.ErrUnknownCluster) {
		t.Fatalf("a removed cluster: %v", err)
	}
}
