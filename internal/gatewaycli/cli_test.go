// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSplit(t *testing.T) {
	for in, want := range map[string][]string{
		"list":                             {"list"},
		"  create dev1   --image  x:1 ":    {"create", "dev1", "--image", "x:1"},
		`set dev1 --shell "/bin/my shell"`: {"set", "dev1", "--shell", "/bin/my shell"},
		`a 'b c' "d \"e\" \\" f\ g ''`:     {"a", "b c", `d "e" \`, "f g", ""},
		`echo $HOME *.txt $(id) ; rm`:      {"echo", "$HOME", "*.txt", "$(id)", ";", "rm"}, // nothing is expanded
		"":                                 nil,
	} {
		got, err := Split(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Split(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{`a 'b`, `a "b`, `a\`} {
		if _, err := Split(in); err == nil {
			t.Errorf("Split(%q) should fail", in)
		}
	}
}

// fakeAPI is a stand-in for the console: it records every request and
// answers from a route table.
type fakeAPI struct {
	t      *testing.T
	srv    *httptest.Server
	calls  []string // "METHOD path body"
	routes map[string]any
	status map[string]int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, routes: map[string]any{}, status: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gwk_k3y" || r.Header.Get(UserHeader) != "alice" {
			t.Errorf("headers: Authorization=%q %s=%q", r.Header.Get("Authorization"), UserHeader, r.Header.Get(UserHeader))
		}
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.RequestURI()
		f.calls = append(f.calls, strings.TrimSpace(key+" "+string(body)))
		v, ok := f.routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"NOT_FOUND","message":"devpod not found"}`)
			return
		}
		if st := f.status[key]; st != 0 {
			w.WriteHeader(st)
		}
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) run(line string) (code int, out, errs string) {
	f.t.Helper()
	argv, err := Split(line)
	if err != nil {
		f.t.Fatal(err)
	}
	var o, e bytes.Buffer
	code = Run(&Env{Ctx: context.Background(), User: "alice", API: NewAPI(f.srv.URL, "k3y"), Out: &o, Err: &e,
		Now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }}, argv)
	return code, o.String(), e.String()
}

func devpod(name, phase, img string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "creationTimestamp": "2026-11-10T12:00:00Z"},
		"spec": map[string]any{"owner": "alice", "running": phase == "Running", "collaborators": []string{"bob"},
			"pod": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "dev", "image": img,
				"resources": map[string]any{"limits": map[string]any{"cpu": "4", "memory": "8Gi"}}}}}}},
		"status": map[string]any{"phase": phase},
	}
}

func TestCommands(t *testing.T) {
	f := newFakeAPI(t)
	a, b := devpod("alice-a", "Running", "ubuntu:24.04"), devpod("alice-b", "Stopped", "pytorch:2")
	f.routes["GET /api/devpods"] = map[string]any{"items": []any{b, a}, "pods": map[string]any{"alice-a": map[string]any{"node": "g206", "ip": "10.0.0.1"}}}
	f.routes["GET /api/devpods?scope=all"] = map[string]any{"items": []any{a}, "pods": map[string]any{}}
	f.routes["GET /api/devpods/alice-a"] = map[string]any{"devpod": a, "pod": map[string]any{"node": "g206", "ip": "10.0.0.1"}}
	f.routes["POST /api/devpods"] = devpod("alice-new", "Pending", "ubuntu:24.04")
	f.status["POST /api/devpods"] = http.StatusCreated
	patched := map[string]any{"devpod": a, "rebuild": true}
	f.routes["PATCH /api/devpods/alice-a"] = patched
	f.routes["DELETE /api/devpods/alice-a"] = nil
	f.status["DELETE /api/devpods/alice-a"] = http.StatusNoContent
	f.routes["POST /api/devpods/alice-a/redeploy"] = nil
	f.status["POST /api/devpods/alice-a/redeploy"] = http.StatusNoContent
	view := map[string]any{"id": "abcdefgh2345", "urlTemplate": "https://abcdefgh2345-{port}-devpod.apps.test", "publicPorts": []int{8080}}
	f.routes["GET /api/devpods/alice-a/http"] = view
	f.routes["PUT /api/devpods/alice-a/http"] = view
	f.routes["GET /api/me"] = map[string]any{"user": "alice", "admin": false, "usage": map[string]any{"devpods": 2, "running": 1, "compute": map[string]string{"cpu": "4"}},
		"ssh": map[string]any{"host": "ssh.test", "port": 2222, "loginSuffix": ""}}
	f.routes["GET /api/admin/nodes"] = map[string]any{"items": []any{map[string]any{"name": "g206", "ready": true, "cpu": map[string]any{"allocatable": 64, "allocated": 4},
		"mem": map[string]any{"allocatable": 256 << 30, "allocated": 8 << 30}, "gpu": map[string]any{"model": "A100", "total": 8, "used": 1}, "devpods": 1}}}
	f.routes["GET /api/admin/users"] = map[string]any{"items": []any{map[string]any{"name": "alice", "admin": false, "devpods": 2, "running": 1, "source": "password", "pubkeys": 1}}}
	last := func() string { return f.calls[len(f.calls)-1] }

	// list: a sorted table; --json is the API's answer untouched
	code, out, _ := f.run("list")
	want := "NAME     OWNER  PHASE    NODE  IMAGE         CPU  MEMORY  GPU  AGE\n" +
		"alice-a  alice  Running  g206  ubuntu:24.04  4    8Gi     -    24h\n" +
		"alice-b  alice  Stopped  -     pytorch:2     4    8Gi     -    24h\n"
	if code != 0 || out != want {
		t.Fatalf("list: %d\n%s\nwant\n%s", code, out, want)
	}
	code, out, _ = f.run("ls -o json")
	var js map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &js) != nil || len(js["items"].([]any)) != 2 {
		t.Fatalf("list json: %d %s", code, out)
	}
	if f.run("list --all"); last() != "GET /api/devpods?scope=all" {
		t.Fatalf("list --all asked %s", last())
	}

	if code, out, _ = f.run("show alice-a"); code != 0 || !strings.Contains(out, "phase:         Running") || !strings.Contains(out, "node:          g206") || !strings.Contains(out, "collaborators: bob") {
		t.Fatalf("show: %d %s", code, out)
	}
	if code, out, _ = f.run("whoami"); code != 0 || !strings.Contains(out, "user:     alice") || !strings.Contains(out, "devpods:  2 (1 running)") {
		t.Fatalf("whoami: %d %s", code, out)
	}

	if code, out, _ = f.run("create new --image ubuntu:24.04 --cpu 4 --memory=8Gi --collab bob,carol"); code != 0 || out != "created alice-new\n" {
		t.Fatalf("create: %d %s", code, out)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(strings.TrimPrefix(last(), "POST /api/devpods ")), &sent)
	if sent["name"] != "new" || sent["image"] != "ubuntu:24.04" || sent["cpu"] != "4" || sent["memory"] != "8Gi" || len(sent["collaborators"].([]any)) != 2 {
		t.Fatalf("create sent %s", last())
	}

	// several names: each is reported, a failure does not stop the rest
	code, out, errs := f.run("stop alice-a nope")
	if code != ExitAPI || out != "alice-a: stopped\n" || errs != "error: nope: devpod not found (NOT_FOUND)\n" {
		t.Fatalf("stop: %d %q %q", code, out, errs)
	}
	if f.calls[len(f.calls)-2] != `PATCH /api/devpods/alice-a {"running":false}` {
		t.Fatalf("stop sent %v", f.calls[len(f.calls)-2:])
	}
	code, out, _ = f.run("start alice-a nope --json")
	if code != ExitAPI || strings.TrimSpace(out) != `[{"name":"alice-a","ok":true},{"name":"nope","ok":false,"code":"NOT_FOUND","error":"devpod not found"}]` {
		t.Fatalf("start json: %d %s", code, out)
	}
	if code, out, _ = f.run("redeploy alice-a"); code != 0 || out != "alice-a: redeploying\n" || last() != "POST /api/devpods/alice-a/redeploy" {
		t.Fatalf("redeploy: %d %s %s", code, out, last())
	}

	// delete insists on --yes
	n := len(f.calls)
	if code, _, errs = f.run("delete alice-a"); code != ExitUsage || !strings.Contains(errs, "--yes") || len(f.calls) != n {
		t.Fatalf("delete without --yes: %d %q", code, errs)
	}
	if code, out, _ = f.run("rm alice-a -y"); code != 0 || out != "alice-a: deleted\n" || last() != "DELETE /api/devpods/alice-a" {
		t.Fatalf("delete: %d %s", code, out)
	}

	if code, out, _ = f.run("set alice-a --image ubuntu:25.04 --cpu 8"); code != 0 || !strings.Contains(out, "alice-a: updated (the Pod is being rebuilt") {
		t.Fatalf("set: %d %s", code, out)
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(last(), "PATCH /api/devpods/alice-a ")), &sent)
	if sent["image"] != "ubuntu:25.04" || sent["cpu"] != "8" {
		t.Fatalf("set sent %s", last())
	}
	if code, _, _ = f.run("set alice-a"); code != ExitUsage {
		t.Fatalf("set without changes: %d", code)
	}

	if f.run("collab alice-a add carol bob"); last() != `PATCH /api/devpods/alice-a {"collaborators":["bob","carol"]}` {
		t.Fatalf("collab add sent %s", last())
	}
	if f.run("collab alice-a remove bob"); last() != `PATCH /api/devpods/alice-a {"collaborators":[]}` {
		t.Fatalf("collab remove sent %s", last())
	}
	if code, out, _ = f.run("collab alice-a"); code != 0 || out != "alice-a: bob\n" {
		t.Fatalf("collab: %d %q", code, out)
	}

	if code, out, _ = f.run("ports alice-a"); code != 0 || out != "address: https://abcdefgh2345-{port}-devpod.apps.test\npublic:  8080 (anyone with the link)\nusers:   none (anyone signed in to the console)\nother ports: owner, collaborators and admins, after a console sign-in\n" {
		t.Fatalf("ports: %d %q", code, out)
	}
	if code, out, _ = f.run("ports alice-a url 8888"); code != 0 || out != "https://abcdefgh2345-8888-devpod.apps.test\n" {
		t.Fatalf("ports url: %d %q", code, out)
	}
	if f.run("ports alice-a public 8888"); last() != `PUT /api/devpods/alice-a/http {"publicPorts":[8080,8888],"userPorts":[]}` {
		t.Fatalf("ports public sent %s", last())
	}
	// one level per port: opening 8080 to signed-in users takes it off the public list
	if f.run("ports alice-a users 8080 9000"); last() != `PUT /api/devpods/alice-a/http {"publicPorts":[],"userPorts":[8080,9000]}` {
		t.Fatalf("users: %s", last())
	}
	if f.run("ports alice-a private 8080"); last() != `PUT /api/devpods/alice-a/http {"publicPorts":[],"userPorts":[]}` {
		t.Fatalf("ports private sent %s", last())
	}
	if f.run("ports alice-a reset"); last() != `PUT /api/devpods/alice-a/http {"reset":true}` {
		t.Fatalf("ports reset sent %s", last())
	}
	if code, _, _ = f.run("ports alice-a public http"); code != ExitUsage {
		t.Fatalf("bad port: %d", code)
	}

	if code, out, _ = f.run("ssh alice-a"); code != 0 || out != "ssh -p 2222 alice+a@ssh.test\n" {
		t.Fatalf("ssh: %d %q", code, out)
	}
	if f.run("migrate alice-a g206"); last() != `PATCH /api/devpods/alice-a {"node":"g206"}` {
		t.Fatalf("migrate sent %s", last())
	}
	if f.run("migrate alice-a -"); last() != `PATCH /api/devpods/alice-a {"node":""}` {
		t.Fatalf("unpin sent %s", last())
	}
	if code, out, _ = f.run("nodes"); code != 0 || !strings.Contains(out, "g206  true   4/64") || !strings.Contains(out, "8Gi/256Gi") || !strings.Contains(out, "1/8 A100") {
		t.Fatalf("nodes: %d %s", code, out)
	}
	if code, out, _ = f.run("users"); code != 0 || !strings.Contains(out, "alice  false  2        1        password  1") {
		t.Fatalf("users: %d %s", code, out)
	}
	// templates are gone
	if code, _, _ = f.run("templates"); code != ExitUsage {
		t.Fatalf("templates: %d", code)
	}

	// usage errors never reach the API
	n = len(f.calls)
	for _, line := range []string{"frobnicate", "show", "list --nope", "create x", "create", "ports", "migrate alice-a", "show a b", "list -o yaml"} {
		if code, _, errs = f.run(line); code != ExitUsage || !strings.HasPrefix(errs, "error: ") {
			t.Errorf("%q: exit %d, stderr %q", line, code, errs)
		}
	}
	if len(f.calls) != n {
		t.Fatalf("usage errors called the API: %v", f.calls[n:])
	}
	// help is generated from the command table
	if code, out, _ = f.run("help"); code != 0 || !strings.Contains(out, "migrate <name> <node|->") || !strings.Contains(out, "(admin)") || !strings.Contains(out, "--json") {
		t.Fatalf("help: %d %s", code, out)
	}
	if code, out, _ = f.run("create --help"); code != 0 || !strings.HasPrefix(out, "usage: create <name> --image IMG") {
		t.Fatalf("create --help: %d %s", code, out)
	}
	if code, out, _ = f.run(""); code != 0 || !strings.Contains(out, "DevPod CLI") {
		t.Fatalf("empty: %d %s", code, out)
	}
}

func TestAPIErrors(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["GET /api/devpods"] = map[string]any{"code": "QUOTA_EXCEEDED", "message": "over quota", "detail": "cpu 8 > 4"}
	f.status["GET /api/devpods"] = http.StatusConflict
	if code, _, errs := f.run("list"); code != ExitAPI || errs != "error: over quota: cpu 8 > 4 (QUOTA_EXCEEDED)\n" {
		t.Fatalf("api error: %d %q", code, errs)
	}
	// an unreachable API and an oversized answer are reported, not hung on
	var o, e bytes.Buffer
	dead := NewAPI("http://127.0.0.1:1", "k3y")
	if code := Run(&Env{Ctx: context.Background(), User: "alice", API: dead, Out: &o, Err: &e}, []string{"list"}); code != ExitAPI || !strings.Contains(e.String(), "(UNAVAILABLE)") {
		t.Fatalf("unreachable: %d %q", code, e.String())
	}
	f.status["GET /api/devpods"] = 0
	api := NewAPI(f.srv.URL, "k3y")
	api.MaxBody = 8
	e.Reset()
	if code := Run(&Env{Ctx: context.Background(), User: "alice", API: api, Out: &o, Err: &e}, []string{"list"}); code != ExitAPI || !strings.Contains(e.String(), "(TOO_LARGE)") {
		t.Fatalf("too large: %d %q", code, e.String())
	}
}

// sshRig serves the CLI on a real SSH connection, the way the gateway
// does after it authenticated the caller.
func sshRig(t *testing.T, srv *Server) *ssh.Client {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				srv.Serve(context.Background(), conn, chans, reqs, Meta{User: "alice", Remote: c.RemoteAddr().String(), AuthKind: "direct"})
				_ = conn.Close()
			}()
		}
	}()
	cl, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "devpod", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*ssh.ExitError); ok {
		return ee.ExitStatus()
	}
	return -1
}

func TestServeOverSSH(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["GET /api/devpods"] = map[string]any{"items": []any{devpod("alice-a", "Running", "ubuntu:24.04")}, "pods": map[string]any{}}
	cl := sshRig(t, &Server{API: NewAPI(f.srv.URL, "k3y")})
	exec := func(cmd string) (int, string, string) {
		t.Helper()
		s, err := cl.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		var o, e bytes.Buffer
		s.Stdout, s.Stderr = &o, &e
		return exitCode(s.Run(cmd)), o.String(), e.String()
	}
	if code, out, _ := exec("list"); code != 0 || !strings.Contains(out, "alice-a  alice  Running") {
		t.Fatalf("exec list: %d %q", code, out)
	}
	if code, _, errs := exec("show nope"); code != ExitAPI || errs != "error: devpod not found (NOT_FOUND)\n" {
		t.Fatalf("exec show: %d %q", code, errs)
	}
	if code, _, errs := exec("frobnicate"); code != ExitUsage || !strings.Contains(errs, "no such command") {
		t.Fatalf("exec unknown: %d %q", code, errs)
	}
	if code, _, errs := exec(`show 'unterminated`); code != ExitUsage || !strings.Contains(errs, "unterminated") {
		t.Fatalf("exec bad quoting: %d %q", code, errs)
	}

	// a shell without a terminal prints the help
	s, _ := cl.NewSession()
	var o bytes.Buffer
	s.Stdout = &o
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(s.Wait()); code != 0 || !strings.Contains(o.String(), "DevPod CLI") {
		t.Fatalf("shell: %d %q", code, o.String())
	}

	// with a terminal: a prompt that runs commands until exit
	s, _ = cl.NewSession()
	in, _ := s.StdinPipe()
	o.Reset()
	s.Stdout = &o
	if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(in, "list\rexit\r")
	if code := exitCode(s.Wait()); code != 0 || !strings.Contains(o.String(), "signed in as alice") || !strings.Contains(o.String(), "alice-a  alice  Running") || !strings.Contains(o.String(), "devpod> ") {
		t.Fatalf("interactive: %d %q", code, o.String())
	}

	// nothing but sessions: no forwarding, no sftp
	if _, err := cl.Dial("tcp", "10.0.0.1:22"); err == nil || !strings.Contains(err.Error(), "only runs commands") {
		t.Fatalf("direct-tcpip: %v", err)
	}
	if _, err := cl.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Fatal("tcpip-forward was accepted")
	}
	s, _ = cl.NewSession()
	if err := s.RequestSubsystem("sftp"); err == nil {
		t.Fatal("the sftp subsystem was accepted")
	}
	_ = s.Close()
}

func TestServeLimits(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["GET /api/devpods"] = map[string]any{"items": []any{}, "pods": map[string]any{}}
	srv := &Server{API: NewAPI(f.srv.URL, "k3y"), MaxConns: 1, IdleTimeout: 200 * time.Millisecond}
	first := sshRig(t, srv)
	s, _ := first.NewSession()
	if err := s.Run("list"); err != nil {
		t.Fatalf("first: %v", err)
	}
	// a second connection over the cap is told so, and gets exit 1
	second := sshRig(t, srv)
	s2, _ := second.NewSession()
	var e bytes.Buffer
	s2.Stderr = &e
	if code := exitCode(s2.Run("list")); code != ExitAPI || !strings.Contains(e.String(), "too many DevPod CLI sessions") {
		t.Fatalf("over the cap: %d %q", code, e.String())
	}
	// an idle connection is closed
	done := make(chan error, 1)
	go func() { done <- first.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle connection was not closed")
	}
}
