// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// accessRig: a server with no access rules and no groups (removed again
// after the test), and a way to call its API as some user.
func accessRig(t *testing.T) (*webui.Server, func(user string, admin bool, method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	clear := func() {
		_ = k8sClient.Delete(ctx, &devpodv1alpha1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: devpodv1alpha1.AccessPolicyName}})
		_ = k8sClient.DeleteAllOf(ctx, &devpodv1alpha1.UserGroup{})
		// gone from the cache too, before the test reads through it
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var p devpodv1alpha1.AccessPolicy
			var gs devpodv1alpha1.UserGroupList
			if k8sClient.Get(ctx, client.ObjectKey{Name: devpodv1alpha1.AccessPolicyName}, &p) != nil && k8sClient.List(ctx, &gs) == nil && len(gs.Items) == 0 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	clear()
	t.Cleanup(clear)
	h := s.Routes()
	call := func(user string, admin bool, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		return doJSON(t, h.ServeHTTP, method, path, nil, forge(sm, user, admin), body)
	}
	return s, call
}

type accessBody struct {
	Rules           []devpodv1alpha1.AccessRule `json:"rules"`
	Default         devpodv1alpha1.AccessEffect `json:"default"`
	ResourceVersion string                      `json:"resourceVersion"`
}

func readAccess(t *testing.T, rec *httptest.ResponseRecorder) accessBody {
	t.Helper()
	var b accessBody
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &b) != nil {
		t.Fatalf("access: %d %s", rec.Code, rec.Body)
	}
	return b
}

func putBody(t *testing.T, b accessBody) string {
	t.Helper()
	raw, _ := json.Marshal(b)
	return string(raw)
}

var everyoneDenied = devpodv1alpha1.AccessRule{Machines: []string{"x-*"}, Subject: devpodv1alpha1.AccessSubject{Kind: "everyone"}}

func TestAccessDefaultWhenAbsent(t *testing.T) {
	s, call := accessRig(t)
	b := readAccess(t, call("ac-root", true, "GET", "/api/admin/access", ""))
	if len(b.Rules) != 0 || b.ResourceVersion != "" || !b.Default.Allow || !b.Default.Privileged || len(b.Default.HostPaths) != 1 || b.Default.HostPaths[0].Path != "/" {
		t.Fatalf("absent: %+v", b)
	}
	d, admin, err := s.DecideForTest(context.Background(), "ac-bob", "", "n1", nil)
	if err != nil || admin || d.Rule != -1 || !d.Allow || !d.Privileged {
		t.Fatalf("decide: %+v %v %v", d, admin, err)
	}
}

func TestAccessPutAndGet(t *testing.T) {
	_, call := accessRig(t)
	second := devpodv1alpha1.AccessRule{Cluster: "", Subject: devpodv1alpha1.AccessSubject{Kind: "user", Name: "ac-bob"}, Effect: devpodv1alpha1.AccessEffect{Allow: true, NeedsReservation: true}}
	rec := call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{everyoneDenied, second}, Default: devpodv1alpha1.DefaultAccessEffect()}))
	if put := readAccess(t, rec); put.ResourceVersion == "" {
		t.Fatalf("no resourceVersion: %+v", put)
	}
	got := readAccess(t, call("ac-root", true, "GET", "/api/admin/access", ""))
	if len(got.Rules) != 2 || got.Rules[0].Machines[0] != "x-*" || got.Rules[1].Subject.Name != "ac-bob" || !got.Rules[1].Effect.NeedsReservation {
		t.Fatalf("order: %+v", got.Rules)
	}
}

// Two admins saving: the later one, holding an older version, is refused.
func TestAccessPutConflict(t *testing.T) {
	_, call := accessRig(t)
	first := readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Default: devpodv1alpha1.DefaultAccessEffect()})))
	if rec := call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Default: devpodv1alpha1.DefaultAccessEffect()})); rec.Code != http.StatusConflict {
		t.Fatalf("stale create: %d %s", rec.Code, rec.Body)
	}
	second := readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{everyoneDenied}, Default: devpodv1alpha1.DefaultAccessEffect(), ResourceVersion: first.ResourceVersion})))
	if rec := call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Default: devpodv1alpha1.DefaultAccessEffect(), ResourceVersion: first.ResourceVersion})); rec.Code != http.StatusConflict {
		t.Fatalf("stale update: %d %s", rec.Code, rec.Body)
	}
	if got := readAccess(t, call("ac-root", true, "GET", "/api/admin/access", "")); got.ResourceVersion != second.ResourceVersion || len(got.Rules) != 1 {
		t.Fatalf("the earlier save was overwritten: %+v", got)
	}
}

func TestAccessValidation(t *testing.T) {
	_, call := accessRig(t)
	for name, r := range map[string]devpodv1alpha1.AccessRule{
		"unknown group":     {Subject: devpodv1alpha1.AccessSubject{Kind: "group", Name: "ac-nogroup"}},
		"relative hostPath": {Subject: devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: devpodv1alpha1.AccessEffect{Allow: true, HostPaths: []devpodv1alpha1.HostPathGrant{{Path: "data"}}}},
		"empty machine":     {Machines: []string{""}, Subject: devpodv1alpha1.AccessSubject{Kind: "everyone"}},
		"unknown kind":      {Subject: devpodv1alpha1.AccessSubject{Kind: "someone"}},
		"user without name": {Subject: devpodv1alpha1.AccessSubject{Kind: "user"}},
	} {
		if rec := call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{r}, Default: devpodv1alpha1.DefaultAccessEffect()})); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestGroupsCRUD(t *testing.T) {
	_, call := accessRig(t)
	if rec := call("ac-root", true, "POST", "/api/admin/groups", `{"name":"ac-infer","displayName":"推理","members":["ac-wyc"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := call("ac-root", true, "PUT", "/api/admin/groups/ac-infer", `{"displayName":"推理","members":["ac-wyc","ac-qiulei"]}`); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Items []struct {
			Name    string   `json:"name"`
			Members []string `json:"members"`
		} `json:"items"`
	}
	rec := call("ac-root", true, "GET", "/api/admin/groups", "")
	if json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Items) != 1 || len(list.Items[0].Members) != 2 {
		t.Fatalf("list: %s", rec.Body)
	}
	byGroup := devpodv1alpha1.AccessRule{Subject: devpodv1alpha1.AccessSubject{Kind: "group", Name: "ac-infer"}, Effect: devpodv1alpha1.AccessEffect{Allow: true}}
	saved := readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{byGroup}, Default: devpodv1alpha1.DefaultAccessEffect()})))
	if rec := call("ac-root", true, "DELETE", "/api/admin/groups/ac-infer", ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete while a rule names it: %d %s", rec.Code, rec.Body)
	}
	readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Default: devpodv1alpha1.DefaultAccessEffect(), ResourceVersion: saved.ResourceVersion})))
	if rec := call("ac-root", true, "DELETE", "/api/admin/groups/ac-infer", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
}

func TestAccessAdminOnly(t *testing.T) {
	_, call := accessRig(t)
	for _, p := range []string{"/api/admin/access", "/api/admin/groups", "/api/admin/access/match?machines=x"} {
		if rec := call("ac-bob", false, "GET", p, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

// The machines a selector names, per cluster, before it is saved.
func TestAccessMatch(t *testing.T) {
	_, call := accessRig(t)
	ctx := context.Background()
	for _, name := range []string{"am-n1", "am-n2"} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := k8sClient.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, n) })
	}
	var out struct {
		Items []struct {
			Count        int      `json:"count"`
			Names        []string `json:"names"`
			Unreservable int      `json:"unreservable"`
		} `json:"items"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := call("ac-root", true, "GET", "/api/admin/access/match?machines=am-n*", "")
		if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &out) == nil && len(out.Items) == 1 && out.Items[0].Count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("match: %d %s", rec.Code, rec.Body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out.Items[0].Unreservable != 2 {
		t.Fatalf("unreservable %d", out.Items[0].Unreservable)
	}
}

// The running DevPods the rules no longer let be where they are.
func TestAccessCheck(t *testing.T) {
	p := newPlaceRig(t, "ck")
	p.s.Admins = map[string]bool{"ck-root": true}
	for _, u := range []string{"ck-bob", "ck-root"} {
		dp := p.create(u, http.StatusCreated)
		p.patch(u, dp, fmt.Sprintf(`{"node":%q}`, p.n1), http.StatusOK)
	}
	carol := p.create("ck-carol", http.StatusCreated)
	p.patch("ck-carol", carol, fmt.Sprintf(`{"node":%q}`, p.n2), http.StatusOK)
	setRules(t, onNode(p.n1, denied))
	var out struct {
		Items []struct {
			Cluster, DevPod, Owner, Node, Code, Reason string
			Rule                                       int
		} `json:"items"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := doJSON(t, p.s.Routes().ServeHTTP, "POST", "/api/admin/access/check", nil, forge(p.sm, "ck-admin", true), "")
		if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &out) == nil && len(out.Items) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("check: %d %s", rec.Code, rec.Body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(out.Items) != 1 || out.Items[0].DevPod != "ck-bob-dev1" || out.Items[0].Code != "ACL_DENIED" || out.Items[0].Rule != 0 || out.Items[0].Node != p.n1 {
		t.Fatalf("found: %+v", out.Items)
	}
	if rec := doJSON(t, p.s.Routes().ServeHTTP, "POST", "/api/admin/access/check", nil, forge(p.sm, "ck-bob", false), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("a user ran the check: %d", rec.Code)
	}
}
