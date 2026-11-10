// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// searchStore serves canned sessions and commands, honouring the user
// scope the handler asks for.
type searchStore struct {
	fakeAuditStore
	sess []store.SessionSummary
	cmds []store.CommandRow
	err  error
}

func (f *searchStore) ListSessions(_ context.Context, sf store.SessionFilter) ([]store.SessionSummary, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	var out []store.SessionSummary
	for _, s := range f.sess {
		if sf.User == "" || s.User == sf.User {
			out = append(out, s)
		}
	}
	return out, "", nil
}

func (f *searchStore) SearchCommands(_ context.Context, cf store.CommandFilter) ([]store.CommandRow, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	var out []store.CommandRow
	for _, c := range f.cmds {
		if (cf.User == "" || c.User == cf.User) && strings.Contains(c.Cmdline, cf.Q) {
			out = append(out, c)
		}
	}
	return out, "", nil
}

type searchResp struct {
	Users []struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"users"`
	DevPods []struct {
		Name  string `json:"name"`
		Owner string `json:"owner"`
	} `json:"devpods"`
	Sessions []struct {
		ID     string `json:"id"`
		User   string `json:"user"`
		DevPod string `json:"devpod"`
	} `json:"sessions"`
	Commands []struct {
		SessionID string `json:"sessionId"`
		DevPod    string `json:"devpod"`
		Cmd       string `json:"cmd"`
	} `json:"commands"`
	AuditError string `json:"auditError"`
}

func TestSearch(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	root := forge(sm, "sr-root", true)
	alice := forge(sm, "sr-alice", false)
	// search re-resolves admin from the User object (as the audit
	// endpoints do), so the admin needs one the cache already holds
	mkUser(t, "sr-root", func(u *devpodv1alpha1.User) { u.Spec.Admin = true })
	mkUser(t, "sr-alice", func(u *devpodv1alpha1.User) { u.Spec.DisplayName = "Alice Liddell" })
	mkUser(t, "sr-bob", nil)
	for i := 0; i < 6; i++ {
		mkUser(t, fmt.Sprintf("sr-u%d", i), nil)
	}
	t.Cleanup(func() { cleanupDevPods(t) })
	for _, c := range []struct {
		who  *http.Cookie
		body string
		name string
	}{
		{alice, `{"name":"train","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}`, "sr-alice-train"},
		{forge(sm, "sr-bob", false), `{"name":"trainer","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}`, "sr-bob-trainer"},
	} {
		if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, c.who, c.body); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", c.name, rec.Code, rec.Body)
		}
		waitDevPod(t, c.name)
	}
	aliceUID := string(waitDevPod(t, "sr-alice-train").UID)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var u devpodv1alpha1.User
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "sr-u5"}, &u); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("users never reached the cache")
		}
	}

	search := func(t *testing.T, c *http.Cookie, q string, want int) searchResp {
		t.Helper()
		rec := doJSON(t, s.HandleSearchForTest(), "GET", "/api/search?q="+url.QueryEscape(q), nil, c, "")
		if rec.Code != want {
			t.Fatalf("search %q: %d (want %d) %s", q, rec.Code, want, rec.Body)
		}
		var out searchResp
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if want == http.StatusOK && (out.Users == nil || out.DevPods == nil || out.Sessions == nil || out.Commands == nil) {
			t.Fatalf("groups must be arrays, not null: %s", rec.Body)
		}
		return out
	}

	t.Run("admin finds users and other owners' DevPods", func(t *testing.T) {
		out := search(t, root, "liddell", http.StatusOK)
		if len(out.Users) != 1 || out.Users[0].Name != "sr-alice" {
			t.Fatalf("users = %+v", out.Users)
		}
		out = search(t, root, "train", http.StatusOK)
		if len(out.DevPods) != 2 {
			t.Fatalf("devpods = %+v", out.DevPods)
		}
	})
	t.Run("non-admin sees only own DevPods and no users", func(t *testing.T) {
		out := search(t, alice, "sr-", http.StatusOK)
		if len(out.Users) != 0 {
			t.Fatalf("users = %+v", out.Users)
		}
		out = search(t, alice, "train", http.StatusOK)
		if len(out.DevPods) != 1 || out.DevPods[0].Name != "sr-alice-train" {
			t.Fatalf("devpods = %+v", out.DevPods)
		}
	})
	t.Run("at most 5 per group", func(t *testing.T) {
		if out := search(t, root, "sr-u", http.StatusOK); len(out.Users) != 5 {
			t.Fatalf("users = %d", len(out.Users))
		}
	})
	t.Run("bad q is 400", func(t *testing.T) {
		search(t, root, "  ", http.StatusBadRequest)
		search(t, root, strings.Repeat("x", 101), http.StatusBadRequest)
	})
	t.Run("audit off returns empty sessions and commands", func(t *testing.T) {
		out := search(t, alice, "train", http.StatusOK)
		if len(out.Sessions) != 0 || len(out.Commands) != 0 {
			t.Fatalf("out = %+v", out)
		}
	})
	t.Run("audit groups are scoped to the viewer", func(t *testing.T) {
		st := &searchStore{
			sess: []store.SessionSummary{
				{SessionRow: store.SessionRow{SessionID: "abc123", User: "sr-alice", DevPod: "sr-alice-train", OpenedAt: time.Now()}},
				{SessionRow: store.SessionRow{SessionID: "zzz999", User: "sr-bob", DevPod: "sr-bob-trainer", OpenedAt: time.Now()}},
			},
			cmds: []store.CommandRow{{SessionID: "abc123", User: "sr-alice", DevPodUID: aliceUID, Cmdline: "python train.py", TS: time.Now()}},
		}
		s.Audit = &webui.AuditDeps{Store: st}
		defer func() { s.Audit = nil }()
		out := search(t, alice, "train", http.StatusOK)
		if len(out.Sessions) != 1 || out.Sessions[0].ID != "abc123" {
			t.Fatalf("sessions = %+v", out.Sessions)
		}
		if len(out.Commands) != 1 || out.Commands[0].DevPod != "sr-alice-train" || out.Commands[0].Cmd != "python train.py" {
			t.Fatalf("commands = %+v", out.Commands)
		}
		st.err = errors.New("clickhouse down")
		out = search(t, alice, "train", http.StatusOK)
		if out.AuditError == "" || len(out.Sessions) != 0 || len(out.DevPods) != 1 {
			t.Fatalf("audit error not reported: %+v", out)
		}
	})
}
