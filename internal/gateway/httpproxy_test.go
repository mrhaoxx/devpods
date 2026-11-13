// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
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
	_ = p.dir.(*kubeDirectory).c.Get(context.Background(), client.ObjectKey{Name: "alice-dev", Namespace: "devpods"}, &dp)
	dp.Spec.Collaborators = nil
	if err := p.dir.(*kubeDirectory).c.(client.Client).Update(context.Background(), &dp); err != nil {
		t.Fatal(err)
	}
	if resp := do("GET", "/lab", map[string]string{"Cookie": ck.Name + "=" + ck.Value}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed collaborator: %d", resp.StatusCode)
	}
}

// A port opened to signed-in users: anyone the console signed in gets
// through, nobody without a cookie does.
func TestHTTPProxy_SignedInUsersPort(t *testing.T) {
	p, do, seen, bport := proxyRig(t)
	var dp devpodv1alpha1.DevPod
	_ = p.dir.(*kubeDirectory).c.Get(context.Background(), client.ObjectKey{Name: "alice-dev", Namespace: "devpods"}, &dp)
	dp.Annotations[httpauth.UserPortsAnnotation] = bport
	if err := p.dir.(*kubeDirectory).c.(client.Client).Update(context.Background(), &dp); err != nil {
		t.Fatal(err)
	}
	if resp := do("GET", "/", map[string]string{"Accept": "text/html"}); resp.StatusCode != http.StatusFound {
		t.Fatalf("no cookie: %d (want the sign-in redirect)", resp.StatusCode)
	}
	if resp := do("GET", "/api", nil); resp.StatusCode != http.StatusUnauthorized || len(*seen) != 0 {
		t.Fatalf("no cookie, not a browser: %d %v", resp.StatusCode, *seen)
	}
	// carol is neither owner nor collaborator
	tk := httpauth.Sign(p.Key, httpauth.Claims{Kind: httpauth.KindTicket, ID: idA, User: "carol", Exp: time.Now().Add(time.Minute).Unix()})
	resp := do("GET", httpauth.AuthPath+"?rd=%2F&t="+tk, nil)
	if resp.StatusCode != http.StatusFound || len(resp.Cookies()) != 1 {
		t.Fatalf("ticket for a signed-in stranger: %d", resp.StatusCode)
	}
	ck := resp.Cookies()[0]
	if resp := do("GET", "/app", map[string]string{"Cookie": ck.Name + "=" + ck.Value}); resp.StatusCode != http.StatusOK || len(*seen) != 1 {
		t.Fatalf("signed-in user: %d %v", resp.StatusCode, *seen)
	}
	// the same cookie does not open a private port of the same DevPod
	if resp := do("GET", "/", map[string]string{"Host": idA + "-9-devpod.apps.test", "Cookie": ck.Name + "=" + ck.Value}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("private port with a stranger's cookie: %d", resp.StatusCode)
	}
	// closing the port again shuts the stranger out at once
	delete(dp.Annotations, httpauth.UserPortsAnnotation)
	_ = p.dir.(*kubeDirectory).c.Get(context.Background(), client.ObjectKey{Name: "alice-dev", Namespace: "devpods"}, &dp)
	delete(dp.Annotations, httpauth.UserPortsAnnotation)
	if err := p.dir.(*kubeDirectory).c.(client.Client).Update(context.Background(), &dp); err != nil {
		t.Fatal(err)
	}
	if resp := do("GET", "/app", map[string]string{"Cookie": ck.Name + "=" + ck.Value}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after closing: %d", resp.StatusCode)
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
	_ = p.dir.(*kubeDirectory).c.Get(context.Background(), client.ObjectKey{Name: "alice-dev", Namespace: "devpods"}, &dp)
	dp.Annotations[httpauth.PublicPortsAnnotation] = bport + ",1"
	if err := p.dir.(*kubeDirectory).c.(client.Client).Update(context.Background(), &dp); err != nil {
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
	if err := p.dir.(*kubeDirectory).c.(client.Client).Create(context.Background(), fresh); err != nil {
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

// oneDevPod is a directory of several clusters holding one DevPod.
type oneDevPod struct {
	cluster string
	dp      *devpodv1alpha1.DevPod
}

func (d oneDevPod) Lookup(context.Context, string, string, string) ([]Found, error) { return nil, nil }
func (d oneDevPod) ByHTTPID(_ context.Context, id string) (*Found, error) {
	if d.dp.Annotations[httpauth.IDAnnotation] != id {
		return nil, nil
	}
	return &Found{Cluster: d.cluster, DevPod: d.dp}, nil
}

// A gateway of several clusters has no route to a Pod: a port link goes
// to the DevPod's cluster through that cluster's agent, said to be HTTP.
func TestHTTPProxy_ThroughTheClustersAgent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello from mulan") }))
	defer backend.Close()
	real := strings.TrimPrefix(backend.URL, "http://")
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-dev", Namespace: "devpods", UID: "uid-alice-dev", Annotations: map[string]string{
			httpauth.IDAnnotation: idA, httpauth.PublicPortsAnnotation: "8080"}},
		Spec:   devpodv1alpha1.DevPodSpec{Owner: "alice", Running: true},
		Status: devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning, Endpoint: "10.9.9.9:22"}, // nowhere this test can reach
	}
	p := NewHTTPProxyWith(oneDevPod{cluster: "mulan", dp: dp}, "apps.test", "")
	var got []Target
	p.Net = func(ctx context.Context, t Target) (net.Conn, error) {
		got = append(got, t)
		return (&net.Dialer{}).DialContext(ctx, "tcp", real) // what the agent would connect to
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	get := func() string {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/", nil)
		req.Host = idA + "-8080.apps.test"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
		return string(b)
	}
	if body := get(); body != "hello from mulan" {
		t.Fatalf("body %q", body)
	}
	if len(got) != 1 || got[0] != (Target{Cluster: "mulan", Addr: "10.9.9.9:8080", Purpose: PurposeHTTP, DevPodUID: "uid-alice-dev"}) {
		t.Fatalf("what the agent was asked for: %+v", got)
	}
	// the connection is kept for the next request, not dialled again
	if body := get(); body != "hello from mulan" || len(got) != 1 {
		t.Fatalf("second request: %q after %d dials", body, len(got))
	}
}

// fakeHTTPAudit admits (and records) while serve, and keeps the rows.
type fakeHTTPAudit struct {
	mu    sync.Mutex
	serve bool
	rows  []store.HTTPRequestRow
}

func (f *fakeHTTPAudit) Admit(context.Context, string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.serve, f.serve
}

func (f *fakeHTTPAudit) Record(r store.HTTPRequestRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, r)
}

// waitRows: the rows once there are n (a row is recorded as its request
// ends, after the answer may already have reached the client).
func (f *fakeHTTPAudit) waitRows(t *testing.T, n int) []store.HTTPRequestRow {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		rows := append([]store.HTTPRequestRow(nil), f.rows...)
		f.mu.Unlock()
		if len(rows) >= n || time.Now().After(deadline) {
			if len(rows) != n {
				t.Fatalf("%d rows, want %d: %+v", len(rows), n, rows)
			}
			return rows
		}
	}
}

// Each request to a DevPod is recorded as it ended — refusals and the
// sign-in included, a public port's visitor named when their cookie is
// good; nothing is served while audit storage refuses.
func TestHTTPProxy_Audit(t *testing.T) {
	p, do, seen, bport := proxyRig(t)
	fa := &fakeHTTPAudit{serve: true}
	p.Audit = fa
	if resp := do("GET", "/", map[string]string{"Host": "zzzzzzzz2222-" + bport + "-devpod.apps.test"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: %d", resp.StatusCode)
	}
	if resp := do("GET", "/api", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}
	rows := fa.waitRows(t, 1) // the unknown id left none
	if r := rows[0]; r.User != "" || r.Status != 401 || r.Access != "private" || r.DevPod != "alice-dev" || r.Path != "/api" || r.Method != "GET" {
		t.Fatalf("refused: %+v", r)
	}
	// bob, a collaborator, signs in and reaches the private port
	tk := httpauth.Sign(p.Key, httpauth.Claims{Kind: httpauth.KindTicket, ID: idA, User: "bob", Exp: time.Now().Add(time.Minute).Unix()})
	resp := do("GET", httpauth.AuthPath+"?rd=%2F&t="+tk, nil)
	if resp.StatusCode != http.StatusFound || len(resp.Cookies()) != 1 {
		t.Fatalf("ticket: %d", resp.StatusCode)
	}
	bob := resp.Cookies()[0].Name + "=" + resp.Cookies()[0].Value
	if resp := do("GET", "/x?a=1&token=t", map[string]string{"Cookie": bob}); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %d", resp.StatusCode)
	}
	rows = fa.waitRows(t, 3)
	if r := rows[1]; r.User != "bob" || r.Path != httpauth.AuthPath || r.Status != http.StatusFound {
		t.Fatalf("sign-in: %+v", r)
	}
	if r := rows[2]; r.User != "bob" || r.Status != 200 || r.Path != "/x" || r.Query != "a=1&token=t" ||
		r.BytesOut != uint64(len("hello from the devpod")) || r.Websocket || r.Port == 0 || r.RequestID == "" {
		t.Fatalf("served: %+v", r)
	}
	// a public port: anyone, named only by a good cookie, never sent to sign in
	var dp devpodv1alpha1.DevPod
	_ = p.dir.(*kubeDirectory).c.Get(context.Background(), client.ObjectKey{Name: "alice-dev", Namespace: "devpods"}, &dp)
	dp.Annotations[httpauth.PublicPortsAnnotation] = bport
	if err := p.dir.(*kubeDirectory).c.(client.Client).Update(context.Background(), &dp); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cookie, user string }{{"", ""}, {httpauth.CookieName + "=garbage", ""}, {bob, "bob"}} {
		fa.mu.Lock()
		n := len(fa.rows)
		fa.mu.Unlock()
		if resp := do("GET", "/pub", map[string]string{"Cookie": c.cookie, "Accept": "text/html"}); resp.StatusCode != http.StatusOK {
			t.Fatalf("public port, cookie %q: %d", c.cookie, resp.StatusCode)
		}
		if r := fa.waitRows(t, n+1)[n]; r.User != c.user || r.Access != "public" {
			t.Fatalf("public port, cookie %q: %+v", c.cookie, r)
		}
	}
	// audit storage refuses: nothing is served, nothing recorded
	fa.mu.Lock()
	fa.serve = false
	n, before := len(fa.rows), len(*seen)
	fa.mu.Unlock()
	if resp := do("GET", "/pub", nil); resp.StatusCode != http.StatusServiceUnavailable || len(*seen) != before {
		t.Fatalf("refused by audit: %d, served %d", resp.StatusCode, len(*seen)-before)
	}
	time.Sleep(50 * time.Millisecond)
	fa.waitRows(t, n)
}

// A WebSocket through the proxy still upgrades and streams, and is one
// row when it closes, counting both directions.
func TestHTTPProxy_AuditWebSocket(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		_ = brw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(brw, buf); err == nil {
			_, _ = conn.Write(buf)
		}
	}))
	t.Cleanup(backend.Close)
	_, bport, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-dev", Namespace: "devpods",
			Annotations: map[string]string{httpauth.IDAnnotation: idA, httpauth.PublicPortsAnnotation: bport}},
		Spec:   devpodv1alpha1.DevPodSpec{Owner: "alice", Running: true},
		Status: devpodv1alpha1.DevPodStatus{Phase: devpodv1alpha1.DevPodRunning, Endpoint: "127.0.0.1:1"},
	}
	p := NewHTTPProxy(newFakeClient(t, dp), "devpods", "apps.test", "-devpod")
	fa := &fakeHTTPAudit{serve: true}
	p.Audit = fa
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s-%s-devpod.apps.test\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n", idA, bport)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	_, _ = c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q %v", buf, err)
	}
	_ = c.Close()
	if r := fa.waitRows(t, 1)[0]; r.Status != 101 || !r.Websocket || r.BytesIn < 4 || r.BytesOut < 4 || r.Path != "/ws" {
		t.Fatalf("websocket row: %+v", r)
	}
}
