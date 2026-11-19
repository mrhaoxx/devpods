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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
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
	Problems        []access.Problem            `json:"problems,omitempty"`
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

var everyoneDenied = devpodv1alpha1.AccessRule{Machines: []string{"x-*"}, Subject: &devpodv1alpha1.AccessSubject{Kind: "everyone"}}

func TestAccessDefaultWhenAbsent(t *testing.T) {
	s, call := accessRig(t)
	b := readAccess(t, call("ac-root", true, "GET", "/api/admin/access", ""))
	if len(b.Rules) != 0 || b.ResourceVersion != "" || !b.Default.Allow || !access.Grants(b.Default).Privileged || len(b.Default.HostPaths) != 1 || b.Default.HostPaths[0].Path != "/" {
		t.Fatalf("absent: %+v", b)
	}
	d, admin, err := s.DecideForTest(context.Background(), "ac-bob", "", "n1", nil)
	if err != nil || admin || d.Rule != -1 || !d.Allow || !access.Grants(d.AccessEffect).Privileged {
		t.Fatalf("decide: %+v %v %v", d, admin, err)
	}
}

func TestAccessPutAndGet(t *testing.T) {
	_, call := accessRig(t)
	second := devpodv1alpha1.AccessRule{Cluster: "", Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Users: []string{"ac-bob", "ac-carol"}}, Effect: devpodv1alpha1.AccessEffect{Allow: true, NeedsReservation: true}}
	rec := call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{everyoneDenied, second}, Default: devpodv1alpha1.DefaultAccessEffect()}))
	if put := readAccess(t, rec); put.ResourceVersion == "" {
		t.Fatalf("no resourceVersion: %+v", put)
	}
	got := readAccess(t, call("ac-root", true, "GET", "/api/admin/access", ""))
	if len(got.Rules) != 2 || got.Rules[0].Match != "machine in (x-*)" || got.Rules[1].Match != "user in (ac-bob, ac-carol)" || !got.Rules[1].Effect.NeedsReservation {
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
	rule := func(match string) devpodv1alpha1.AccessRule { return devpodv1alpha1.AccessRule{Match: match} }
	caps := func(cs ...string) devpodv1alpha1.AccessRule {
		return devpodv1alpha1.AccessRule{Match: "everyone", Effect: devpodv1alpha1.AccessEffect{Allow: true, Privileges: &devpodv1alpha1.AccessPrivileges{Capabilities: cs}}}
	}
	for name, c := range map[string]struct {
		r    devpodv1alpha1.AccessRule
		says string
	}{
		"syntax":                    {rule("cluster in st"), "第 1 条第 12 个字符："},
		"unknown cluster":           {rule("cluster = nope"), "nope"},
		"unknown group":             {rule("user = ac-bob or group = ac-nogroup"), "ac-nogroup"},
		"a bad machine":             {rule(`machine = "["`), "["},
		"a blank user":              {rule(`user = " "`), ""},
		"too long":                  {rule("user = " + strings.Repeat("a", 1994)), "2000"},
		"a blank capability":        {caps(""), ""},
		"a capability with a space": {caps("NET ADMIN"), "NET ADMIN"},
		"relative hostPath":         {devpodv1alpha1.AccessRule{Effect: devpodv1alpha1.AccessEffect{Allow: true, HostPaths: []devpodv1alpha1.HostPathGrant{{Path: "data"}}}}, ""},
	} {
		rec := call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{c.r}, Default: devpodv1alpha1.DefaultAccessEffect()}))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.says) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// a glob of clusters is not checked; capabilities any case, CAP_ or not
	ok := []devpodv1alpha1.AccessRule{rule("cluster = no*"), caps("cap_net_admin", "*")}
	if rec := call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: ok, Default: devpodv1alpha1.DefaultAccessEffect()})); rec.Code != http.StatusOK {
		t.Errorf("valid: %d %s", rec.Code, rec.Body)
	}
}

// The old form, as kubectl or an older console left it, reads as a
// condition with the privileges spelled out.
func TestAccessGetConvertsLegacy(t *testing.T) {
	_, call := accessRig(t)
	setRules(t, devpodv1alpha1.AccessRule{Cluster: "yc", Machines: []string{"c500-*"}, Subject: &devpodv1alpha1.AccessSubject{Kind: "everyone"}, Effect: devpodv1alpha1.AccessEffect{Allow: true, Privileged: true}})
	got := readAccess(t, call("ac-root", true, "GET", "/api/admin/access", ""))
	if len(got.Rules) != 1 || got.Rules[0].Match != "cluster = yc and machine in (c500-*)" {
		t.Fatalf("rules: %+v", got.Rules)
	}
	e := got.Rules[0].Effect
	if e.Privileged || e.Privileges == nil || !e.Privileges.Privileged || !e.Privileges.HostNetwork || !slices.Equal(e.Privileges.Capabilities, []string{"*"}) {
		t.Fatalf("effect: %+v", e)
	}
	if len(got.Problems) != 0 {
		t.Fatalf("problems: %+v", got.Problems)
	}
}

// Saved, a rule is its condition (as written, trimmed) and its effect in
// the new form; nothing of the old form is kept.
func TestAccessPutNewForm(t *testing.T) {
	_, call := accessRig(t)
	if rec := call("ac-root", true, "POST", "/api/admin/groups", `{"name":"ac-ops","members":["ac-bob"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("group: %d %s", rec.Code, rec.Body)
	}
	r := devpodv1alpha1.AccessRule{Match: "  machine != m1 and not group = ac-ops ", Effect: devpodv1alpha1.AccessEffect{Allow: true, Privileges: &devpodv1alpha1.AccessPrivileges{HostPID: true}}}
	readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{r}, Default: devpodv1alpha1.DefaultAccessEffect()})))
	var p devpodv1alpha1.AccessPolicy
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: devpodv1alpha1.AccessPolicyName}, &p); err != nil {
		t.Fatal(err)
	}
	got := p.Spec.Rules[0]
	if got.Match != "machine != m1 and not group = ac-ops" || got.Subject != nil || got.Cluster != "" || got.Effect.Privileged || got.Effect.Privileges == nil || !got.Effect.Privileges.HostPID || got.Effect.Privileges.HostNetwork {
		t.Fatalf("stored: %+v %+v", got, got.Effect.Privileges)
	}
}

// A group deleted behind the console's back: the rule naming it is
// reported (it denies everyone).
func TestAccessReportsMissingGroup(t *testing.T) {
	_, call := accessRig(t)
	if rec := call("ac-root", true, "POST", "/api/admin/groups", `{"name":"ac-lab","members":["ac-bob"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("group: %d %s", rec.Code, rec.Body)
	}
	readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{{Match: "group = ac-lab"}}, Default: devpodv1alpha1.DefaultAccessEffect()})))
	if err := k8sClient.Delete(context.Background(), &devpodv1alpha1.UserGroup{ObjectMeta: metav1.ObjectMeta{Name: "ac-lab"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := readAccess(t, call("ac-root", true, "GET", "/api/admin/access", ""))
		if len(got.Problems) == 1 && got.Problems[0].Rule == 0 && got.Problems[0].Message == "点名了不存在的组 ac-lab" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("problems: %+v", got.Problems)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// What a condition names, before it is saved: the clusters, the machines
// (any user), the users (any cluster, any machine) — "maybe" where it
// depends on what is not known.
func TestAccessPreview(t *testing.T) {
	_, call := accessRig(t)
	ctx := context.Background()
	for name, labels := range map[string]map[string]string{"pw-g1": {"pw-gpu": "B300"}, "pw-g2": nil} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
		if err := k8sClient.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, n) })
	}
	for _, u := range []string{"pw-alice", "pw-bob"} {
		obj := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: u}}
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, obj) })
	}
	if rec := call("ac-root", true, "POST", "/api/admin/groups", `{"name":"ac-lab","members":["pw-alice"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("group: %d %s", rec.Code, rec.Body)
	}
	type preview struct {
		Error *struct {
			Offset  int    `json:"offset"`
			Message string `json:"message"`
		} `json:"error"`
		Clusters []struct {
			Name  string `json:"name"`
			Maybe bool   `json:"maybe"`
		} `json:"clusters"`
		Machines []struct {
			Count, Maybe, Unreservable int
			Names                      []string
		} `json:"machines"`
		Users struct {
			Count, Maybe, Total int
			Names               []string
		} `json:"users"`
		HasMachine bool `json:"hasMachine"`
	}
	ask := func(match string, ready func(preview) bool) preview {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"match": match})
		deadline := time.Now().Add(10 * time.Second)
		for {
			var out preview
			rec := call("ac-root", true, "POST", "/api/admin/access/preview", string(body))
			if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &out) == nil && ready(out) {
				return out
			}
			if time.Now().After(deadline) {
				t.Fatalf("preview %q: %d %s", match, rec.Code, rec.Body)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	machines := func(p preview) (count, maybe int) {
		for _, m := range p.Machines {
			count, maybe = count+m.Count, maybe+m.Maybe
		}
		return
	}
	if p := ask("machine in (pw-g*)", func(p preview) bool { c, _ := machines(p); return c == 2 }); !p.HasMachine || p.Users.Count != p.Users.Total || p.Users.Maybe != p.Users.Total || len(p.Clusters) == 0 || !p.Clusters[0].Maybe {
		t.Errorf("machine in (pw-g*): %+v", p)
	}
	if p := ask("machine = pw-gpu=B300", func(p preview) bool { c, _ := machines(p); return c == 1 }); p.Machines[0].Names[0] != "pw-g1" || p.Machines[0].Unreservable != 1 {
		t.Errorf("a label: %+v", p)
	}
	p := ask("group = ac-lab", func(p preview) bool { return p.Users.Count == 1 })
	if !slices.Equal(p.Users.Names, []string{"pw-alice"}) || p.Users.Maybe != 0 || p.HasMachine {
		t.Errorf("group = ac-lab: users %+v", p.Users)
	}
	if c, m := machines(p); c < 2 || c != m {
		t.Errorf("group = ac-lab: every machine, maybe: %d %d", c, m)
	}
	if p := ask("cluster in (", func(p preview) bool { return p.Error != nil }); p.Error.Offset != 12 {
		t.Errorf("error: %+v", p.Error)
	}
}

// A group a rule names is not deleted — wherever it is named, "not"
// included; a glob does not hold it.
func TestGroupDeleteGuard(t *testing.T) {
	_, call := accessRig(t)
	for _, g := range []string{"ac-interns", "ac-x"} {
		if rec := call("ac-root", true, "POST", "/api/admin/groups", `{"name":"`+g+`","members":["ac-bob"]}`); rec.Code != http.StatusCreated {
			t.Fatalf("group %s: %d %s", g, rec.Code, rec.Body)
		}
	}
	saved := readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{{Match: "everyone and not group = ac-interns"}}, Default: devpodv1alpha1.DefaultAccessEffect()})))
	if rec := call("ac-root", true, "DELETE", "/api/admin/groups/ac-interns", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "rule 1") {
		t.Fatalf("named under not: %d %s", rec.Code, rec.Body)
	}
	readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{{Match: "group = ac-intern*"}}, Default: devpodv1alpha1.DefaultAccessEffect(), ResourceVersion: saved.ResourceVersion})))
	if rec := call("ac-root", true, "DELETE", "/api/admin/groups/ac-interns", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("a glob holds it: %d %s", rec.Code, rec.Body)
	}
	setRules(t, devpodv1alpha1.AccessRule{Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Groups: []string{"ac-x"}}})
	if rec := call("ac-root", true, "DELETE", "/api/admin/groups/ac-x", ""); rec.Code != http.StatusConflict {
		t.Fatalf("named in the old form: %d %s", rec.Code, rec.Body)
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
	if rec := call("ac-root", true, "POST", "/api/admin/groups", `{"name":"ac-other","members":["ac-x"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create ac-other: %d %s", rec.Code, rec.Body)
	}
	byGroup := devpodv1alpha1.AccessRule{Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Users: []string{"ac-bob"}, Groups: []string{"ac-other", "ac-infer"}}, Effect: devpodv1alpha1.AccessEffect{Allow: true}}
	saved := readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Rules: []devpodv1alpha1.AccessRule{byGroup}, Default: devpodv1alpha1.DefaultAccessEffect()})))
	for _, g := range []string{"ac-infer", "ac-other"} {
		if rec := call("ac-root", true, "DELETE", "/api/admin/groups/"+g, ""); rec.Code != http.StatusConflict {
			t.Fatalf("delete %s while a rule names it among others: %d %s", g, rec.Code, rec.Body)
		}
	}
	readAccess(t, call("ac-root", true, "PUT", "/api/admin/access", putBody(t, accessBody{Default: devpodv1alpha1.DefaultAccessEffect(), ResourceVersion: saved.ResourceVersion})))
	if rec := call("ac-root", true, "DELETE", "/api/admin/groups/ac-infer", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
}

func TestAccessAdminOnly(t *testing.T) {
	_, call := accessRig(t)
	for _, p := range []string{"/api/admin/access", "/api/admin/groups"} {
		if rec := call("ac-bob", false, "GET", p, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := call("ac-bob", false, "POST", "/api/admin/access/preview", `{"match":""}`); rec.Code != http.StatusForbidden {
		t.Errorf("preview: %d", rec.Code)
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
