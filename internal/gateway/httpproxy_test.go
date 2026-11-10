// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/httpauth"
)

func TestParseSubdomain(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantPort int
		wantErr  bool
	}{
		{"st43-dev-8080", "st43-dev", 8080, false},
		{"scratch-3000", "scratch", 3000, false},
		{"my-pod-443", "my-pod", 443, false},
		{"a-1", "a", 1, false},
		{"dev-65535", "dev", 65535, false},
		// errors
		{"", "", 0, true},
		{"noport", "", 0, true},
		{"-8080", "", 0, true},
		{"dev-0", "", 0, true},
		{"dev-99999", "", 0, true},
		{"dev-abc", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, port, err := parseSubdomain(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tc.wantName || port != tc.wantPort {
				t.Errorf("got (%q, %d), want (%q, %d)", name, port, tc.wantName, tc.wantPort)
			}
		})
	}
}

// proxyRig: one DevPod (id idA) whose "sshd" endpoint is 127.0.0.1:1 and
// whose HTTP service is a local test server.
func proxyRig(t *testing.T) (*HTTPProxy, func(method, path string, hdr map[string]string) *http.Response, *[]string, string) {
	t.Helper()
	var seen []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" cookie="+r.Header.Get("Cookie"))
		_, _ = io.WriteString(w, "hello from the devpod")
	}))
	t.Cleanup(backend.Close)
	_, bport, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-dev", Namespace: "devpods", Annotations: map[string]string{httpauth.IDAnnotation: idA}},
		Spec:       devpodv1alpha1.DevPodSpec{Owner: "alice", Collaborators: []string{"bob"}, Running: true},
		Status:     devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning, Endpoint: "127.0.0.1:1"},
	}
	p := NewHTTPProxy(newFakeClient(t, dp), "devpods", "apps.test", "-devpod")
	p.Key, p.LoginURL = []byte("k3y"), "https://console.test/http-auth"
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Host = idA + "-" + bport + "-devpod.apps.test"
		for k, v := range hdr {
			if k == "Host" {
				req.Host = v
			} else {
				req.Header.Set(k, v)
			}
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	return p, do, &seen, bport
}

const idA = "abcdefgh2345"

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := devpodv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestHTTPProxy_PrivateByDefault(t *testing.T) {
	p, do, seen, bport := proxyRig(t)
	html := map[string]string{"Accept": "text/html"}

	// a browser is sent to the console for a ticket; anything else gets 401
	resp := do("GET", "/lab?x=1", html)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Host != "console.test" || loc.Path != "/http-auth" ||
		loc.Query().Get("host") != idA+"-"+bport+"-devpod.apps.test" || loc.Query().Get("rd") != "/lab?x=1" {
		t.Fatalf("login redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := do("GET", "/api", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api without cookie: %d", resp.StatusCode)
	}
	if len(*seen) != 0 {
		t.Fatalf("the backend was reached: %v", *seen)
	}

	// the ticket becomes this host's cookie and the browser goes back
	ticket := func(user, id string, admin bool) string {
		return httpauth.Sign(p.Key, httpauth.Claims{Kind: httpauth.KindTicket, ID: id, User: user, Admin: admin, Exp: time.Now().Add(time.Minute).Unix()})
	}
	resp = do("GET", httpauth.AuthPath+"?rd=%2Flab%3Fx%3D1&t="+ticket("bob", idA, false), nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/lab?x=1" || len(resp.Cookies()) != 1 {
		t.Fatalf("auth: %d %s %v", resp.StatusCode, resp.Header.Get("Location"), resp.Cookies())
	}
	ck := resp.Cookies()[0]
	if ck.Name != httpauth.CookieName || !ck.HttpOnly || !ck.Secure || ck.Domain != "" {
		t.Fatalf("cookie %+v", ck)
	}
	// with it the request is proxied, and the program never sees our cookie
	resp = do("GET", "/lab", map[string]string{"Cookie": ck.Name + "=" + ck.Value + "; theirs=1"})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello from the devpod" || len(*seen) != 1 || (*seen)[0] != "/lab cookie=theirs=1" {
		t.Fatalf("proxied: %d %q %v", resp.StatusCode, body, *seen)
	}

	for name, tk := range map[string]string{
		"someone who is not a collaborator": ticket("carol", idA, false),
		"a ticket for another DevPod":       ticket("bob", "zzzzzzzz2222", false),
		"a cookie used as a ticket":         ck.Value,
		"garbage":                           "x.y",
	} {
		if resp := do("GET", httpauth.AuthPath+"?t="+tk, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp := do("GET", httpauth.AuthPath+"?rd=%2F%2Fevil.test&t="+ticket("root", idA, true), nil); resp.Header.Get("Location") != "/" {
		t.Fatalf("open redirect: %s", resp.Header.Get("Location"))
	}

	// a collaborator who was removed loses access with the cookie still valid
	var dp devpodv1alpha1.DevPod
	_ = p.c.Get(context.Background(), client.ObjectKey{Name: "alice-dev", Namespace: "devpods"}, &dp)
	dp.Spec.Collaborators = nil
	if err := p.c.(client.Client).Update(context.Background(), &dp); err != nil {
		t.Fatal(err)
	}
	if resp := do("GET", "/lab", map[string]string{"Cookie": ck.Name + "=" + ck.Value}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed collaborator: %d", resp.StatusCode)
	}
}

func TestHTTPProxy_PublicPortAndGuards(t *testing.T) {
	p, do, seen, bport := proxyRig(t)
	if resp := do("GET", "/", map[string]string{"Host": "zzzzzzzz2222-" + bport + "-devpod.apps.test"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: %d", resp.StatusCode)
	}
	if resp := do("GET", "/", map[string]string{"Host": "alice-dev-" + bport + "-devpod.apps.test"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the DevPod's name must not work as an id: %d", resp.StatusCode)
	}
	if resp := do("GET", "/", map[string]string{"Host": idA + "-" + bport + ".apps.test"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing suffix: %d", resp.StatusCode)
	}
	var dp devpodv1alpha1.DevPod
	_ = p.c.Get(context.Background(), client.ObjectKey{Name: "alice-dev", Namespace: "devpods"}, &dp)
	dp.Annotations[httpauth.PublicPortsAnnotation] = bport + ",1"
	if err := p.c.(client.Client).Update(context.Background(), &dp); err != nil {
		t.Fatal(err)
	}
	if resp := do("GET", "/pub", nil); resp.StatusCode != http.StatusOK || len(*seen) != 1 {
		t.Fatalf("public port: %d %v", resp.StatusCode, *seen)
	}
	// the sshd port is never proxied, public or not
	if resp := do("GET", "/", map[string]string{"Host": idA + "-1-devpod.apps.test"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sshd port: %d", resp.StatusCode)
	}
	// a DevPod that gets its id after the index was built is found once
	// the index is a second old, not only after the full TTL
	fresh := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "bob-dev", Namespace: "devpods", Annotations: map[string]string{httpauth.IDAnnotation: "yyyyyyyy3333", httpauth.PublicPortsAnnotation: bport}},
		Status:     devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning, Endpoint: "127.0.0.1:1"},
	}
	if err := p.c.(client.Client).Create(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p.Now = func() time.Time { return now.Add(2 * time.Second) }
	if resp := do("GET", "/", map[string]string{"Host": "yyyyyyyy3333-" + bport + "-devpod.apps.test"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("fresh id: %d", resp.StatusCode)
	}
	// without a key only public ports work
	p.Key = nil
	if resp := do("GET", "/", map[string]string{"Host": idA + "-9-devpod.apps.test", "Accept": "text/html"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
}
