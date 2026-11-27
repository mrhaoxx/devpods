// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// Recording never holds the action up: a store that does not answer
// leaves record returning at once.
func TestRecordNeverBlocks(t *testing.T) {
	setupSuite(t)
	s, _ := newServer(t)
	f := &fakeAuditStore{eventHold: make(chan struct{})}
	s.Audit = &webui.AuditDeps{Store: f}
	start := time.Now()
	for range 3 {
		s.RecordForTest(context.Background(), store.EventRow{Kind: "devpod", Object: "x", Actor: "al", Via: "web", Action: "start"})
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("record took %s", d)
	}
	close(f.eventHold)
	waitFor(t, func() bool { return len(f.written()) == 3 })
}

// A record the store does not take is tried again until it does, in order.
func TestRecordRetriesWhenStoreDown(t *testing.T) {
	setupSuite(t)
	s, _ := newServer(t)
	f := &fakeAuditStore{eventErrs: 2}
	s.Audit = &webui.AuditDeps{Store: f}
	s.SetEventRetryForTest(20 * time.Millisecond)
	s.RecordForTest(context.Background(), store.EventRow{Kind: "devpod", Object: "a", Actor: "al", Via: "web", Action: "create"})
	s.RecordForTest(context.Background(), store.EventRow{Kind: "devpod", Object: "a", Actor: "al", Via: "web", Action: "start"})
	waitFor(t, func() bool { return len(f.written()) == 2 })
	if w := f.written(); w[0].Action != "create" || w[1].Action != "start" || w[0].TS.IsZero() {
		t.Fatalf("written %+v", w)
	}
}

// What waited while the store was away goes in a few writes, not one
// each.
func TestRecordBatchesAfterOutage(t *testing.T) {
	setupSuite(t)
	s, _ := newServer(t)
	f := &fakeAuditStore{eventHold: make(chan struct{})}
	s.Audit = &webui.AuditDeps{Store: f}
	for i := range 50 {
		s.RecordForTest(context.Background(), store.EventRow{Kind: "devpod", Object: fmt.Sprintf("o%d", i), Actor: "al", Via: "web", Action: "start"})
	}
	close(f.eventHold)
	waitFor(t, func() bool { return len(f.written()) == 50 })
	if n := f.eventCalls(); n > 3 {
		t.Fatalf("50 records in %d writes", n)
	}
}

// A record the store refuses on its own, while it takes the others, holds
// none of them up and is given up.
func TestRecordGivesUpRefusedRecord(t *testing.T) {
	setupSuite(t)
	s, _ := newServer(t)
	f := &fakeAuditStore{eventBad: "bad"}
	s.Audit = &webui.AuditDeps{Store: f}
	s.SetEventRetryForTest(10 * time.Millisecond)
	for _, o := range []string{"bad", "a", "b"} {
		s.RecordForTest(context.Background(), store.EventRow{Kind: "devpod", Object: o, Actor: "al", Via: "web", Action: "start"})
	}
	waitFor(t, func() bool { return len(f.written()) == 2 })
	s.RecordForTest(context.Background(), store.EventRow{Kind: "devpod", Object: "c", Actor: "al", Via: "web", Action: "start"})
	waitFor(t, func() bool { return len(f.written()) == 3 })
	for _, e := range f.written() {
		if e.Object == "bad" {
			t.Fatalf("the refused record written: %+v", f.written())
		}
	}
}

// A write that went in though it failed (timed out) is sent again as it
// was: the store keeps one copy.
func TestRecordRetryKeepsOneCopy(t *testing.T) {
	setupSuite(t)
	s, _ := newServer(t)
	f := &fakeAuditStore{eventLost: 1}
	s.Audit = &webui.AuditDeps{Store: f}
	s.SetEventRetryForTest(10 * time.Millisecond)
	s.RecordForTest(context.Background(), store.EventRow{Kind: "devpod", Object: "a", Actor: "al", Via: "web", Action: "create"})
	s.RecordForTest(context.Background(), store.EventRow{Kind: "devpod", Object: "a", Actor: "al", Via: "web", Action: "start"})
	waitFor(t, func() bool { return f.eventCalls() >= 2 && len(f.written()) >= 2 })
	time.Sleep(100 * time.Millisecond)
	if w := f.written(); len(w) != 2 {
		t.Fatalf("written %d: %+v", len(w), w)
	}
}

// How a request came is told from how it was signed in: a cookie is the
// web, a token its ID, the CLI's key the CLI — or dp, when the CLI says
// from which DevPod (heard only with the CLI's key).
func TestEventFromVia(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.CLIKey = []byte("cli-key")
	ctx := context.Background()
	sum := sha256.Sum256([]byte("sec"))
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "ev-tok"}, Spec: devpodv1alpha1.UserSpec{
		APITokens: []devpodv1alpha1.APIToken{{ID: "ci", Name: "my ci", Hash: hex.EncodeToString(sum[:]), CreatedAt: metav1.Now()}}}}
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, u) })
	via := func(set func(r *http.Request)) (string, string, bool) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		r.Pattern = "GET /api/me"
		set(r)
		var sess webui.Session
		ok := false
		waitFor(t, func() bool {
			sess, ok = s.FreshSessionForTest(httptest.NewRecorder(), r)
			return ok
		})
		ev := s.EventFromForTest(r, sess)
		return ev.Via, ev.Origin, ev.Actor == sess.User && ev.Actor != ""
	}
	cookie := forge(sm, "ev-web", false)
	for name, c := range map[string]struct {
		set         func(r *http.Request)
		via, origin string
	}{
		"cookie": {func(r *http.Request) { r.AddCookie(cookie) }, "web", ""},
		"cookie with an origin header": {func(r *http.Request) {
			r.AddCookie(cookie)
			r.Header.Set("X-DevPod-Origin", "st/ev-web-dev")
		}, "web", ""},
		"token": {func(r *http.Request) { r.Header.Set("Authorization", "Bearer dpt_ci_sec") }, "token:ci", ""},
		"cli": {func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer gwk_cli-key")
			r.Header.Set("X-DevPod-User", "ev-cli")
		}, "cli", ""},
		"dp": {func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer gwk_cli-key")
			r.Header.Set("X-DevPod-User", "ev-cli")
			r.Header.Set("X-DevPod-Origin", "st/ev-cli-dev")
		}, "dp", "st/ev-cli-dev"},
	} {
		gotVia, gotOrigin, actor := via(c.set)
		if gotVia != c.via || gotOrigin != c.origin || !actor {
			t.Errorf("%s: via %q origin %q actor ok %v", name, gotVia, gotOrigin, actor)
		}
	}
}
