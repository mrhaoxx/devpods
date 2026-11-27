// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// scopeRig: the audit rig, its cluster's nodes as-n1 and as-n2, and
// carol's sessions there (C1 on as-n1, C2 on as-n2, G1 on a node gone,
// B1 bob's own on as-n2); grants are made per test.
func scopeRig(t *testing.T) (*webui.Server, *webui.SessionManager, *fakeAuditStore) {
	t.Helper()
	setupSuite(t)
	s, sm, st, objs := auditRig(t)
	s.Admins = map[string]bool{"as-root": true} // the audit judges admins afresh, not by the cookie
	ctx := context.Background()
	for _, name := range []string{"as-n1", "as-n2"} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := k8sClient.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), n) })
	}
	t0 := time.Unix(1794210460, 0).UTC()
	seg := []store.RawSegmentRow{{SessionID: "x", Seq: 1, S3Key: "raw/s1/1"}}
	sess := func(id, user, node string) *store.SessionDetail {
		return &store.SessionDetail{Session: store.SessionRow{SessionID: id, User: user, Node: node, OpenedAt: t0}, Segments: seg}
	}
	st.sessions["C1"] = sess("C1", "as-carol", "as-n1")
	st.sessions["C2"] = sess("C2", "as-carol", "as-n2")
	st.sessions["G1"] = sess("G1", "as-carol", "as-gone")
	st.sessions["B1"] = sess("B1", "as-bob", "as-n2")
	file := func(id, session string) *store.FileEventRow {
		return &store.FileEventRow{EventID: id, SessionID: session, User: "as-carol", Path: "/home/carol/" + id, S3Key: "files/s1/1-1"}
	}
	st.files["FC1"], st.files["FC2"], st.files["FX"] = file("FC1", "C1"), file("FC2", "C2"), file("FX", "")
	_ = objs
	return s, sm, st
}

func viewAudit(t *testing.T, s *webui.Server, name, match string) {
	t.Helper()
	grantNow(t, s, name, match, devpodv1alpha1.PermViewAudit)
}

func TestAuditScopeComputed(t *testing.T) {
	s, sm, st := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	viewAudit(t, s, "as-dan", "user = as-dan")
	list := func(user string, admin bool) *store.Scope {
		if rec := route(t, s, "GET", "/api/audit/sessions", forge(sm, user, admin), ""); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", user, rec.Code, rec.Body)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.sessF.Scope
	}
	if got := list("as-bob", false); !reflect.DeepEqual(got, &store.Scope{User: "as-bob", Machines: []store.ClusterNode{{Cluster: "", Node: "as-n1"}}}) {
		t.Errorf("a machine grant: %+v", got)
	}
	if got := list("as-dan", false); !reflect.DeepEqual(got, &store.Scope{User: "as-dan", Clusters: []string{""}}) {
		t.Errorf("a whole-cluster grant: %+v", got)
	}
	if got := list("as-eve", false); !reflect.DeepEqual(got, &store.Scope{User: "as-eve"}) {
		t.Errorf("no grant: %+v", got)
	}
	if got := list("as-root", true); got != nil {
		t.Errorf("an admin: %+v", got)
	}
	// commands, files and HTTP alike
	route(t, s, "GET", "/api/audit/commands", forge(sm, "as-bob", false), "")
	route(t, s, "GET", "/api/audit/files", forge(sm, "as-bob", false), "")
	route(t, s, "GET", "/api/audit/http", forge(sm, "as-bob", false), "")
	st.mu.Lock()
	defer st.mu.Unlock()
	for name, sc := range map[string]*store.Scope{"commands": st.cmdF.Scope, "files": st.fileF.Scope, "http": st.httpF.Scope} {
		if sc == nil || sc.User != "as-bob" || len(sc.Machines) != 1 {
			t.Errorf("%s: %+v", name, sc)
		}
	}
}

// user= narrows within one's scope; it never widens it.
func TestAuditScopeUserParamNarrows(t *testing.T) {
	s, sm, st := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	if rec := route(t, s, "GET", "/api/audit/sessions?user=as-carol", forge(sm, "as-bob", false), ""); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessF.User != "as-carol" || st.sessF.Scope == nil || st.sessF.Scope.User != "as-bob" || len(st.sessF.Scope.Machines) != 1 {
		t.Fatalf("filter %+v scope %+v", st.sessF, st.sessF.Scope)
	}
}

func TestAuditSessionByGrant(t *testing.T) {
	s, sm, _ := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	bob := forge(sm, "as-bob", false)
	if rec := route(t, s, "GET", "/api/audit/sessions/C1", bob, ""); rec.Code != http.StatusOK {
		t.Errorf("C1 on as-n1: %d %s", rec.Code, rec.Body)
	}
	if rec := route(t, s, "GET", "/api/audit/sessions/C1/channels/1/cast", bob, ""); rec.Code != http.StatusOK {
		t.Errorf("C1's replay: %d", rec.Code)
	}
	if rec := route(t, s, "GET", "/api/audit/sessions/C2", bob, ""); rec.Code != http.StatusNotFound {
		t.Errorf("C2 on as-n2: %d", rec.Code)
	}
	// raw records: by the grant, not by being one's own
	if rec := route(t, s, "GET", "/api/audit/sessions/C1/channels/1/raw?stream=out", bob, ""); rec.Code != http.StatusOK {
		t.Errorf("C1's raw: %d %s", rec.Code, rec.Body)
	}
	if rec := route(t, s, "GET", "/api/audit/sessions/B1/channels/1/raw?stream=out", bob, ""); rec.Code != http.StatusForbidden {
		t.Errorf("bob's own raw without a grant there: %d", rec.Code)
	}
}

// A session on a node no longer in the cluster: its machine is not known.
func TestAuditSessionOnGoneNode(t *testing.T) {
	s, sm, _ := scopeRig(t)
	viewAudit(t, s, "as-fay", "user = as-fay and machine = as-gone")
	viewAudit(t, s, "as-dan", "user = as-dan")
	if rec := route(t, s, "GET", "/api/audit/sessions/G1", forge(sm, "as-fay", false), ""); rec.Code != http.StatusNotFound {
		t.Errorf("a machine grant, the machine gone: %d", rec.Code)
	}
	if rec := route(t, s, "GET", "/api/audit/sessions/G1", forge(sm, "as-dan", false), ""); rec.Code != http.StatusOK {
		t.Errorf("a whole-cluster grant: %d", rec.Code)
	}
}

func TestAuditAlertsByGrant(t *testing.T) {
	s, sm, st := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	if rec := route(t, s, "GET", "/api/audit/alerts", forge(sm, "as-eve", false), ""); rec.Code != http.StatusForbidden {
		t.Errorf("no grant: %d", rec.Code)
	}
	if rec := route(t, s, "GET", "/api/audit/alerts", forge(sm, "as-bob", false), ""); rec.Code != http.StatusOK {
		t.Fatalf("a grant: %d %s", rec.Code, rec.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if sc := st.alertF.Scope; sc == nil || sc.User != "" || len(sc.Machines) != 1 {
		t.Errorf("alert scope: %+v", sc)
	}
}

func TestAuditDisconnectByGrant(t *testing.T) {
	s, sm, _ := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	grantNow(t, s, "as-gil", "user = as-gil and machine = as-n1", devpodv1alpha1.PermViewAudit, devpodv1alpha1.PermDisconnectSessions)
	if rec := route(t, s, "POST", "/api/audit/sessions/C1/disconnect", forge(sm, "as-bob", false), ""); rec.Code != http.StatusForbidden {
		t.Errorf("view only: %d", rec.Code)
	}
	if rec := route(t, s, "POST", "/api/audit/sessions/C1/disconnect", forge(sm, "as-gil", false), ""); rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound && !strings.Contains(rec.Body.String(), "LIVE_DISABLED") {
		t.Errorf("view and disconnect: %d %s", rec.Code, rec.Body)
	}
}

// A cluster that cannot be reached: its machine grants wait, the whole-
// cluster ones stand, and the list is answered.
func TestAuditScopeClusterAway(t *testing.T) {
	w := setupWorld(t)
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "hz-b1"}}
	if err := w.b.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.b.Delete(context.Background(), n) })
	st := &fakeAuditStore{sessions: map[string]*store.SessionDetail{}, files: map[string]*store.FileEventRow{}, presence: map[string][]string{}}
	w.s.Audit = &webui.AuditDeps{Store: st, Objects: fakeObjects{}}
	grantNow(t, w.s, "hz-bob-a", "user = hz-bob and cluster = a", devpodv1alpha1.PermViewAudit)
	grantNow(t, w.s, "hz-bob-b", "user = hz-bob and cluster = b and machine = hz-b1", devpodv1alpha1.PermViewAudit)
	scope := func() *store.Scope {
		code, _, _ := w.call("hz-bob", "GET", "/api/audit/sessions", "")
		if code != http.StatusOK {
			t.Fatalf("list: %d", code)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.sessF.Scope
	}
	w.until("b's machine is in the scope", func() bool {
		sc := scope()
		return sc != nil && len(sc.Machines) == 1 && sc.Machines[0] == store.ClusterNode{Cluster: "b", Node: "hz-b1"}
	})
	w.stopB()
	w.until("b is stale", func() bool { _, stale := w.s.Clusters.Stale("b"); return stale })
	if sc := scope(); !reflect.DeepEqual(sc.Clusters, []string{"a"}) || len(sc.Machines) != 0 {
		t.Fatalf("with b away: %+v", sc)
	}
}

// File contents are the audit too: one's own, or by a grant on the machine
// of the file's session; one with no session counts its machine unknown.
func TestAuditFileContentByGrant(t *testing.T) {
	s, sm, _ := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	viewAudit(t, s, "as-dan", "user = as-dan")
	get := func(user, id string) int {
		return route(t, s, "GET", "/api/audit/files/"+id+"/content", forge(sm, user, false), "").Code
	}
	if code := get("as-bob", "FC1"); code != http.StatusOK {
		t.Errorf("bob, a file of a session on as-n1: %d", code)
	}
	if code := get("as-bob", "FC2"); code != http.StatusNotFound {
		t.Errorf("bob, a file of a session on as-n2: %d", code)
	}
	if code := get("as-bob", "FX"); code != http.StatusNotFound {
		t.Errorf("bob, a file of no session: %d", code)
	}
	if code := get("as-dan", "FX"); code != http.StatusOK {
		t.Errorf("dan (the whole cluster), a file of no session: %d", code)
	}
	if code := get("as-eve", "FC1"); code != http.StatusNotFound {
		t.Errorf("eve, no grant: %d", code)
	}
}

// grantsTrouble: the console's client, its AccessGrants failing (the CRD
// not there) or never answering (no RBAC: a cache that cannot sync).
type grantsTrouble struct {
	client.Client
	stall bool
	asked atomic.Int32
}

func (g *grantsTrouble) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*devpodv1alpha1.AccessGrantList); ok {
		g.asked.Add(1)
		if g.stall {
			<-ctx.Done()
			return ctx.Err()
		}
		return errors.New(`no matches for kind "AccessGrant"`)
	}
	return g.Client.List(ctx, list, opts...)
}

// Grants that cannot be read take nothing away: a list holds the viewer
// to their own, as before grants.
func TestAuditGrantsUnreadable(t *testing.T) {
	s, sm, st := scopeRig(t)
	tr := &grantsTrouble{Client: s.Client}
	s.Client = tr
	t.Cleanup(func() { s.Client = tr.Client })
	if rec := route(t, s, "GET", "/api/audit/sessions", forge(sm, "as-eve", false), ""); rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !reflect.DeepEqual(st.sessF.Scope, &store.Scope{User: "as-eve"}) {
		t.Fatalf("scope: %+v", st.sessF.Scope)
	}
}

// Grants that never answer hold no page up: /api/me answers in time
// (without them for an admin, who has every permission anyway).
func TestMeGrantsStall(t *testing.T) {
	s, sm, _ := scopeRig(t)
	tr := &grantsTrouble{Client: s.Client, stall: true}
	s.Client = tr
	t.Cleanup(func() { s.Client = tr.Client })
	me := func(user string, admin bool) int {
		done := make(chan int, 1)
		go func() { done <- route(t, s, "GET", "/api/me", forge(sm, user, admin), "").Code }()
		select {
		case code := <-done:
			return code
		case <-time.After(8 * time.Second):
			t.Fatalf("%s: /api/me waited on the grants", user)
			return 0
		}
	}
	if code := me("as-eve", false); code != http.StatusOK {
		t.Fatalf("eve: %d", code)
	}
	before := tr.asked.Load()
	if code := me("as-root", true); code != http.StatusOK || tr.asked.Load() != before {
		t.Fatalf("an admin: %d, grants asked %d times", code, tr.asked.Load()-before)
	}
}

// The record of what was done is read in one's scope, narrowed as asked.
func TestAuditEventsScope(t *testing.T) {
	s, sm, st := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	viewAudit(t, s, "as-dan", "user = as-dan")
	list := func(user string, admin bool, query string) store.EventFilter {
		t.Helper()
		if rec := route(t, s, "GET", "/api/audit/events"+query, forge(sm, user, admin), ""); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", user, rec.Code, rec.Body)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.eventF
	}
	if f := list("as-eve", false, ""); !reflect.DeepEqual(f.Scope, &store.Scope{User: "as-eve"}) {
		t.Errorf("no grant: %+v", f.Scope)
	}
	if f := list("as-bob", false, ""); !reflect.DeepEqual(f.Scope, &store.Scope{User: "as-bob", Machines: []store.ClusterNode{{Cluster: "", Node: "as-n1"}}}) {
		t.Errorf("a machine grant: %+v", f.Scope)
	}
	if f := list("as-dan", false, ""); !reflect.DeepEqual(f.Scope, &store.Scope{User: "as-dan", Clusters: []string{""}}) {
		t.Errorf("a whole-cluster grant: %+v", f.Scope)
	}
	f := list("as-root", true, "?kind=devpod&action=start&object=x&owner=o&actor=a")
	if f.Scope != nil || f.Kind != "devpod" || f.Action != "start" || f.Object != "x" || f.Owner != "o" || f.Actor != "a" || f.From.IsZero() {
		t.Errorf("an admin's: %+v", f)
	}
}

// A DevPod's history is its owner's, its collaborators', an admin's and
// its machine's auditors' to read — of this DevPod, since it was made.
func TestDevPodHistory(t *testing.T) {
	s, sm, st := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	ctx := context.Background()
	dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "as-carol-h", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: "as-carol", Collaborators: []string{"as-cat"},
		Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "as-n1"}, Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}}}
	if err := k8sClient.Create(ctx, dp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, dp) })
	waitFor(t, func() bool { return waitDevPod(t, dp.Name).UID != "" })
	st.mu.Lock()
	st.events = []store.EventRow{
		{Kind: "devpod", Object: dp.Name, ObjectUID: string(dp.UID), Owner: "as-carol", Actor: "as-carol", Action: "create"},
		{Kind: "devpod", Object: dp.Name, ObjectUID: "an-earlier-one", Owner: "as-carol", Actor: "as-carol", Action: "delete"},
	}
	st.mu.Unlock()
	get := func(user string, admin bool) (int, []store.EventRow) {
		t.Helper()
		rec := route(t, s, "GET", "/api/devpods/"+dp.Name+"/history", forge(sm, user, admin), "")
		var out struct{ Items []store.EventRow }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Items
	}
	for _, who := range []struct {
		user  string
		admin bool
	}{{"as-carol", false}, {"as-cat", false}, {"as-bob", false}, {"as-root", true}} {
		code, items := get(who.user, who.admin)
		if code != http.StatusOK || len(items) != 1 || items[0].Action != "create" {
			t.Errorf("%s: %d %+v", who.user, code, items)
		}
	}
	if code, _ := get("as-eve", false); code != http.StatusNotFound {
		t.Errorf("a stranger: %d", code)
	}
	st.mu.Lock()
	from := st.eventF.From
	st.mu.Unlock()
	if from.After(dp.CreationTimestamp.Time) {
		t.Errorf("from %s, after it was made (%s)", from, dp.CreationTimestamp.Time)
	}
}

// Who did something from where is the auditors' (and admins') to see: an
// owner or a collaborator reading a history sees their own client IP, not
// another's. The history is asked by the DevPod's name too (the store's
// order key).
func TestHistoryClientIPs(t *testing.T) {
	s, sm, st := scopeRig(t)
	viewAudit(t, s, "as-bob", "user = as-bob and machine = as-n1")
	ctx := context.Background()
	dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "as-carol-ip", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: "as-carol", Collaborators: []string{"as-cat"},
		Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "as-n1"}, Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}}}
	if err := k8sClient.Create(ctx, dp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, dp) })
	waitFor(t, func() bool { return waitDevPod(t, dp.Name).UID != "" })
	row := func(actor, action, ip string) store.EventRow {
		return store.EventRow{Kind: "devpod", Object: dp.Name, ObjectUID: string(dp.UID), Owner: "as-carol", Actor: actor, Action: action, ClientIP: ip}
	}
	st.mu.Lock()
	st.events = []store.EventRow{row("as-carol", "create", "10.0.0.1"), row("as-cat", "edit", "10.0.0.5"), row("as-root", "stop", "10.0.0.9")}
	st.mu.Unlock()
	ips := func(path, user string, admin bool) map[string]string {
		t.Helper()
		rec := route(t, s, "GET", path, forge(sm, user, admin), "")
		var out struct{ Items []store.EventRow }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%s as %s: %d %s", path, user, rec.Code, rec.Body)
		}
		m := map[string]string{}
		for _, e := range out.Items {
			m[e.Actor] = e.ClientIP
		}
		return m
	}
	history := "/api/devpods/" + dp.Name + "/history"
	all := map[string]string{"as-carol": "10.0.0.1", "as-cat": "10.0.0.5", "as-root": "10.0.0.9"}
	for _, c := range []struct {
		path, user string
		admin      bool
		want       map[string]string
	}{
		{history, "as-carol", false, map[string]string{"as-carol": "10.0.0.1", "as-cat": "", "as-root": ""}},
		{history, "as-cat", false, map[string]string{"as-carol": "", "as-cat": "10.0.0.5", "as-root": ""}},
		{history, "as-bob", false, all},
		{history, "as-root", true, all},
		{"/api/audit/events", "as-carol", false, map[string]string{"as-carol": "10.0.0.1", "as-cat": "", "as-root": ""}},
		{"/api/audit/events", "as-root", true, all},
	} {
		if got := ips(c.path, c.user, c.admin); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s as %s: %v, want %v", c.path, c.user, got, c.want)
		}
	}
	ips(history, "as-carol", false)
	st.mu.Lock()
	f := st.eventF
	st.mu.Unlock()
	if f.Object != dp.Name || f.Kind != "devpod" || f.ObjectUID != string(dp.UID) {
		t.Errorf("the history asked %+v", f)
	}
}
