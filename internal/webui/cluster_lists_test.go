// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/live"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Lists that are a cluster's cover every cluster when the request names
// none, each item saying its cluster; naming one still gets that one.
func TestListsCoverEveryCluster(t *testing.T) {
	w := setupWorld(t)
	ctx := context.Background()
	for _, cl := range []struct {
		name string
		mk   func(*corev1.Node) error
	}{
		{"a", func(n *corev1.Node) error { return w.a.Create(ctx, n) }},
		{"b", func(n *corev1.Node) error { return w.b.Create(ctx, n) }},
	} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fl-n1"}}
		if err := cl.mk(n); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = w.a.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fl-n1"}})
		_ = w.b.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fl-n1"}})
	})
	clustersOf := func(user, path string) string {
		t.Helper()
		var got []string
		w.until(path+" lists fl-n1", func() bool {
			code, out, _ := w.call(user, "GET", path, "")
			got = nil
			if code != http.StatusOK {
				return false
			}
			raw, _ := out["items"].([]any)
			for _, x := range raw {
				m := x.(map[string]any)
				if m["name"] == "fl-n1" {
					c, _ := m["cluster"].(string)
					got = append(got, c)
				}
			}
			return len(got) > 0
		})
		sort.Strings(got)
		return strings.Join(got, ",")
	}
	if got := clustersOf("gl-alice", "/api/nodes"); got != "a,b" {
		t.Fatalf("/api/nodes: fl-n1 in %q, want a,b", got)
	}
	if got := clustersOf("gl-alice", "/api/clusters/a/nodes"); got != "a" {
		t.Fatalf("/api/clusters/a/nodes: fl-n1 in %q, want a", got)
	}
	// with live data, which covers every cluster at once: still each
	// cluster's nodes under it, once
	w.s.Live = &webui.LiveDeps{Snapshot: func(context.Context) (live.Snapshot, bool) {
		return live.NewState(nil).Snapshot((&webui.LiveInputs{S: w.s}).Build(context.Background())), true
	}}
	for _, path := range []string{"/api/nodes", "/api/admin/nodes"} {
		if got := clustersOf("root", path); got != "a,b" {
			t.Fatalf("%s with live data: fl-n1 in %q, want a,b", path, got)
		}
	}
}

// The DevPod watch without a cluster carries every cluster's changes,
// each saying its cluster.
func TestWatchCoversEveryCluster(t *testing.T) {
	w := setupWorld(t)
	srv := httptest.NewServer(w.h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/devpods?watch=true", nil)
	req.AddCookie(forge(w.sm, "gl-alice", false))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch: %d", resp.StatusCode)
	}
	seen := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Cluster string `json:"cluster"`
				DevPod  struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"devpod"`
			}
			if json.Unmarshal([]byte(line), &ev) == nil && ev.DevPod.Metadata.Name == "gl-alice-wch" {
				seen <- ev.Cluster
			}
		}
	}()
	for _, c := range []string{"a", "b"} {
		if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/"+c+"/devpods", `{"name":"wch","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}`); code != http.StatusCreated {
			t.Fatalf("create in %s: %d %v", c, code, out)
		}
	}
	got := map[string]bool{}
	deadline := time.After(20 * time.Second)
	for !(got["a"] && got["b"]) {
		select {
		case c := <-seen:
			got[c] = true
		case <-deadline:
			t.Fatalf("events seen from %v, want a and b", got)
		}
	}
}

// A cluster that is away does not take the lists of the others down.
func TestListsWithAClusterAway(t *testing.T) {
	w := setupWorld(t)
	w.s.Reservations = &webui.ReservationRules{Granularity: 30 * time.Minute, MaxDuration: 24 * time.Hour, MaxAdvance: 14 * 24 * time.Hour,
		ClearNotice: 30 * time.Minute, Now: time.Now}
	w.stopB()
	w.until("b is stale", func() bool { _, stale := w.s.Clusters.Stale("b"); return stale })
	for _, path := range []string{"/api/nodes", "/api/reservations"} {
		if code, out, _ := w.call("gl-alice", "GET", path, ""); code != http.StatusOK {
			t.Fatalf("%s with b away: %d %v", path, code, out)
		}
	}
}

// An Event about a DevPod is written in the cluster the DevPod is in.
func TestDevPodEventInItsCluster(t *testing.T) {
	w := setupWorld(t)
	ctx := context.Background()
	if code, out, _ := w.call("gl-alice", "POST", "/api/clusters/b/devpods", `{"name":"ev","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}`); code != http.StatusCreated {
		t.Fatalf("create in b: %d %v", code, out)
	}
	var dp devpodv1alpha1.DevPod
	w.until("b has the DevPod", func() bool {
		return w.b.Get(ctx, types.NamespacedName{Namespace: "devpods", Name: "gl-alice-ev"}, &dp) == nil
	})
	w.until("the console sees it", func() bool {
		_, cluster, ok := w.s.DevPodByUID(ctx, string(dp.UID))
		return ok && cluster == "b"
	})
	if err := w.s.DevPodEvent(ctx, string(dp.UID), "Warning", "TelemetryAlert", "agent_silent: no telemetry"); err != nil {
		t.Fatal(err)
	}
	count := func(c client.Client) int {
		var evs corev1.EventList
		_ = c.List(ctx, &evs, client.InNamespace("devpods"))
		n := 0
		for _, e := range evs.Items {
			if e.InvolvedObject.UID == dp.UID && e.Reason == "TelemetryAlert" {
				n++
			}
		}
		return n
	}
	if count(w.b) != 1 || count(w.a) != 0 {
		t.Fatalf("events: in b %d, in a %d", count(w.b), count(w.a))
	}
}
