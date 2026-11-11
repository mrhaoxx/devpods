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
	p := newLayerProgress(nil)
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
	lp := newLayerProgress(nil)
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
func (f *fakeBackend) Push(_ context.Context, progress func(int64, int64), layer func(string, int64)) (string, error) {
	f.calls = append(f.calls, "push")
	progress(1, 2)
	layer("layer-sha256:1a2b3c4d5e6f7a8b9c0d", 2<<20)
	layer("manifest-sha256:feedfacefeedface0000", 0)
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

// The log box in the console (and the CLI) shows the lines that are not
// protocol: a run must say what it is doing in words too.
func TestRun_SaysWhatItDoes(t *testing.T) {
	var buf bytes.Buffer
	b := &fakeBackend{size: 3 << 20, digest: "sha256:feed"}
	o := options{push: true, pause: true, container: "f2dab30670b4677ba01e59145ca8d01d", image: "reg.test/u/img:v1"}
	if code := run(context.Background(), o, b, newEmitter(&buf, filepath.Join(t.TempDir(), "term"))); code != 0 {
		t.Fatalf("exit %d\n%s", code, buf.String())
	}
	var human []string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !strings.HasPrefix(l, "@") {
			human = append(human, l)
		}
	}
	want := []string{
		"提交容器 f2dab30670b4（期间容器暂停）",
		"已提交，镜像 3.0 MiB",
		"推送到 reg.test/u/img:v1",
		"层 sha256:1a2b3c4d5e6f 2.0 MiB 已推送",
		"清单 sha256:feedfacefeed 仓库里已有，跳过",
		"推送完成，摘要 sha256:feed",
		"完成，用时 0s",
	}
	if strings.Join(human, "|") != strings.Join(want, "|") {
		t.Fatalf("log lines\n got %q\nwant %q", human, want)
	}
}

func TestLayerProgress_TellsOncePerLayer(t *testing.T) {
	var told []string
	lp := newLayerProgress(func(id string, size int64) { told = append(told, fmt.Sprintf("%s=%d", id, size)) })
	lp.Set("a", 5, 10)
	lp.Set("a", 10, 10) // reaches its total
	lp.Set("a", 10, 10)
	lp.Finish("a")
	lp.Finish("b") // never reported a size: the registry had it
	lp.Finish("b")
	if strings.Join(told, ",") != "a=10,b=0" {
		t.Fatalf("told %v", told)
	}
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
	// the API range the daemon serves (GET /version); requests outside it
	// are refused the way the daemon's version middleware does
	apiMax, apiMin string
	hasAuthHeader  bool
	commitCT       string
	commitBody     int64
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
	f := &fakeDocker{t: t, images: map[string]string{}, socket: filepath.Join(dir, "d.sock"), pushing: make(chan struct{}, 1), apiMax: "1.47", apiMin: "1.24"}
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
	if r.Method == "GET" && r.URL.Path == "/version" {
		f.log("version")
		// the fields of the reference's SystemVersion this code reads
		_, _ = fmt.Fprintf(w, `{"Version":"27.5.1","ApiVersion":%q,"MinAPIVersion":%q,"Os":"linux","Arch":"amd64"}`, f.apiMax, f.apiMin)
		return
	}
	ver, p, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v"), "/")
	if !ok || !strings.HasPrefix(r.URL.Path, "/v") {
		http.Error(w, `{"message":"page not found"}`, http.StatusNotFound)
		return
	}
	p = "/" + p
	// moby api/server/middleware/version.go
	if apiVersionLess(ver, f.apiMin) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"message":"client version %s is too old. Minimum supported API version is %s, please upgrade your client to a newer version"}`, ver, f.apiMin)
		return
	}
	if apiVersionLess(f.apiMax, ver) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"message":"client version %s is too new. Maximum supported API version is %s"}`, ver, f.apiMax)
		return
	}
	f.log("api %s", ver)
	q := r.URL.Query()
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"No such image"}`)
	}
	switch {
	case r.Method == "POST" && p == "/commit":
		name := q.Get("repo") + ":" + q.Get("tag")
		f.log("commit %s -> %s pause=%s", q.Get("container"), name, q.Get("pause"))
		f.mu.Lock()
		f.commitCT, f.commitBody = r.Header.Get("Content-Type"), r.ContentLength
		f.mu.Unlock()
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
		_, f.hasAuthHeader = r.Header["X-Registry-Auth"]
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

// the containerd image store's stream (moby daemon/containerd/progress.go
// and image_push.go; the default store from Docker 29): the same layer
// statuses, the digest only in the closing status line — no aux.
var c8dStream = []string{
	`{"status":"The push refers to repository [reg.test:5000/u/img]"}`,
	`{"status":"Waiting","progressDetail":{},"id":"0123456789ab"}`,
	`{"status":"Pushing","progressDetail":{"current":1024,"total":4096},"id":"0123456789ab"}`,
	`{"status":"Mounted from library/ubuntu","progressDetail":{},"id":"cdef01234567"}`,
	`{"status":"Pushed","progressDetail":{},"id":"0123456789ab"}`,
	`{"status":"v1: digest: sha256:` + strings.Repeat("cd", 32) + ` size: 855"}`,
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

// Docker 29 refuses API versions below 1.44; an old daemon tops out below
// the version this code was written against. Either way the calls go out
// in a version the daemon serves.
func TestDocker_SpeaksAVersionTheDaemonServes(t *testing.T) {
	for _, c := range []struct{ min, max, want string }{
		{"1.24", "1.47", "1.41"}, // Docker 27: ours is in range
		{"1.44", "1.52", "1.44"}, // Docker 29: its minimum
		{"1.12", "1.40", "1.40"}, // Docker 19.03: its maximum
	} {
		f := newFakeDocker(t)
		f.apiMin, f.apiMax = c.min, c.max
		f.pushStream = okStream
		if code, _, _, out := runDocker(t, context.Background(), f); code != 0 {
			t.Fatalf("daemon %s..%s: exit %d\n%s", c.min, c.max, code, out)
		}
		if !f.has("api "+c.want) || !f.has("version") {
			t.Fatalf("daemon %s..%s: calls %v", c.min, c.max, f.calls)
		}
		for _, call := range f.calls {
			if strings.HasPrefix(call, "api ") && call != "api "+c.want {
				t.Fatalf("daemon %s..%s: a call used %q", c.min, c.max, call)
			}
		}
	}
	if pickAPIVersion("", "") != "1.41" || !apiVersionLess("1.9", "1.41") || apiVersionLess("1.44", "1.41") {
		t.Fatal("version comparison")
	}
}

// The request shapes the reference asks for: commit without a body (so
// no Content-Type), and X-Registry-Auth always present — "{}" when the
// push is anonymous.
func TestDocker_RequestShapes(t *testing.T) {
	f := newFakeDocker(t)
	f.pushStream = okStream
	if code, _, _, out := runDocker(t, context.Background(), f); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if f.commitCT != "" || f.commitBody > 0 {
		t.Fatalf("commit sent Content-Type %q, %d body bytes", f.commitCT, f.commitBody)
	}
	raw, err := base64.URLEncoding.DecodeString(f.auth)
	if !f.hasAuthHeader || err != nil || string(raw) != "{}" {
		t.Fatalf("anonymous X-Registry-Auth: present=%v %q %v", f.hasAuthHeader, raw, err)
	}
}

// With the containerd image store the digest only appears in the closing
// status line, and a layer mounted from another repository never reports
// a size.
func TestDocker_ContainerdImageStoreStream(t *testing.T) {
	f := newFakeDocker(t)
	f.pushStream = c8dStream
	code, lines, res, out := runDocker(t, context.Background(), f)
	digest := "sha256:" + strings.Repeat("cd", 32)
	if code != 0 || res != "digest="+digest+" size=4242" {
		t.Fatalf("code %d result %q\n%s", code, res, out)
	}
	if last := lines[len(lines)-3]; last != "@push 4096 4096" {
		t.Fatalf("final progress %q in %v", last, lines)
	}
}

// A stream that ends without a digest and without an error is a failure,
// not a success with nothing to show.
func TestDocker_NoDigestIsAFailure(t *testing.T) {
	f := newFakeDocker(t)
	f.pushStream = okStream[:8]
	code, lines, _, out := runDocker(t, context.Background(), f)
	if code != 1 || !strings.Contains(lines[len(lines)-1], "without reporting a digest") {
		t.Fatalf("code %d lines %v\n%s", code, lines, out)
	}
	if f.image(img) != "" {
		t.Fatalf("the image was left on the node: %v", f.images)
	}
}

// config.json the way the docker CLI reads it.
func TestRegistryAuth_LikeTheDockerCLI(t *testing.T) {
	write := func(body string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	// the exact key wins over one that merely reduces to the host; "auth" wins over the separate fields
	dir := write(`{"auths":{"https://harbor.x.io/v2/":{"username":"legacy","password":"old"},"harbor.x.io":{"auth":"` + b64("robot:pw") + `","username":"stale","password":"stale"}}}`)
	if a, err := loadRegistryAuth(dir, "harbor.x.io/p/img:1"); err != nil || a.Username != "robot" || a.Password != "pw" {
		t.Fatalf("exact key / auth precedence: %+v %v", a, err)
	}
	// a host with a port; a legacy key with scheme and path
	dir = write(`{"auths":{"http://reg.test:5000/v1/":{"auth":"` + b64("u:p:with:colons") + `"}}}`)
	if a, err := loadRegistryAuth(dir, "reg.test:5000/u/img:v1"); err != nil || a.Username != "u" || a.Password != "p:with:colons" || a.ServerAddress != "reg.test:5000" {
		t.Fatalf("legacy key: %+v %v", a, err)
	}
	// an identity token instead of a password
	dir = write(`{"auths":{"harbor.x.io":{"identitytoken":"tok"}}}`)
	if a, err := loadRegistryAuth(dir, "harbor.x.io/p/img:1"); err != nil || a.IdentityToken != "tok" || a.Username != "" {
		t.Fatalf("identity token: %+v %v", a, err)
	}
	// credential helpers cannot run here: say so instead of pushing anonymously
	for _, body := range []string{
		`{"auths":{"harbor.x.io":{}},"credsStore":"osxkeychain"}`,
		`{"credHelpers":{"harbor.x.io":"ecr-login"}}`,
	} {
		if _, err := loadRegistryAuth(write(body), "harbor.x.io/p/img:1"); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("%s: %v", body, err)
		}
	}
	// a helper for another registry does not concern this push
	if a, err := loadRegistryAuth(write(`{"credHelpers":{"other.io":"x"}}`), "harbor.x.io/p/img:1"); err != nil || a.Username != "" {
		t.Fatalf("unrelated helper: %+v %v", a, err)
	}
	if _, err := loadRegistryAuth(write(`{"auths":{"harbor.x.io":{"auth":"!!!"}}}`), "harbor.x.io/p/img:1"); err == nil {
		t.Fatal("a malformed auth value was accepted")
	}
}
