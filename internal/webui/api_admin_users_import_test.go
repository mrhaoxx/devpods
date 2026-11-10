// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestImportCSVRows(t *testing.T) {
	in := "\xef\xbb\xbfUsername,displayName,password\r\n" +
		"im-a,Alice\r\n" +
		"\r\n" +
		"im-b,\"Li, Lei\",given-pass-1\r\n" +
		",NoName\r\n" +
		"Bad_Name,X\r\n" +
		"im-a,Again\r\n" +
		"im-c,C,p,extra\r\n" +
		"  im-d , Dee \r\n"
	rows := webui.ParseImportCSVForTest([]byte(in))
	type want struct {
		line          int
		user, display string
		pass          string
		errContains   string
	}
	wants := []want{
		{2, "im-a", "Alice", "", ""},
		{4, "im-b", "Li, Lei", "given-pass-1", ""},
		{5, "", "NoName", "", "username"},
		{6, "Bad_Name", "X", "", "DNS-1123"},
		{7, "im-a", "Again", "", "duplicate"},
		{8, "im-c", "C", "p", "username,displayName[,password]"},
		{9, "im-d", "Dee", "", ""},
	}
	if len(rows) != len(wants) {
		t.Fatalf("rows = %+v", rows)
	}
	for i, w := range wants {
		r := rows[i]
		if r.Line != w.line || r.Username != w.user || r.DisplayName != w.display || r.Password != w.pass {
			t.Errorf("row %d = %+v, want %+v", i, r, w)
		}
		if (w.errContains == "") != (r.Err == "") || !strings.Contains(r.Err, w.errContains) {
			t.Errorf("row %d err = %q, want containing %q", i, r.Err, w.errContains)
		}
	}
}

type importResp struct {
	Rows []struct {
		Line     int    `json:"line"`
		Username string `json:"username"`
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Password string `json:"password"`
	} `json:"rows"`
	Created int `json:"created"`
}

func TestImportUsers(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	s.PasswordAuth = true
	root := forge(sm, "im-root", true)
	mkUser(t, "im-x", nil)
	t.Cleanup(func() {
		for _, n := range []string{"im-a", "im-c", "im-dry"} {
			_ = k8sClient.Delete(context.Background(), &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: n}})
		}
	})
	post := func(t *testing.T, c *http.Cookie, target, body string, want int) importResp {
		t.Helper()
		rec := doJSON(t, s.HandleImportUsersForTest(), "POST", target, nil, c, body)
		if rec.Code != want {
			t.Fatalf("import: %d (want %d) %s", rec.Code, want, rec.Body)
		}
		var out importResp
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	exists := func(name string) bool {
		var u devpodv1alpha1.User
		return k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Name: name}, &u) == nil
	}

	t.Run("dryRun writes nothing", func(t *testing.T) {
		out := post(t, root, "/api/admin/users:import?dryRun=1", "im-dry,D\nim-x,X\n", http.StatusOK)
		if len(out.Rows) != 2 || !out.Rows[0].OK || out.Rows[1].OK || out.Created != 0 {
			t.Fatalf("out = %+v", out)
		}
		if out.Rows[0].Password != "" || exists("im-dry") {
			t.Fatalf("dry run created or returned a password: %+v", out)
		}
	})
	t.Run("commit creates valid rows and reports existing users as errors", func(t *testing.T) {
		out := post(t, root, "/api/admin/users:import", "im-a,A\nim-x,X\nim-c,C,given-pass-12\n", http.StatusOK)
		if out.Created != 2 || len(out.Rows) != 3 {
			t.Fatalf("out = %+v", out)
		}
		if !out.Rows[0].OK || len(out.Rows[0].Password) != 16 {
			t.Fatalf("generated password row: %+v", out.Rows[0])
		}
		if out.Rows[1].OK || !strings.Contains(out.Rows[1].Error, "exists") {
			t.Fatalf("existing user row: %+v", out.Rows[1])
		}
		if !out.Rows[2].OK || out.Rows[2].Password != "" {
			t.Fatalf("given passwords are never echoed: %+v", out.Rows[2])
		}
		if !exists("im-a") || !exists("im-c") {
			t.Fatal("users not created")
		}
		rec := doJSON(t, s.HandlePasswordLoginForTest(), "POST", "/api/auth/password", nil, nil, `{"username":"im-c","password":"given-pass-12"}`)
		if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
			t.Fatalf("login: %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("body over 1 MiB is 413", func(t *testing.T) {
		big := strings.Repeat(fmt.Sprintf("u%07d,x\n", 1), (1<<20)/10+10)
		post(t, root, "/api/admin/users:import?dryRun=1", big, http.StatusRequestEntityTooLarge)
	})
	t.Run("non-admin 403", func(t *testing.T) {
		post(t, forge(sm, "im-a", false), "/api/admin/users:import?dryRun=1", "im-z,Z\n", http.StatusForbidden)
	})
}
