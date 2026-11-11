// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// regFake plays the registry-credential API (snapshot v2 spec §5.3).
type regFake struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
	list  string // body of GET /api/me/registries
}

func newRegFake(t *testing.T) *regFake {
	f := &regFake{t: t, list: `{"items":[{"host":"harbor.test","username":"robot","pending":false},{"host":"reg.test:5000","username":"me","pending":true}]}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gwk_k3y" || r.Header.Get(UserHeader) != "alice" {
			t.Errorf("headers: Authorization=%q %s=%q", r.Header.Get("Authorization"), UserHeader, r.Header.Get(UserHeader))
		}
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.RequestURI()
		f.mu.Lock()
		f.calls = append(f.calls, strings.TrimSpace(key+" "+string(body)))
		list := f.list
		f.mu.Unlock()
		switch {
		case key == "GET /api/me/registries":
			_, _ = io.WriteString(w, list)
		case key == "PUT /api/me/registries/full.test":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":"BAD_REQUEST","message":"at most 20 registries"}`)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/me/registries/"):
			w.WriteHeader(http.StatusNoContent)
		case key == "DELETE /api/me/registries/gone.test":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"NOT_FOUND","message":"no such registry"}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/me/registries/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"NOT_FOUND","message":"not found"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *regFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *regFake) run(line string, mod func(*Env)) (code int, out, errs string) {
	f.t.Helper()
	argv, err := Split(line)
	if err != nil {
		f.t.Fatal(err)
	}
	var o, e bytes.Buffer
	env := &Env{Ctx: context.Background(), User: "alice", API: NewAPI(f.srv.URL, "k3y"), Out: &o, Err: &e,
		Now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }}
	if mod != nil {
		mod(env)
	}
	code = Run(env, argv)
	return code, o.String(), e.String()
}

func TestRegistryList(t *testing.T) {
	f := newRegFake(t)
	for _, line := range []string{"registry list", "registry", "registries", "registry ls"} {
		code, out, errs := f.run(line, nil)
		want := "HOST           USERNAME  STATUS\nharbor.test    robot     ready\nreg.test:5000  me        pending\n"
		if code != ExitOK || out != want {
			t.Fatalf("%s: %d %q %q", line, code, out, errs)
		}
	}
	if got := f.seen(); len(got) != 4 || got[0] != "GET /api/me/registries" {
		t.Fatalf("requests %v", got)
	}
	// --json passes the API body through
	if code, out, _ := f.run("registry list --json", nil); code != ExitOK || strings.TrimSpace(out) != f.list {
		t.Fatalf("json: %d %q", code, out)
	}
	f.list = `{"items":[]}`
	if code, out, _ := f.run("registry list", nil); code != ExitOK || !strings.Contains(out, "registry add <host>") {
		t.Fatalf("empty: %d %q", code, out)
	}
}

func TestRegistryAddAndRemove(t *testing.T) {
	const secret = "s3cr3t-pw"
	type ask struct {
		prompt string
		echo   bool
	}
	prompts := func(asked *[]ask, answers ...string) func(*Env) {
		return func(e *Env) {
			e.TTY = true
			e.ReadLine = func(p string, echo bool) (string, error) {
				*asked = append(*asked, ask{p, echo})
				a := answers[0]
				answers = answers[1:]
				return a, nil
			}
		}
	}
	wantPut := `PUT /api/me/registries/harbor.test {"username":"bob","password":"` + secret + `"}`

	t.Run("terminal: user name echoed, password not; the pasted address is tidied", func(t *testing.T) {
		f := newRegFake(t)
		var asked []ask
		code, out, errs := f.run("registry add https://Harbor.Test/harbor/projects", prompts(&asked, "bob", secret))
		if code != ExitOK || len(asked) != 2 || !asked[0].echo || asked[1].echo {
			t.Fatalf("prompts: %d %+v %q", code, asked, errs)
		}
		if got := f.seen(); !reflect.DeepEqual(got, []string{wantPut}) {
			t.Fatalf("request %v", got)
		}
		if !strings.Contains(out, "saved the login for harbor.test (user bob)") || strings.Contains(out+errs, secret) {
			t.Fatalf("output %q %q", out, errs)
		}
	})
	t.Run("terminal with --username: only the password is asked", func(t *testing.T) {
		f := newRegFake(t)
		var asked []ask
		code, _, _ := f.run("registry add harbor.test --username bob", prompts(&asked, secret))
		if code != ExitOK || len(asked) != 1 || asked[0].echo || !reflect.DeepEqual(f.seen(), []string{wantPut}) {
			t.Fatalf("%d %+v %v", code, asked, f.seen())
		}
	})
	t.Run("no terminal: the first line of standard input; --password-stdin forces it", func(t *testing.T) {
		f := newRegFake(t)
		stdin := func(e *Env) { e.StdinLine = func() (string, error) { return secret, nil } }
		code, out, errs := f.run("registry add harbor.test --username bob --json", stdin)
		if code != ExitOK || !reflect.DeepEqual(f.seen(), []string{wantPut}) || strings.Contains(out+errs, secret) {
			t.Fatalf("stdin: %d %v %q %q", code, f.seen(), out, errs)
		}
		// the API answered without a body: --json still prints an object, without the password
		if strings.TrimSpace(out) != `{"host":"harbor.test","saved":true,"username":"bob"}` {
			t.Fatalf("json: %q", out)
		}
		f = newRegFake(t)
		var asked []ask
		code, _, _ = f.run("registry add harbor.test --username bob --password-stdin", func(e *Env) {
			prompts(&asked)(e)
			stdin(e)
		})
		if code != ExitOK || len(asked) != 0 || !reflect.DeepEqual(f.seen(), []string{wantPut}) {
			t.Fatalf("--password-stdin: %d %+v %v", code, asked, f.seen())
		}
	})
	t.Run("usage errors send nothing", func(t *testing.T) {
		for _, line := range []string{
			"registry add",
			"registry add a.test b.test",
			"registry add harbor.test",                    // no terminal, no --username
			"registry add harbor.test --password-stdin",   // stdin needs --username
			"registry add harbor.test --username bob",     // no terminal and nothing on stdin
			"registry add harbor.test --password hunter2", // there is no --password
			"registry rm",
			"registry rm harbor.test --username bob",
			"registry list extra",
			"registry frobnicate harbor.test",
		} {
			f := newRegFake(t)
			code, _, errs := f.run(line, nil)
			if code != ExitUsage || len(f.seen()) != 0 || !strings.Contains(errs, "usage:") {
				t.Fatalf("%s: %d %q %v", line, code, errs, f.seen())
			}
		}
	})
	t.Run("API errors", func(t *testing.T) {
		f := newRegFake(t)
		code, _, errs := f.run("registry add full.test --username bob", func(e *Env) { e.StdinLine = func() (string, error) { return secret, nil } })
		if code != ExitAPI || !strings.Contains(errs, "at most 20 registries") || strings.Contains(errs, secret) {
			t.Fatalf("add: %d %q", code, errs)
		}
		code, _, errs = f.run("registry rm gone.test", nil)
		if code != ExitAPI || !strings.Contains(errs, "NOT_FOUND") {
			t.Fatalf("rm: %d %q", code, errs)
		}
	})
	t.Run("rm", func(t *testing.T) {
		f := newRegFake(t)
		code, out, _ := f.run("registry rm Reg.Test:5000", nil)
		if code != ExitOK || !reflect.DeepEqual(f.seen(), []string{"DELETE /api/me/registries/reg.test:5000"}) || !strings.Contains(out, "removed the login for reg.test:5000") {
			t.Fatalf("rm: %d %q %v", code, out, f.seen())
		}
		if code, out, _ = f.run("registry rm harbor.test --json", nil); code != ExitOK || strings.TrimSpace(out) != `{"host":"harbor.test","removed":true}` {
			t.Fatalf("rm json: %d %q", code, out)
		}
	})
	t.Run("help is generated from the table", func(t *testing.T) {
		f := newRegFake(t)
		code, out, _ := f.run("help registry", nil)
		if code != ExitOK || !strings.Contains(out, "registry add <host>") || !strings.Contains(out, "never goes on the command line") {
			t.Fatalf("help: %d %q", code, out)
		}
		if _, out, _ = f.run("help", nil); !strings.Contains(out, "registry") {
			t.Fatalf("help list: %q", out)
		}
	})
}

func TestImageRegistry(t *testing.T) {
	for in, want := range map[string]string{
		"Harbor.Test/team/app:v1": "harbor.test",
		"reg:5000/app:v1":         "reg:5000",
		"localhost/app:v1":        "localhost",
		"team/app:v1":             "docker.io",
		"ubuntu:24.04":            "docker.io",
	} {
		if got := imageRegistry(in); got != want {
			t.Errorf("imageRegistry(%q) = %q, want %q", in, got, want)
		}
	}
	if got := registryHost(" HTTPS://Reg.Test:5000/v2/ "); got != "reg.test:5000" {
		t.Errorf("registryHost = %q", got)
	}
}

// When a snapshot's address needs credentials, the hint names the way to
// save a login for that registry.
func TestSnapshotPushAuthHintNamesRegistryAdd(t *testing.T) {
	f := newSnapFake(t)
	code, _, errs := f.run("snapshot alice-a --image reg.test/me/env:v1 --detach", nil)
	if code != ExitAPI || !strings.Contains(errs, "registry add reg.test") || !strings.Contains(errs, "--username") || !strings.Contains(errs, "PUSH_AUTH_REQUIRED") {
		t.Fatalf("no tty: %d %q", code, errs)
	}
	f = newSnapFake(t)
	code, _, errs = f.run("snapshot alice-a --image reg.test/me/env:v1 --detach", func(e *Env) {
		e.TTY = true
		answers := []string{"bob", "pw"}
		e.ReadLine = func(string, bool) (string, error) {
			a := answers[0]
			answers = answers[1:]
			return a, nil
		}
	})
	if code != ExitOK || !strings.Contains(errs, "registry add reg.test") {
		t.Fatalf("tty: %d %q", code, errs)
	}
}
