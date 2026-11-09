// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func startEnv(t *testing.T) (kubernetes.Interface, client.Client) {
	t.Helper()
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cs := kubernetes.NewForConfigOrDie(cfg)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "devpod-system"}}); err != nil {
		t.Fatal(err)
	}
	return cs, c
}

type event struct {
	who   string
	start bool
	at    time.Time
}

func TestElector_SingleLeaderAndHandover(t *testing.T) {
	cs, _ := startEnv(t)
	var mu sync.Mutex
	var events []event
	electors := map[string]*Elector{}
	cancels := map[string]context.CancelFunc{}
	for _, id := range []string{"a", "b"} {
		e := &Elector{Client: cs, Namespace: "devpod-system", Name: "devpod-webui-leader", Identity: id,
			LeaseDuration: 4 * time.Second, RenewDeadline: 3 * time.Second, RetryPeriod: 500 * time.Millisecond}
		electors[id] = e
		ctx, cancel := context.WithCancel(context.Background())
		cancels[id] = cancel
		t.Cleanup(cancel)
		go e.Run(ctx, func(context.Context) {
			mu.Lock()
			events = append(events, event{who: id, start: true, at: time.Now()})
			mu.Unlock()
		}, func() {
			mu.Lock()
			events = append(events, event{who: id, start: false, at: time.Now()})
			mu.Unlock()
		})
	}
	first := waitStart(t, &mu, &events, 0, 10*time.Second)
	time.Sleep(2 * time.Second)
	mu.Lock()
	starts := 0
	for _, e := range events {
		if e.start {
			starts++
		}
	}
	mu.Unlock()
	if starts != 1 {
		t.Fatalf("exactly one leader expected, got %d starts: %+v", starts, events)
	}
	other := map[string]string{"a": "b", "b": "a"}[first.who]
	if electors[other].IsLeader() || !electors[first.who].IsLeader() {
		t.Fatal("IsLeader disagrees with onStart")
	}
	cancels[first.who]()
	second := waitStart(t, &mu, &events, 1, 20*time.Second)
	if second.who != other {
		t.Fatalf("handover went to %q", second.who)
	}
	if electors[first.who].IsLeader() {
		t.Fatal("the old leader still claims leadership")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.who == first.who && !e.start && e.at.After(second.at) {
			t.Fatal("old leader's onStop must run before the new leader starts")
		}
	}
}

// waitStart returns the n-th (0-based) start event.
func waitStart(t *testing.T, mu *sync.Mutex, events *[]event, n int, d time.Duration) event {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := 0
		for _, e := range *events {
			if e.start {
				if seen == n {
					mu.Unlock()
					return e
				}
				seen++
			}
		}
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no leader start #%d within %v", n+1, d)
	return event{}
}

func TestPublishIngestEndpoint(t *testing.T) {
	_, c := startEnv(t)
	ctx := context.Background()
	if err := PublishIngestEndpoint(ctx, c, "devpod-system", "devpod-webui-ingest", "webui-0", "10.1.2.3", 8081); err != nil {
		t.Fatal(err)
	}
	if addr, err := LeaderAddr(ctx, c, "devpod-system", "devpod-webui-ingest"); err != nil || addr != "10.1.2.3:8081" {
		t.Fatalf("%q %v", addr, err)
	}
	if err := PublishIngestEndpoint(ctx, c, "devpod-system", "devpod-webui-ingest", "webui-1", "10.1.2.4", 8081); err != nil {
		t.Fatal(err)
	}
	if addr, err := LeaderAddr(ctx, c, "devpod-system", "devpod-webui-ingest"); err != nil || addr != "10.1.2.4:8081" {
		t.Fatalf("republish must replace the endpoint: %q %v", addr, err)
	}
}
