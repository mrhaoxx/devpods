// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/httpauth"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestHTTPProxyAPI(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "hp-alice", false)
	bob := forge(sm, "hp-bob", false)
	root := forge(sm, "hp-root", true)
	t.Cleanup(func() { cleanupDevPods(t) })
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "hp-alice-dev1")
	path := map[string]string{"name": "hp-alice-dev1"}
	type view struct {
		ID          string `json:"id"`
		URLTemplate string `json:"urlTemplate"`
		PublicPorts []int  `json:"publicPorts"`
		UserPorts   []int  `json:"userPorts"`
		URL         string `json:"url"`
	}
	call := func(h http.HandlerFunc, method, target string, c *http.Cookie, body string, want int) view {
		t.Helper()
		var v view
		deadline := time.Now().Add(10 * time.Second)
		for { // writes reach the cached client a moment later
			rec := doJSON(t, h, method, target, path, c, body)
			if rec.Code == want {
				_ = json.Unmarshal(rec.Body.Bytes(), &v)
				return v
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s: %d (want %d) %s", method, target, rec.Code, want, rec.Body)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	get, put, ticket := s.HandleGetHTTPForTest(), s.HandlePutHTTPForTest(), s.HandleHTTPTicketForTest()

	call(get, "GET", "/api/devpods/hp-alice-dev1/http", alice, "", http.StatusNotFound) // off
	hp, err := webui.NewHTTPProxy("{id}-{port}-devpod.apps.test", "", []byte("k3y"))
	if err != nil {
		t.Fatal(err)
	}
	s.HTTPProxy = hp
	if _, err := webui.NewHTTPProxy("{port}.apps.test", "", []byte("k")); err == nil {
		t.Fatal("a template without {id} was accepted")
	}

	v := call(get, "GET", "/api/devpods/hp-alice-dev1/http", alice, "", http.StatusOK)
	if !httpauth.ValidID(v.ID) || v.URLTemplate != "https://"+v.ID+"-{port}-devpod.apps.test" || len(v.PublicPorts) != 0 {
		t.Fatalf("view %+v", v)
	}
	if again := call(get, "GET", "/api/devpods/hp-alice-dev1/http", alice, "", http.StatusOK); again.ID != v.ID {
		t.Fatalf("the id changed on a second look: %s → %s", v.ID, again.ID)
	}
	call(get, "GET", "/api/devpods/hp-alice-dev1/http", bob, "", http.StatusNotFound) // not theirs

	pub := call(put, "PUT", "/api/devpods/hp-alice-dev1/http", alice, `{"publicPorts":[8888,8080,8080]}`, http.StatusOK)
	if len(pub.PublicPorts) != 2 || pub.PublicPorts[0] != 8080 || pub.ID != v.ID {
		t.Fatalf("public %+v", pub)
	}
	call(put, "PUT", "/api/devpods/hp-alice-dev1/http", alice, `{"publicPorts":[70000]}`, http.StatusBadRequest)

	host := v.ID + "-8080-devpod.apps.test"
	body := func(h, rd string) string {
		b, _ := json.Marshal(map[string]string{"host": h, "rd": rd})
		return string(b)
	}
	tk := call(ticket, "POST", "/api/http/ticket", alice, body(host, "/lab?x=1"), http.StatusOK)
	u, _ := url.Parse(tk.URL)
	c, err := httpauth.Verify([]byte("k3y"), u.Query().Get("t"), httpauth.KindTicket, time.Now())
	if err != nil || u.Scheme != "https" || u.Host != host || u.Path != httpauth.AuthPath || u.Query().Get("rd") != "/lab?x=1" || c.ID != v.ID || c.User != "hp-alice" || c.Admin {
		t.Fatalf("ticket url %s claims %+v err %v", tk.URL, c, err)
	}
	// admins get in; strangers and unknown ids get the same answer
	if tk := call(ticket, "POST", "/api/http/ticket", root, body(host, "/"), http.StatusOK); !strings.Contains(tk.URL, host) {
		t.Fatalf("admin ticket %s", tk.URL)
	}
	// (8080 is public by now, so the stranger is tried on a private port)
	call(ticket, "POST", "/api/http/ticket", bob, body(v.ID+"-7000-devpod.apps.test", "/"), http.StatusForbidden)
	call(ticket, "POST", "/api/http/ticket", alice, body("zzzzzzzz2222-8080-devpod.apps.test", "/"), http.StatusForbidden)
	// a ticket is only ever sent to a proxy hostname
	call(ticket, "POST", "/api/http/ticket", alice, body("evil.test", "/"), http.StatusBadRequest)
	call(ticket, "POST", "/api/http/ticket", alice, body(v.ID+"-8080-devpod.apps.test.evil.test", "/"), http.StatusBadRequest)
	if tk := call(ticket, "POST", "/api/http/ticket", alice, body(host, "//evil.test"), http.StatusOK); !strings.Contains(tk.URL, "rd=%2F&") && !strings.HasSuffix(tk.URL, "rd=%2F") {
		t.Fatalf("rd not neutralised: %s", tk.URL)
	}

	// a port opened to signed-in users: a stranger gets a ticket for that
	// port only
	users := call(put, "PUT", "/api/devpods/hp-alice-dev1/http", alice, `{"userPorts":[9000]}`, http.StatusOK)
	if len(users.UserPorts) != 1 || users.UserPorts[0] != 9000 || len(users.PublicPorts) != 2 {
		t.Fatalf("user ports %+v", users)
	}
	call(ticket, "POST", "/api/http/ticket", bob, body(v.ID+"-9000-devpod.apps.test", "/"), http.StatusOK)
	call(ticket, "POST", "/api/http/ticket", bob, body(v.ID+"-9001-devpod.apps.test", "/"), http.StatusForbidden)

	// a reset draws a new id: the old address is gone
	re := call(put, "PUT", "/api/devpods/hp-alice-dev1/http", alice, `{"reset":true}`, http.StatusOK)
	if re.ID == v.ID || !httpauth.ValidID(re.ID) || len(re.PublicPorts) != 2 {
		t.Fatalf("reset %+v", re)
	}
	call(ticket, "POST", "/api/http/ticket", alice, body(host, "/"), http.StatusForbidden)
}
