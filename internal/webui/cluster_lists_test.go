// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

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
