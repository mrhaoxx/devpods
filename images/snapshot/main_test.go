// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* ---- protocol ---- */

func protoLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.HasPrefix(l, "@") {
			out = append(out, l)
		}
	}
	return out
}

func TestEmitter_PushIsThrottledAndEndsWithTheLastValue(t *testing.T) {
	var buf bytes.Buffer
	e := newEmitter(&buf, "")
	now := time.Unix(1000, 0)
	e.now = func() time.Time { return now }

	e.Push(10, 100) // first one goes out
	e.Push(20, 100) // same second: held back
	e.Push(30, 100)
	now = now.Add(1100 * time.Millisecond)
	e.Push(40, 100) // a second later: out
	e.Push(40, 100) // unchanged: nothing
	now = now.Add(100 * time.Millisecond)
	e.Push(100, 100) // held back …
	e.PushEnd()      // … until the end
	e.PushEnd()      // and never twice
	want := []string{"@push 10 100", "@push 40 100", "@push 100 100"}
	if got := protoLines(buf.String()); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEmitter_LinesAndResult(t *testing.T) {
	var buf bytes.Buffer
	term := filepath.Join(t.TempDir(), "term")
	e := newEmitter(&buf, term)
	e.Stage("commit")
	e.Size(123)
	e.Log("@not a protocol line\nsecond line")
	e.Error("push failed:\n denied")
	want := "@stage commit\n@size 123\nnot a protocol line second line\n@error push failed: denied\n"
	if buf.String() != want {
		t.Fatalf("got %q", buf.String())
	}
	read := func() string { b, _ := os.ReadFile(term); return string(b) }
	e.Result("sha256:abc", 123, "")
	if read() != "digest=sha256:abc size=123\n" {
		t.Fatalf("result %q", read())
	}
	e.Result("", 42, "")
	if read() != "size=42\n" {
		t.Fatalf("result %q", read())
	}
	e.Result("", 0, "push failed:\n denied")
	if read() != "error=push failed: denied\n" {
		t.Fatalf("result %q", read())
	}
}

func TestLayerProgress(t *testing.T) {
	p := newLayerProgress()
	p.Set("a", 10, 100)
	p.Set("b", 0, 50)
	p.Set("a", 60, 0)   // total remembered
	p.Set("a", 30, 100) // never backwards
	p.Set("c", 5, 0)    // no size known: counts as nothing
	if pushed, total := p.Sum(); pushed != 60 || total != 150 {
		t.Fatalf("sum %d/%d", pushed, total)
	}
	p.Set("b", 999, 50) // clamped
	p.Finish("a")
	p.Finish("c")
	if pushed, total := p.Sum(); pushed != 150 || total != 150 {
		t.Fatalf("finished sum %d/%d", pushed, total)
	}
}

func TestReport_FoldsTrackerStatus(t *testing.T) {
	lp := newLayerProgress()
	jobs := map[string]int64{"layer-1": 1000, "layer-2": 500, "config": 10}
	seen := map[string][2]int64{"layer-1": {400, 1000}, "config": {10, 0}} // total 0: fall back to the blob's size
	status := func(ref string) (int64, int64, bool) {
		s, ok := seen[ref]
		return s[0], s[1], ok
	}
	if pushed, total := report(jobs, status, lp); pushed != 410 || total != 1510 {
		t.Fatalf("report %d/%d", pushed, total)
	}
	seen["layer-2"] = [2]int64{500, 500}
	seen["layer-1"] = [2]int64{1000, 1000}
	if pushed, total := report(jobs, status, lp); pushed != 1510 || total != 1510 {
		t.Fatalf("report %d/%d", pushed, total)
	}
}

/* ---- the run loop ---- */

type fakeBackend struct {
	calls     []string
	commitErr error
	pushErr   error
	size      int64
	digest    string
	onPush    func()
}

func (f *fakeBackend) Commit(context.Context) error {
	f.calls = append(f.calls, "commit")
	return f.commitErr
}
func (f *fakeBackend) Size(context.Context) (int64, error) {
	return f.size, nil
}
func (f *fakeBackend) Push(_ context.Context, progress func(int64, int64)) (string, error) {
	f.calls = append(f.calls, "push")
	progress(1, 2)
	if f.onPush != nil {
		f.onPush()
	}
	if f.pushErr != nil {
		return "", f.pushErr
	}
	progress(2, 2)
	return f.digest, nil
}
func (f *fakeBackend) Rollback(context.Context) error {
	f.calls = append(f.calls, "rollback")
	return nil
}
func (f *fakeBackend) Release(context.Context) error {
	f.calls = append(f.calls, "release")
	return nil
}
func (f *fakeBackend) Close() {}

func runFake(t *testing.T, ctx context.Context, o options, b *fakeBackend) (int, []string, string) {
	t.Helper()
	var buf bytes.Buffer
	term := filepath.Join(t.TempDir(), "term")
	code := run(ctx, o, b, newEmitter(&buf, term))
	res, _ := os.ReadFile(term)
	return code, protoLines(buf.String()), strings.TrimSpace(string(res))
}

func TestRun(t *testing.T) {
	t.Run("commit and push", func(t *testing.T) {
		b := &fakeBackend{size: 1234, digest: "sha256:feed"}
		code, lines, res := runFake(t, context.Background(), options{push: true}, b)
		want := "@stage commit|@size 1234|@stage push|@push 1 2|@push 2 2|@digest sha256:feed|@done"
		if code != 0 || strings.Join(lines, "|") != want || res != "digest=sha256:feed size=1234" {
			t.Fatalf("code %d lines %v result %q", code, lines, res)
		}
		if strings.Join(b.calls, ",") != "commit,push,release" {
			t.Fatalf("calls %v", b.calls)
		}
	})
	t.Run("commit only keeps the image", func(t *testing.T) {
		b := &fakeBackend{size: 7}
		code, lines, res := runFake(t, context.Background(), options{push: false}, b)
		if code != 0 || strings.Join(lines, "|") != "@stage commit|@size 7|@done" || res != "size=7" || strings.Join(b.calls, ",") != "commit" {
			t.Fatalf("code %d lines %v result %q calls %v", code, lines, res, b.calls)
		}
	})
	t.Run("a failed push rolls back and says why", func(t *testing.T) {
		b := &fakeBackend{size: 7, pushErr: errors.New("denied: requested access to the resource is denied")}
		code, lines, res := runFake(t, context.Background(), options{push: true}, b)
		if code != 1 || lines[len(lines)-1] != "@error push failed: denied: requested access to the resource is denied" ||
			res != "error=push failed: denied: requested access to the resource is denied" || strings.Join(b.calls, ",") != "commit,push,rollback" {
			t.Fatalf("code %d lines %v result %q calls %v", code, lines, res, b.calls)
		}
		for _, l := range lines {
			if l == "@done" {
				t.Fatal("@done after a failure")
			}
		}
	})
	t.Run("a failed commit never pushes", func(t *testing.T) {
		b := &fakeBackend{commitErr: errors.New("no such container")}
		code, lines, _ := runFake(t, context.Background(), options{push: true}, b)
		if code != 1 || lines[len(lines)-1] != "@error commit failed: no such container" || strings.Join(b.calls, ",") != "commit,rollback" {
			t.Fatalf("code %d lines %v calls %v", code, lines, b.calls)
		}
	})
	t.Run("a cancel during the push rolls back as cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		b := &fakeBackend{size: 7, pushErr: context.Canceled, onPush: cancel}
		code, lines, res := runFake(t, ctx, options{push: true}, b)
		if code != 143 || lines[len(lines)-1] != "@error cancelled" || res != "error=cancelled" || strings.Join(b.calls, ",") != "commit,push,rollback" {
			t.Fatalf("code %d lines %v result %q calls %v", code, lines, res, b.calls)
		}
	})
}

func TestParseFlags(t *testing.T) {
	t.Setenv("RUNTIME", "containerd")
	t.Setenv("CONTAINER_ID", "abc")
	t.Setenv("TARGET_IMAGE", "reg/x:1")
	t.Setenv("PAUSE", "false")
	t.Setenv("TERMINATION_LOG", "/tmp/t")
	o, err := parseFlags(nil)
	if err != nil || o.socket != "/run/containerd/containerd.sock" || o.namespace != "k8s.io" || o.pause || !o.push || o.termPath != "/tmp/t" {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = parseFlags([]string{"--runtime=docker", "--push=false"})
	if err != nil || o.socket != "/var/run/docker.sock" || o.push {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := parseFlags([]string{"--runtime=podman"}); err == nil {
		t.Fatal("unknown runtime accepted")
	}
	t.Setenv("CONTAINER_ID", "")
	if _, err := parseFlags(nil); err == nil {
		t.Fatal("missing container accepted")
	}
}

/* ---- references and credentials ---- */

func TestRefsAndAuth(t *testing.T) {
	for in, want := range map[string][2]string{
		"reg.test:5000/u/img:v1": {"reg.test:5000/u/img", "v1"},
		"reg.test:5000/u/img":    {"reg.test:5000/u/img", "latest"},
		"ubuntu":                 {"ubuntu", "latest"},
	} {
		repo, tag, err := splitRef(in)
		if err != nil || repo != want[0] || tag != want[1] {
			t.Errorf("splitRef(%q) = %q %q %v", in, repo, tag, err)
		}
	}
	if _, _, err := splitRef("reg/x@sha256:abc"); err == nil {
		t.Error("digest reference accepted")
	}
	for in, want := range map[string]string{"reg.test:5000/u/img:v1": "reg.test:5000", "harbor.x.io/a/b": "harbor.x.io", "ubuntu:24.04": "docker.io", "library/ubuntu": "docker.io", "localhost/x": "localhost"} {
		if got := registryOf(in); got != want {
			t.Errorf("registryOf(%q) = %q", in, got)
		}
	}

	dir := t.TempDir()
	cfg := map[string]any{"auths": map[string]any{
		"https://harbor.x.io/v2/":     map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("robot$devpod:s3cr3t:with:colons"))},
		"https://index.docker.io/v1/": map[string]string{"username": "hubuser", "password": "hubpass"},
	}}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := loadRegistryAuth(dir, "harbor.x.io/devpod-users/alice/img:v1")
	if err != nil || a.Username != "robot$devpod" || a.Password != "s3cr3t:with:colons" || a.ServerAddress != "harbor.x.io" {
		t.Fatalf("harbor auth %+v %v", a, err)
	}
	if a, _ := loadRegistryAuth(dir, "ubuntu:24.04"); a.Username != "hubuser" {
		t.Fatalf("hub auth %+v", a)
	}
	if a, err := loadRegistryAuth(dir, "other.io/x:1"); err != nil || a.Username != "" {
		t.Fatalf("no entry must mean anonymous: %+v %v", a, err)
	}
	if a, err := loadRegistryAuth("", "harbor.x.io/x:1"); err != nil || a.Username != "" {
		t.Fatalf("no config dir: %+v %v", a, err)
	}
	// the header is base64url JSON the daemon can read back
	raw, err := base64.URLEncoding.DecodeString(a.header())
	if err != nil || !json.Valid(raw) {
		t.Fatalf("header %q %v", a.header(), err)
	}
}

/* ---- the Docker Engine API ---- */

// fakeDocker is a daemon on a unix socket. Images are a name → ID map.
type fakeDocker struct {
	t      *testing.T
	mu     sync.Mutex
	images map[string]string
	calls  []string
	auth   string
	// pushStream is what POST /images/…/push answers, line by line;
	// pushBlock, if set, is closed-over by a handler that waits on it
	// after the first line (to cancel mid-push).
	pushStream []string
	pushHold   chan struct{}
	pushing    chan struct{}
	socket     string
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir, err := os.MkdirTemp("", "dk") // short path: unix socket names are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeDocker{t: t, images: map[string]string{}, socket: filepath.Join(dir, "d.sock"), pushing: make(chan struct{}, 1)}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeDocker) log(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeDocker) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (f *fakeDocker) image(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[name]
}

func (f *fakeDocker) serve(w http.ResponseWriter, r *http.Request) {
	p, ok := strings.CutPrefix(r.URL.Path, "/"+dockerAPIVersion)
	if !ok {
		http.Error(w, `{"message":"wrong API version"}`, http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"No such image"}`)
	}
	switch {
	case r.Method == "POST" && p == "/commit":
		name := q.Get("repo") + ":" + q.Get("tag")
		f.log("commit %s -> %s pause=%s", q.Get("container"), name, q.Get("pause"))
		if q.Get("container") == "missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such container: missing"}`)
			return
		}
		f.mu.Lock()
		f.images[name] = "sha256:new"
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"Id":"sha256:new"}`)
	case r.Method == "GET" && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
		id := f.image(name)
		if id == "" {
			notFound()
			return
		}
		_, _ = fmt.Fprintf(w, `{"Id":%q,"Size":4242}`, id)
	case r.Method == "POST" && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/push"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/push") + ":" + q.Get("tag")
		f.log("push %s", name)
		f.mu.Lock()
		f.auth = r.Header.Get("X-Registry-Auth")
		f.mu.Unlock()
		fl := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		for i, l := range f.pushStream {
			_, _ = io.WriteString(w, l+"\n")
			fl.Flush()
			if i == 0 && f.pushHold != nil {
				f.pushing <- struct{}{}
				select {
				case <-f.pushHold:
				case <-r.Context().Done():
					f.log("push aborted")
					return
				}
			}
		}
	case r.Method == "POST" && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/tag"):
		src := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/tag")
		name := q.Get("repo") + ":" + q.Get("tag")
		f.log("tag %s -> %s", src, name)
		f.mu.Lock()
		f.images[name] = src
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case r.Method == "DELETE" && strings.HasPrefix(p, "/images/"):
		name := strings.TrimPrefix(p, "/images/")
		f.log("delete %s", name)
		f.mu.Lock()
		_, ok := f.images[name]
		delete(f.images, name)
		f.mu.Unlock()
		if !ok {
			notFound()
			return
		}
		_, _ = io.WriteString(w, `[{"Untagged":"`+name+`"}]`)
	default:
		http.Error(w, `{"message":"unexpected request"}`, http.StatusNotImplemented)
	}
}

const img = "reg.test:5000/u/img:v1"

// a push stream shaped like the daemon's: two layers, one already there
var okStream = []string{
	`{"status":"The push refers to repository [reg.test:5000/u/img]"}`,
	`{"status":"Preparing","progressDetail":{},"id":"aaa"}`,
	`{"status":"Preparing","progressDetail":{},"id":"bbb"}`,
	`{"status":"Layer already exists","progressDetail":{},"id":"bbb"}`,
	`{"status":"Pushing","progressDetail":{"current":512,"total":2048},"progress":"[==>  ]","id":"aaa"}`,
	`{"status":"Pushing","progressDetail":{"current":2048,"total":2048},"id":"aaa"}`,
	`{"status":"Pushing","progressDetail":{"current":3000},"id":"aaa"}`,
	`{"status":"Pushed","progressDetail":{},"id":"aaa"}`,
	`{"status":"v1: digest: sha256:` + strings.Repeat("ab", 32) + ` size: 741"}`,
	`{"progressDetail":{},"aux":{"Tag":"v1","Digest":"sha256:` + strings.Repeat("ab", 32) + `","Size":741}}`,
}

func runDocker(t *testing.T, ctx context.Context, f *fakeDocker, extra ...string) (int, []string, string, string) {
	t.Helper()
	var buf bytes.Buffer
	term := filepath.Join(t.TempDir(), "term")
	args := append([]string{"--runtime=docker", "--socket=" + f.socket, "--container=c1", "--image=" + img, "--termination-log=" + term}, extra...)
	o, err := parseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newDockerBackend(o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	code := run(ctx, o, b, newEmitter(&buf, term))
	res, _ := os.ReadFile(term)
	return code, protoLines(buf.String()), strings.TrimSpace(string(res)), buf.String()
}

func TestDocker_CommitPushAndCleanUp(t *testing.T) {
	f := newFakeDocker(t)
	f.pushStream = okStream
	cfg := t.TempDir()
	_ = os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"auths":{"reg.test:5000":{"username":"robot","password":"pw"}}}`), 0o600)
	code, lines, res, out := runDocker(t, context.Background(), f, "--docker-config="+cfg)
	digest := "sha256:" + strings.Repeat("ab", 32)
	if code != 0 || res != "digest="+digest+" size=4242" {
		t.Fatalf("code %d result %q\n%s", code, res, out)
	}
	if lines[0] != "@stage commit" || lines[1] != "@size 4242" || lines[2] != "@stage push" || lines[len(lines)-2] != "@digest "+digest || lines[len(lines)-1] != "@done" {
		t.Fatalf("lines %v", lines)
	}
	// progress is summed over layers and ends complete; no raw frames in the log
	if last := lines[len(lines)-3]; last != "@push 2048 2048" {
		t.Fatalf("final progress %q in %v", last, lines)
	}
	if strings.Contains(out, "progressDetail") || strings.Contains(out, "[==>") {
		t.Fatalf("raw progress frames leaked:\n%s", out)
	}
	if !f.has("commit c1 -> "+img+" pause=true") || !f.has("push "+img) {
		t.Fatalf("calls %v", f.calls)
	}
	// the registry has it: the name this run created leaves the node
	if !f.has("delete "+img) || f.image(img) != "" {
		t.Fatalf("not cleaned up: calls %v images %v", f.calls, f.images)
	}
	raw, _ := base64.URLEncoding.DecodeString(f.auth)
	var a registryAuth
	if json.Unmarshal(raw, &a) != nil || a.Username != "robot" || a.Password != "pw" || a.ServerAddress != "reg.test:5000" {
		t.Fatalf("X-Registry-Auth %q", raw)
	}
}

func TestDocker_PushErrorRemovesTheImage(t *testing.T) {
	f := newFakeDocker(t)
	f.pushStream = []string{
		`{"status":"The push refers to repository [reg.test:5000/u/img]"}`,
		`{"status":"Preparing","progressDetail":{},"id":"aaa"}`,
		`{"errorDetail":{"message":"unauthorized: authentication required"},"error":"unauthorized: authentication required"}`,
	}
	code, lines, res, out := runDocker(t, context.Background(), f, "--pause=false")
	if code != 1 || lines[len(lines)-1] != "@error push failed: unauthorized: authentication required" || res != "error=push failed: unauthorized: authentication required" {
		t.Fatalf("code %d lines %v result %q\n%s", code, lines, res, out)
	}
	if !f.has("commit c1 -> "+img+" pause=false") || !f.has("delete "+img) || f.image(img) != "" {
		t.Fatalf("calls %v images %v", f.calls, f.images)
	}
}

func TestDocker_CommitErrorCarriesTheDaemonsMessage(t *testing.T) {
	f := newFakeDocker(t)
	code, lines, _, _ := runDocker(t, context.Background(), f, "--container=missing")
	if code != 1 || lines[len(lines)-1] != "@error commit failed: No such container: missing" {
		t.Fatalf("code %d lines %v", code, lines)
	}
	if f.has("push "+img) || f.has("delete "+img) {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestDocker_CancelDuringPushRemovesTheImage(t *testing.T) {
	f := newFakeDocker(t)
	f.pushStream = okStream
	f.pushHold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-f.pushing // the push is under way
		cancel()    // SIGTERM
	}()
	code, lines, res, out := runDocker(t, ctx, f)
	if code != 143 || lines[len(lines)-1] != "@error cancelled" || res != "error=cancelled" {
		t.Fatalf("code %d lines %v result %q\n%s", code, lines, res, out)
	}
	if !f.has("delete "+img) || f.image(img) != "" {
		t.Fatalf("the cancelled run left its image: calls %v images %v", f.calls, f.images)
	}
}

func TestDocker_ANameThatExistedIsRestoredNotRemoved(t *testing.T) {
	// failure: the name goes back to the image it pointed at
	f := newFakeDocker(t)
	f.images[img] = "sha256:old"
	f.pushStream = []string{`{"errorDetail":{"message":"denied"},"error":"denied"}`}
	if code, _, _, out := runDocker(t, context.Background(), f); code != 1 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if f.has("delete "+img) || f.image(img) != "sha256:old" || !f.has("tag sha256:old -> "+img) {
		t.Fatalf("calls %v images %v", f.calls, f.images)
	}
	// success: the name stays, on the new image
	f = newFakeDocker(t)
	f.images[img] = "sha256:old"
	f.pushStream = okStream
	if code, _, _, out := runDocker(t, context.Background(), f); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if f.has("delete "+img) || f.image(img) != "sha256:new" {
		t.Fatalf("calls %v images %v", f.calls, f.images)
	}
}

func TestDocker_CommitOnlyKeepsTheImage(t *testing.T) {
	f := newFakeDocker(t)
	code, lines, res, _ := runDocker(t, context.Background(), f, "--push=false")
	if code != 0 || strings.Join(lines, "|") != "@stage commit|@size 4242|@done" || res != "size=4242" {
		t.Fatalf("code %d lines %v result %q", code, lines, res)
	}
	if f.has("push "+img) || f.image(img) != "sha256:new" {
		t.Fatalf("calls %v images %v", f.calls, f.images)
	}
}
