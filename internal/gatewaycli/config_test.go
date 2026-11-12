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

// cfgFake plays the saved-config API (config center spec §5.1): answers
// come from a table keyed by "METHOD uri"; anything else is a 404.
type cfgFake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []string
	routes map[string]cfgAnswer
}

type cfgAnswer struct {
	status int
	body   string
}

func newCfgFake(t *testing.T) *cfgFake {
	f := &cfgFake{t: t, routes: map[string]cfgAnswer{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gwk_k3y" || r.Header.Get(UserHeader) != "alice" {
			t.Errorf("headers: Authorization=%q %s=%q", r.Header.Get("Authorization"), UserHeader, r.Header.Get(UserHeader))
		}
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.RequestURI()
		f.mu.Lock()
		f.calls = append(f.calls, strings.TrimSpace(key+" "+string(body)))
		a, ok := f.routes[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"NOT_FOUND","message":"no such config"}`)
			return
		}
		if a.status != 0 {
			w.WriteHeader(a.status)
		}
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *cfgFake) on(key string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[key] = cfgAnswer{status, body}
}

// taken returns the calls so far and forgets them.
func (f *cfgFake) taken() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// run executes one command line; stdin, when not empty, is what the
// command reads from standard input.
func (f *cfgFake) run(line, stdin string) (code int, out, errs string) {
	f.t.Helper()
	argv, err := Split(line)
	if err != nil {
		f.t.Fatal(err)
	}
	var o, e bytes.Buffer
	env := &Env{Ctx: context.Background(), User: "alice", API: NewAPI(f.srv.URL, "k3y"), Out: &o, Err: &e,
		Now: func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }}
	if stdin != "" {
		lines := strings.Split(strings.TrimSuffix(stdin, "\n"), "\n")
		env.StdinLine = func() (string, error) {
			if len(lines) == 0 {
				return "", io.EOF
			}
			l := lines[0]
			lines = lines[1:]
			return l, nil
		}
	}
	code = Run(env, argv)
	return code, o.String(), e.String()
}

const (
	cfgMine   = `{"name":"alice-sg","owner":"alice","title":"SGLang","description":"eight cards","content":"name: ${NAME}\nnode: ${NODE}\nimage: ${IMAGE:-ubuntu:24.04}\n","sharedWith":["bob","carol"],"public":false,"variables":[{"name":"NAME","builtin":true,"required":true},{"name":"NODE","builtin":true,"required":true},{"name":"IMAGE","builtin":false,"required":false,"default":"ubuntu:24.04"}],"mine":true,"updatedAt":"2026-11-12T10:00:00Z"}`
	cfgTheirs = `{"name":"bob-k3s","owner":"bob","title":"k3s","description":"","content":"name: ${NAME}\n","sharedWith":[],"public":true,"variables":[{"name":"NAME","builtin":true,"required":true}],"mine":false,"updatedAt":"2026-11-11T10:00:00Z"}`
)

func TestConfigListAndShow(t *testing.T) {
	f := newCfgFake(t)
	list := `{"items":[` + cfgMine + `,` + cfgTheirs + `]}`
	f.on("GET /api/configs", 0, list)
	f.on("GET /api/configs?scope=public", 0, `{"items":[`+cfgTheirs+`]}`)
	f.on("GET /api/configs?scope=mine", 0, `{"items":[]}`)
	f.on("GET /api/configs?scope=shared", 0, `{"items":[]}`)
	f.on("GET /api/configs/alice-sg", 0, cfgMine)
	f.on("GET /api/configs/bob-k3s", 0, cfgTheirs)

	for _, line := range []string{"config", "config list", "configs"} {
		code, out, errs := f.run(line, "")
		if code != ExitOK || errs != "" {
			t.Fatalf("%q: %d %q", line, code, errs)
		}
		rows := strings.Split(strings.TrimSpace(out), "\n")
		if len(rows) != 3 || !strings.HasPrefix(rows[0], "NAME") ||
			!reflect.DeepEqual(strings.Fields(rows[1]), []string{"alice-sg", "alice", "shared:2", "NAME,NODE,IMAGE", "SGLang"}) ||
			!reflect.DeepEqual(strings.Fields(rows[2]), []string{"bob-k3s", "bob", "public", "NAME", "k3s"}) {
			t.Fatalf("%q:\n%s", line, out)
		}
	}
	f.taken()
	for flag, want := range map[string]string{"--public": "GET /api/configs?scope=public", "--mine": "GET /api/configs?scope=mine", "--shared": "GET /api/configs?scope=shared"} {
		if code, _, errs := f.run("config list "+flag, ""); code != ExitOK {
			t.Fatalf("%s: %d %s", flag, code, errs)
		}
		if got := f.taken(); len(got) != 1 || got[0] != want {
			t.Fatalf("%s: %v", flag, got)
		}
	}
	if code, _, _ := f.run("config list --mine --public", ""); code != ExitUsage {
		t.Fatalf("two scopes: %d", code)
	}
	if code, out, _ := f.run("config list --json", ""); code != ExitOK || strings.TrimSpace(out) != list {
		t.Fatalf("json: %d %s", code, out)
	}

	// show: the caller's prefix may be left out
	f.taken()
	code, out, errs := f.run("config show sg", "")
	if code != ExitOK || errs != "" {
		t.Fatalf("show: %d %q", code, errs)
	}
	for _, want := range []string{"alice-sg", "SGLang", "eight cards", "bob, carol", "IMAGE (default ubuntu:24.04)", "NAME", "name: ${NAME}\nnode: ${NODE}\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show lacks %q:\n%s", want, out)
		}
	}
	if got := f.taken(); !reflect.DeepEqual(got, []string{"GET /api/configs/alice-sg"}) {
		t.Fatalf("calls %v", got)
	}
	// someone else's: the literal name, after the prefixed one is not there
	if code, out, _ := f.run("config show bob-k3s", ""); code != ExitOK || !strings.Contains(out, "public") {
		t.Fatalf("theirs: %d %s", code, out)
	}
	if got := f.taken(); !reflect.DeepEqual(got, []string{"GET /api/configs/alice-bob-k3s", "GET /api/configs/bob-k3s"}) {
		t.Fatalf("calls %v", got)
	}
	if code, out, _ := f.run("config show sg --json", ""); code != ExitOK || strings.TrimSpace(out) != cfgMine {
		t.Fatalf("show json: %d %s", code, out)
	}
	if code, _, errs := f.run("config show nope", ""); code != ExitAPI || !strings.Contains(errs, "NOT_FOUND") {
		t.Fatalf("missing: %d %q", code, errs)
	}
	if code, _, _ := f.run("config show", ""); code != ExitUsage {
		t.Fatalf("no name: %d", code)
	}
	if code, _, errs := f.run("config frobnicate x", ""); code != ExitUsage || !strings.Contains(errs, "frobnicate") {
		t.Fatalf("unknown subcommand: %d %q", code, errs)
	}
}

func TestConfigSave(t *testing.T) {
	f := newCfgFake(t)
	yaml := "apiVersion: devpod.io/v1alpha1\nkind: DevPod\n\nmetadata:\n  name: ${NAME}\n"
	f.on("POST /api/configs", http.StatusCreated, cfgMine)

	code, out, errs := f.run(`config save sg --title "SGLang 8" --description "eight cards" --public`, yaml)
	if code != ExitOK || errs != "" || !strings.Contains(out, "alice-sg") {
		t.Fatalf("save: %d %q %q", code, out, errs)
	}
	want := `POST /api/configs {"content":"apiVersion: devpod.io/v1alpha1\nkind: DevPod\n\nmetadata:\n  name: ${NAME}\n","description":"eight cards","id":"sg","public":true,"title":"SGLang 8"}`
	if got := f.taken(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls %v", got)
	}
	// without --title the id is the title
	f.run("config save sg", "a: b\n")
	if got := f.taken(); len(got) != 1 || got[0] != `POST /api/configs {"content":"a: b\n","id":"sg","title":"sg"}` {
		t.Fatalf("calls %v", got)
	}

	// a second save of the same id updates it: the content, and only the
	// flags given
	f.on("POST /api/configs", http.StatusConflict, `{"code":"CONFIG_EXISTS","message":"you already have a config \"sg\""}`)
	f.on("PATCH /api/configs/alice-sg", 0, cfgMine)
	code, out, errs = f.run("config save sg", "a: c\n")
	if code != ExitOK || errs != "" || !strings.Contains(out, "updated") {
		t.Fatalf("update: %d %q %q", code, out, errs)
	}
	if got := f.taken(); len(got) != 2 || got[1] != `PATCH /api/configs/alice-sg {"content":"a: c\n"}` {
		t.Fatalf("calls %v", got)
	}
	f.run("config save sg --title T2 --public", "a: c\n")
	if got := f.taken(); len(got) != 2 || got[1] != `PATCH /api/configs/alice-sg {"content":"a: c\n","public":true,"title":"T2"}` {
		t.Fatalf("calls %v", got)
	}

	// nothing on standard input: nothing is sent
	for _, stdin := range []string{"", "\n\n"} {
		if code, _, errs := f.run("config save sg", stdin); code != ExitUsage || !strings.Contains(errs, "standard input") {
			t.Fatalf("empty stdin %q: %d %q", stdin, code, errs)
		}
	}
	if code, _, _ := f.run("config save", "a: b\n"); code != ExitUsage {
		t.Fatalf("no id: %d", code)
	}
	// more than a config may hold
	if code, _, errs := f.run("config save sg", strings.Repeat("a: b\n", 53000)); code != ExitUsage || !strings.Contains(errs, "256") {
		t.Fatalf("too large: %d %q", code, errs)
	}
	if got := f.taken(); len(got) != 0 {
		t.Fatalf("something was sent: %v", got)
	}
	// the API's refusal is the answer
	f.on("POST /api/configs", http.StatusBadRequest, `{"code":"BAD_REQUEST","message":"id must be lower-case letters"}`)
	if code, _, errs := f.run("config save Bad", "a: b\n"); code != ExitAPI || !strings.Contains(errs, "lower-case") {
		t.Fatalf("refused: %d %q", code, errs)
	}
}

func TestConfigShareAndDelete(t *testing.T) {
	f := newCfgFake(t)
	f.on("GET /api/configs/alice-sg", 0, cfgMine)
	f.on("PATCH /api/configs/alice-sg", 0, cfgMine)
	f.on("DELETE /api/configs/alice-sg", http.StatusNoContent, "")

	if code, _, errs := f.run("config share sg bob carol", ""); code != ExitOK {
		t.Fatalf("share: %d %q", code, errs)
	}
	if got := f.taken(); len(got) != 2 || got[1] != `PATCH /api/configs/alice-sg {"sharedWith":["bob","carol"]}` {
		t.Fatalf("calls %v", got)
	}
	if code, out, _ := f.run("config share sg", ""); code != ExitOK || !strings.Contains(out, "nobody") {
		t.Fatalf("unshare: %d %q", code, out)
	}
	if got := f.taken(); len(got) != 2 || got[1] != `PATCH /api/configs/alice-sg {"sharedWith":[]}` {
		t.Fatalf("calls %v", got)
	}
	for word, want := range map[string]string{"on": "true", "off": "false"} {
		if code, _, errs := f.run("config public sg "+word, ""); code != ExitOK {
			t.Fatalf("public %s: %d %q", word, code, errs)
		}
		if got := f.taken(); len(got) != 2 || got[1] != `PATCH /api/configs/alice-sg {"public":`+want+`}` {
			t.Fatalf("calls %v", got)
		}
	}
	for _, line := range []string{"config public sg", "config public sg maybe", "config public", "config delete", "config share"} {
		if code, _, _ := f.run(line, ""); code != ExitUsage {
			t.Fatalf("%q: %d", line, code)
		}
	}
	if code, out, errs := f.run("config delete sg", ""); code != ExitOK || !strings.Contains(out, "alice-sg") {
		t.Fatalf("delete: %d %q %q", code, out, errs)
	}
	if got := f.taken(); len(got) != 2 || got[1] != "DELETE /api/configs/alice-sg" {
		t.Fatalf("calls %v", got)
	}
	// not the owner
	f.on("GET /api/configs/bob-k3s", 0, cfgTheirs)
	f.on("PATCH /api/configs/bob-k3s", http.StatusForbidden, `{"code":"FORBIDDEN","message":"only the owner may change a config"}`)
	if code, _, errs := f.run("config share bob-k3s carol", ""); code != ExitAPI || !strings.Contains(errs, "FORBIDDEN") {
		t.Fatalf("someone else's: %d %q", code, errs)
	}
}

func TestConfigLaunch(t *testing.T) {
	f := newCfgFake(t)
	f.on("GET /api/configs/alice-sg", 0, cfgMine)
	const launch = "POST /api/configs/alice-sg/launch"

	f.on(launch, 0, `{"items":[{"index":1,"name":"alice-sg-1","node":"a","ok":true},{"index":2,"name":"alice-sg-2","node":"b","ok":true}]}`)
	code, out, errs := f.run("config launch sg --set NAME=sg --set IMAGE=reg.test/x:1=2 --node a --node b", "")
	if code != ExitOK || errs != "" {
		t.Fatalf("launch: %d %q", code, errs)
	}
	if got := f.taken(); len(got) != 2 || got[1] != launch+` {"nodes":["a","b"],"vars":{"IMAGE":"reg.test/x:1=2","NAME":"sg"}}` {
		t.Fatalf("calls %v", got)
	}
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if len(rows) != 2 || !reflect.DeepEqual(strings.Fields(rows[0]), []string{"ok", "alice-sg-1", "a"}) || !reflect.DeepEqual(strings.Fields(rows[1]), []string{"ok", "alice-sg-2", "b"}) {
		t.Fatalf("out:\n%s", out)
	}

	// a count, no variables
	f.on(launch, 0, `{"items":[{"index":1,"name":"alice-w","ok":true}]}`)
	f.run("config launch sg --count 3", "")
	if got := f.taken(); len(got) != 2 || got[1] != launch+` {"count":3}` {
		t.Fatalf("calls %v", got)
	}

	// 207: some were created, one was not — exit status 1
	f.on(launch, http.StatusMultiStatus, `{"items":[{"index":1,"name":"alice-sg-1","node":"a","ok":true},{"index":2,"name":"alice-sg-2","node":"b","ok":false,"code":"NAME_TAKEN","message":"a DevPod named \"alice-sg-2\" already exists"}]}`)
	code, out, _ = f.run("config launch sg --set NAME=sg --node a --node b", "")
	if code != ExitAPI || !strings.Contains(out, "ok") || !strings.Contains(out, "FAIL") || !strings.Contains(out, "NAME_TAKEN: a DevPod named") {
		t.Fatalf("207: %d\n%s", code, out)
	}

	// 422: nothing was created; every item is shown, none as created
	rejected := `{"code":"LAUNCH_REJECTED","message":"nothing was created: some DevPods of this launch cannot be created","items":[{"index":1,"name":"alice-sg-1","node":"a","ok":true,"yaml":"x: 1\n"},{"index":2,"name":"alice-sg-2","node":"b","ok":false,"code":"NODE_RESERVED","message":"node b is held by carol","yaml":"x: 2\n"}]}`
	f.on(launch, http.StatusUnprocessableEntity, rejected)
	code, out, errs = f.run("config launch sg --set NAME=sg --node a --node b", "")
	if code != ExitAPI || !strings.Contains(errs, "nothing was created") || !strings.Contains(out, "NODE_RESERVED: node b is held by carol") {
		t.Fatalf("422: %d %q\n%s", code, errs, out)
	}
	for _, row := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(row, "ok") {
			t.Fatalf("a rejected launch reports a DevPod as created:\n%s", out)
		}
	}
	if code, out, _ := f.run("config launch sg --set NAME=sg --node a --node b --json", ""); code != ExitAPI || strings.TrimSpace(out) != rejected {
		t.Fatalf("422 json: %d %s", code, out)
	}

	// a dry run prints what would be created
	f.on(launch, 0, `{"items":[{"index":1,"name":"alice-d-1","node":"a","ok":true,"yaml":"name: alice-d-1\n"},{"index":2,"name":"alice-d-2","node":"b","ok":true,"yaml":"name: alice-d-2\n"}]}`)
	code, out, _ = f.run("config launch sg --set NAME=d --node a --node b --dry-run", "")
	if code != ExitOK || out != "--- alice-d-1\nname: alice-d-1\n--- alice-d-2\nname: alice-d-2\n" {
		t.Fatalf("dry run: %d\n%q", code, out)
	}
	if got := f.taken(); !strings.Contains(got[len(got)-1], `"dryRun":true`) {
		t.Fatalf("calls %v", got)
	}

	// a missing variable: the API's 400 is the answer
	f.on(launch, http.StatusBadRequest, `{"code":"BAD_VARIABLES","message":"missing variables: NAME","detail":["NAME"]}`)
	if code, _, errs := f.run("config launch sg --node a", ""); code != ExitAPI || !strings.Contains(errs, "missing variables: NAME") {
		t.Fatalf("400: %d %q", code, errs)
	}

	// command lines that send nothing
	f.taken()
	for _, line := range []string{
		"config launch",
		"config launch sg --set NAME", // no =
		"config launch sg --set =x",   // no variable
		"config launch sg --set A=1 --set A=2",
		"config launch sg --count many",
		"config launch sg --count 0",
		"config launch sg --count 2 --node a",
	} {
		if code, _, errs := f.run(line, ""); code != ExitUsage {
			t.Fatalf("%q: %d %q", line, code, errs)
		}
	}
	if got := f.taken(); len(got) != 0 {
		t.Fatalf("something was sent: %v", got)
	}
}

func TestConfigHelp(t *testing.T) {
	var b bytes.Buffer
	if !Help(&b, "config") || !strings.Contains(b.String(), "launch") || !strings.Contains(b.String(), "${NAME}") {
		t.Fatalf("help:\n%s", b.String())
	}
	b.Reset()
	Help(&b, "")
	if !strings.Contains(b.String(), "config ") {
		t.Fatalf("the list lacks config:\n%s", b.String())
	}
}
