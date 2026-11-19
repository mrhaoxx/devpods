// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// A cluster the rules give a user no machine of is not shown to them:
// not among the clusters, not among the machines. An admin sees it.
func TestClustersHiddenWithoutAccess(t *testing.T) {
	w := setupWorld(t)
	ctx := context.Background()
	for _, m := range []struct {
		c    client.Client
		name string
	}{{w.a, "hz-a1"}, {w.b, "hz-b1"}} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: m.name}}
		if err := m.c.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		c := m.c
		t.Cleanup(func() { _ = c.Delete(context.Background(), n) })
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &devpodv1alpha1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.AccessPolicyName}})
	})
	clustersOf := func(user string) []string {
		_, out, _ := w.call(user, "GET", "/api/clusters", "")
		var got []string
		for _, it := range out["items"].([]any) {
			got = append(got, it.(map[string]any)["name"].(string))
		}
		slices.Sort(got)
		return got
	}
	nodesOf := func(user string) []string {
		_, out, _ := w.call(user, "GET", "/api/nodes", "")
		var got []string
		for _, it := range out["items"].([]any) {
			m := it.(map[string]any)
			if name := m["name"].(string); strings.HasPrefix(name, "hz-") {
				got = append(got, m["cluster"].(string)+"/"+name)
			}
		}
		slices.Sort(got)
		return got
	}
	w.until("both machines are seen", func() bool { return len(nodesOf("root")) == 2 })

	everyone := &devpodv1alpha1.AccessSubject{Kind: "everyone"}
	setRules(t, devpodv1alpha1.AccessRule{Cluster: "b", Subject: everyone, Effect: denied})
	if got := clustersOf("hz-carol"); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("carol's clusters %v", got)
	}
	if got := nodesOf("hz-carol"); !slices.Equal(got, []string{"a/hz-a1"}) {
		t.Fatalf("carol's machines %v", got)
	}
	if got := clustersOf("root"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("an admin's clusters %v", got)
	}
	// what /api/me says per cluster (the SSH CLI's whoami) leaves b out too
	meClusters := func(user string) []string {
		_, out, _ := w.call(user, "GET", "/api/me", "")
		var got []string
		for c := range out["clusters"].(map[string]any) {
			got = append(got, c)
		}
		slices.Sort(got)
		return got
	}
	if got := meClusters("hz-carol"); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("carol's /api/me clusters %v", got)
	}
	// someone's reservation in b is not shown to her (the SSH CLI's reserve list)
	w.s.Reservations = &webui.ReservationRules{Granularity: 30 * time.Minute, MaxDuration: 24 * time.Hour, MaxAdvance: 14 * 24 * time.Hour, ClearNotice: 30 * time.Minute, Now: time.Now}
	start := time.Now().Truncate(time.Hour)
	for _, c := range []string{"a", "b"} {
		nr := &devpodv1alpha1.NodeReservation{ObjectMeta: metav1.ObjectMeta{Name: "hz-dave-" + c}, Spec: devpodv1alpha1.NodeReservationSpec{
			Cluster: c, Node: "hz-" + c + "1", User: "hz-dave", Start: metav1.NewTime(start), End: metav1.NewTime(start.Add(time.Hour))}}
		if err := k8sClient.Create(ctx, nr); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), nr) })
	}
	reservationsOf := func(user string) []string {
		_, out, _ := w.call(user, "GET", "/api/reservations", "")
		var got []string
		for _, it := range out["items"].([]any) {
			m := it.(map[string]any)
			if strings.HasPrefix(m["id"].(string), "hz-") {
				got = append(got, m["cluster"].(string)+"/"+m["id"].(string))
			}
		}
		slices.Sort(got)
		return got
	}
	w.until("an admin sees both reservations", func() bool { return len(reservationsOf("root")) == 2 })
	if got := reservationsOf("hz-carol"); !slices.Equal(got, []string{"a/hz-dave-a"}) {
		t.Fatalf("carol's reservations %v", got)
	}

	// some machines of b opened to carol: b is hers to see, not bob's
	setRules(t,
		devpodv1alpha1.AccessRule{Cluster: "b", Machines: []string{"hz-*"}, Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Users: []string{"hz-carol"}}, Effect: devpodv1alpha1.AccessEffect{Allow: true}},
		devpodv1alpha1.AccessRule{Cluster: "b", Subject: everyone, Effect: denied})
	if got := clustersOf("hz-carol"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("carol's clusters once b is opened to her %v", got)
	}
	if got := clustersOf("hz-bob"); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("bob's clusters %v", got)
	}
}

// A rule naming a group that is not there (as kubectl may leave one)
// denies everyone everything: no cluster is shown but to an admin.
func TestClustersHiddenByBrokenRule(t *testing.T) {
	w := setupWorld(t)
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &devpodv1alpha1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.AccessPolicyName}})
	})
	clustersOf := func(user string) []string {
		_, out, _ := w.call(user, "GET", "/api/clusters", "")
		var got []string
		for _, it := range out["items"].([]any) {
			got = append(got, it.(map[string]any)["name"].(string))
		}
		slices.Sort(got)
		return got
	}
	setRules(t, devpodv1alpha1.AccessRule{Match: "everyone and not group = gone", Effect: devpodv1alpha1.DefaultAccessEffect()})
	if got := clustersOf("hz-erin"); len(got) != 0 {
		t.Fatalf("erin's clusters %v", got)
	}
	if got := clustersOf("root"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("an admin's clusters %v", got)
	}
}

// Refused for a privilege, the answer says which one.
func TestStartRefusedNamesThePrivilege(t *testing.T) {
	p := newPlaceRig(t, "pv")
	setRules(t, devpodv1alpha1.AccessRule{Match: "everyone", Effect: devpodv1alpha1.AccessEffect{Allow: true, Privileges: &devpodv1alpha1.AccessPrivileges{HostPID: true}}})
	yaml := fmt.Sprintf("apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: pv-bob-net\nspec:\n  owner: pv-bob\n  running: true\n  pod:\n    spec:\n      hostNetwork: true\n      nodeSelector:\n        kubernetes.io/hostname: %s\n      containers:\n        - name: dev\n          image: ubuntu:24.04\n", p.n1)
	body, _ := json.Marshal(map[string]string{"yaml": yaml})
	rec := doJSON(t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, p.as("pv-bob"), string(body))
	var e struct{ Code, Message string }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusForbidden || e.Code != "PRIVILEGE_DENIED" || !strings.Contains(e.Message, "the host's network") {
		t.Fatalf("host network: %d %s", rec.Code, rec.Body)
	}
}
