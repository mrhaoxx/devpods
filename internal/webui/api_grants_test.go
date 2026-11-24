// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

type grantView struct {
	Name            string   `json:"name"`
	Match           string   `json:"match"`
	Permissions     []string `json:"permissions"`
	Description     string   `json:"description"`
	ResourceVersion string   `json:"resourceVersion"`
	Problem         string   `json:"problem"`
}

func grantsOf(t *testing.T, call func(string, bool, string, string, string) *httptest.ResponseRecorder) []grantView {
	t.Helper()
	rec := call("ac-root", true, "GET", "/api/admin/grants", "")
	var out struct {
		Items []grantView `json:"items"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("grants: %d %s", rec.Code, rec.Body)
	}
	return out.Items
}

func TestGrantsCRUD(t *testing.T) {
	_, call := accessRig(t)
	rec := call("ac-root", true, "POST", "/api/admin/grants", `{"name":"ac-g1","match":"user = ac-bob","permissions":["viewAudit"],"description":"bob looks"}`)
	var made grantView
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.ResourceVersion == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if got := grantsOf(t, call); len(got) != 1 || got[0].Name != "ac-g1" || got[0].Match != "user = ac-bob" || got[0].Description != "bob looks" {
		t.Fatalf("list: %+v", got)
	}
	body := func(rv string) string {
		return `{"match":"user in (ac-bob, ac-carol)","permissions":["viewAudit","disconnectSessions"],"resourceVersion":"` + rv + `"}`
	}
	if rec := call("ac-root", true, "PUT", "/api/admin/grants/ac-g1", body(made.ResourceVersion)); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	if rec := call("ac-root", true, "PUT", "/api/admin/grants/ac-g1", body(made.ResourceVersion)); rec.Code != http.StatusConflict {
		t.Fatalf("a stale update: %d %s", rec.Code, rec.Body)
	}
	if rec := call("ac-root", true, "POST", "/api/admin/grants", `{"name":"Bad Name","match":"user = a","permissions":["viewAudit"]}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "名字") {
		t.Fatalf("a bad name: %d %s", rec.Code, rec.Body)
	}
	if rec := call("ac-bob", false, "GET", "/api/admin/grants", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("not an admin: %d", rec.Code)
	}
	if rec := call("ac-root", true, "DELETE", "/api/admin/grants/ac-g1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(grantsOf(t, call)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("still listed after delete")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestGrantsValidation(t *testing.T) {
	_, call := accessRig(t)
	for name, b := range map[string]string{
		"no condition":            `{"name":"ac-v","match":"","permissions":["viewAudit"]}`,
		"no permission":           `{"name":"ac-v","match":"user = a","permissions":[]}`,
		"an unknown permission":   `{"name":"ac-v","match":"user = a","permissions":["root"]}`,
		"twice":                   `{"name":"ac-v","match":"user = a","permissions":["viewAudit","viewAudit"]}`,
		"disconnect without view": `{"name":"ac-v","match":"user = a","permissions":["disconnectSessions"]}`,
		"an unknown cluster":      `{"name":"ac-v","match":"cluster = nope","permissions":["viewAudit"]}`,
		"an unknown group":        `{"name":"ac-v","match":"group = ac-nogroup","permissions":["viewAudit"]}`,
	} {
		if rec := call("ac-root", true, "POST", "/api/admin/grants", b); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	rec := call("ac-root", true, "POST", "/api/admin/grants", `{"name":"ac-g1","match":"cluster in st","permissions":["viewAudit"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "授权 ac-g1 的条件第 12 个字符：") {
		t.Errorf("syntax: %d %s", rec.Code, rec.Body)
	}
}

// A grant kubectl left naming a group not there is said (it gives nothing).
func TestGrantsProblemReported(t *testing.T) {
	_, call := accessRig(t)
	g := &devpodv1alpha1.AccessGrant{ObjectMeta: metav1.ObjectMeta{Name: "ac-gone"}, Spec: devpodv1alpha1.AccessGrantSpec{Match: "group = ac-gone", Permissions: []devpodv1alpha1.GrantPermission{devpodv1alpha1.PermViewAudit}}}
	if err := k8sClient.Create(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if got := grantsOf(t, call); len(got) != 1 || got[0].Problem != "点名了不存在的组 ac-gone" {
		t.Fatalf("problem: %+v", got)
	}
}

// A group a grant names is not deleted — wherever it is named.
func TestGroupDeleteGuardGrants(t *testing.T) {
	_, call := accessRig(t)
	if rec := call("ac-root", true, "POST", "/api/admin/groups", `{"name":"ac-x","members":["ac-bob"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("group: %d %s", rec.Code, rec.Body)
	}
	if rec := call("ac-root", true, "POST", "/api/admin/grants", `{"name":"ac-notx","match":"everyone and not group = ac-x","permissions":["viewAudit"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := call("ac-root", true, "DELETE", "/api/admin/groups/ac-x", "")
		if rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "ac-notx") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("delete a group a grant names: %d %s", rec.Code, rec.Body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestMePermissions(t *testing.T) {
	_, call := accessRig(t)
	perms := func(user string, admin bool) map[string]bool {
		var out struct {
			Permissions map[string]bool `json:"permissions"`
		}
		rec := call(user, admin, "GET", "/api/me", "")
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("me: %d %s", rec.Code, rec.Body)
		}
		return out.Permissions
	}
	if p := perms("me-alice", false); p["viewAudit"] || p["setReservable"] || p["disconnectSessions"] {
		t.Fatalf("no grants: %v", p)
	}
	if rec := call("ac-root", true, "POST", "/api/admin/grants", `{"name":"me-a","match":"user = me-alice and machine = me-n1","permissions":["viewAudit"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		p := perms("me-alice", false)
		if p["viewAudit"] && !p["setReservable"] && !p["disconnectSessions"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with a grant: %v", p)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if p := perms("ac-root", true); !p["viewAudit"] || !p["setReservable"] || !p["disconnectSessions"] {
		t.Fatalf("an admin: %v", p)
	}
}

// grantNow writes a grant as kubectl would and waits for the cache.
func grantNow(t *testing.T, s *webui.Server, name, match string, ps ...devpodv1alpha1.GrantPermission) {
	t.Helper()
	ctx := context.Background()
	g := &devpodv1alpha1.AccessGrant{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: devpodv1alpha1.AccessGrantSpec{Match: match, Permissions: ps}}
	if err := k8sClient.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), g) })
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got devpodv1alpha1.AccessGrant
		if s.Client.Get(ctx, client.ObjectKey{Name: name}, &got) == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grant %s never reached the cache", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSetReservableByGrant(t *testing.T) {
	s, sm, _, _ := rsvRig(t, "sr")
	grantNow(t, s, "sr-bob", "user = sr-bob and machine = sr-n2", devpodv1alpha1.PermSetReservable)
	// bob may not use these machines at all: switching them is another matter
	setRules(t, devpodv1alpha1.AccessRule{Match: "user = sr-bob"})
	put := func(user, node string) int {
		return doJSON(t, s.Routes().ServeHTTP, "PUT", "/api/admin/nodes/"+node+"/reservable", nil, forge(sm, user, false), `{"reservable":true}`).Code
	}
	if code := put("sr-bob", "sr-n2"); code != http.StatusOK {
		t.Fatalf("bob on sr-n2: %d", code)
	}
	var n corev1.Node
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "sr-n2"}, &n); err != nil || n.Labels[devpodv1alpha1.ReservableNodeLabel] != "true" {
		t.Fatalf("sr-n2 not reservable: %v %v", err, n.Labels)
	}
	if code := put("sr-bob", "sr-n1"); code != http.StatusForbidden {
		t.Fatalf("bob on sr-n1: %d", code)
	}
	if code := put("sr-carol", "sr-n2"); code != http.StatusForbidden {
		t.Fatalf("carol: %d", code)
	}
}

func TestManagedNodes(t *testing.T) {
	s, sm, _, _ := rsvRig(t, "mn")
	grantNow(t, s, "mn-bob", "user = mn-bob and machine = mn-n2", devpodv1alpha1.PermSetReservable)
	managed := func(user string, admin bool) map[string]bool {
		rec := doJSON(t, s.Routes().ServeHTTP, "GET", "/api/nodes/managed", nil, forge(sm, user, admin), "")
		var out struct {
			Items []struct {
				Name       string `json:"name"`
				Reservable bool   `json:"reservable"`
			} `json:"items"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("managed: %d %s", rec.Code, rec.Body)
		}
		got := map[string]bool{}
		for _, it := range out.Items {
			if strings.HasPrefix(it.Name, "mn-") {
				got[it.Name] = it.Reservable
			}
		}
		return got
	}
	if got := managed("mn-bob", false); len(got) != 1 || got["mn-n2"] {
		t.Fatalf("bob: %v", got)
	}
	if got := managed("mn-carol", false); len(got) != 0 {
		t.Fatalf("carol: %v", got)
	}
	if got := managed("mn-root", true); len(got) != 2 || !got["mn-n1"] {
		t.Fatalf("an admin: %v", got)
	}
}

// 管理预约 is a grant permission like the others, and /api/me says it.
func TestGrantManageReservations(t *testing.T) {
	_, call := accessRig(t)
	if rec := call("ac-root", true, "POST", "/api/admin/grants", `{"name":"mr-keeper","match":"user = mr-bob and machine = mr-n1","permissions":["manageReservations"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	perms := func(user string, admin bool) map[string]bool {
		var out struct {
			Permissions map[string]bool `json:"permissions"`
		}
		rec := call(user, admin, "GET", "/api/me", "")
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("me: %d %s", rec.Code, rec.Body)
		}
		return out.Permissions
	}
	deadline := time.Now().Add(10 * time.Second)
	for !perms("mr-bob", false)["manageReservations"] {
		if time.Now().After(deadline) {
			t.Fatalf("bob: %v", perms("mr-bob", false))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if p := perms("ac-root", true); !p["manageReservations"] {
		t.Fatalf("an admin: %v", p)
	}
	if p := perms("mr-carol", false); p["manageReservations"] {
		t.Fatalf("carol: %v", p)
	}
}
