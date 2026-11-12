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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	clustersOf := func(path string) string {
		t.Helper()
		var got []string
		w.until(path+" lists fl-n1", func() bool {
			code, out, _ := w.call("gl-alice", "GET", path, "")
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
	if got := clustersOf("/api/nodes"); got != "a,b" {
		t.Fatalf("/api/nodes: fl-n1 in %q, want a,b", got)
	}
	if got := clustersOf("/api/clusters/a/nodes"); got != "a" {
		t.Fatalf("/api/clusters/a/nodes: fl-n1 in %q, want a", got)
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
