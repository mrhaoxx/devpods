// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// snapFake plays the snapshot API (snapshot v2 spec §2.3): JSON routes
// plus a progress stream the test feeds event by event.
type snapFake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []string
	events chan string // one progress object per send; close ends the stream
	phase  string      // what GET /api/snapshots/s1 reports
}

const snapImage = "harbor.test/devpod-users/alice/a:20261111-1530"

func (f *snapFake) view() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{"id": "s1", "devpod": "alice-a", "owner": "alice", "image": snapImage, "phase": f.phase,
		"digest": "sha256:abc", "sizeBytes": 1288490188, "createdAt": "2026-11-11T11:58:00Z"}
}

func newSnapFake(t *testing.T) *snapFake {
	f := &snapFake{t: t, events: make(chan string, 16), phase: "Running"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gwk_k3y" || r.Header.Get(UserHeader) != "alice" {
			t.Errorf("headers: Authorization=%q %s=%q", r.Header.Get("Authorization"), UserHeader, r.Header.Get(UserHeader))
		}
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.RequestURI()
		f.mu.Lock()
		f.calls = append(f.calls, strings.TrimSpace(key+" "+string(body)))
		f.mu.Unlock()
		switch key {
		case "POST /api/devpods/alice-a/snapshots", "POST /api/devpods/show/snapshots":
			// an address outside the caller's default path needs its own credentials (§2.5)
			if bytes.Contains(body, []byte(`"image":"reg.test/`)) && !bytes.Contains(body, []byte(`"pushAuth"`)) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"code":"PUSH_AUTH_REQUIRED","message":"this address needs credentials"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(f.view())
		case "GET /api/snapshots/s1":
			_ = json.NewEncoder(w).Encode(f.view())
		case "DELETE /api/snapshots/s1":
			w.WriteHeader(http.StatusNoContent)
		case "GET /api/snapshots", "GET /api/snapshots?scope=all&devpod=alice-a":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{f.view()}})
		case "GET /api/snapshots/s1/progress":
			if r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("progress Accept = %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for {
				select {
				case ev, ok := <-f.events:
					if !ok {
						return
					}
					fmt.Fprintf(w, "data: %s\n\n", ev)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"NOT_FOUND","message":"no such snapshot"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *snapFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// lastBody is the most recent call as recorded ("METHOD path body").
func (f *snapFake) lastBody() string {
	if c := f.seen(); len(c) > 0 {
		return c[len(c)-1]
	}
	return ""
}

func (f *snapFake) count(prefix string) int {
	n := 0
	for _, c := range f.seen() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// run executes one command line; mod adjusts the Env (tty, interrupt, …).
func (f *snapFake) run(line string, mod func(*Env)) (code int, out, errs string) {
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

// a whole successful snapshot, as the progress stream tells it
var snapScript = []string{
	`{"phase":"Pending","stage":"pending","elapsedSeconds":0,"lines":[]}`,
	`{"phase":"Running","stage":"commit","elapsedSeconds":12,"lines":["committing container abc"]}`,
	`{"phase":"Running","stage":"push","elapsedSeconds":48,"sizeBytes":1288490188,"lines":["committing container abc","layer 1 pushed","layer 2 pushed"]}`,
	`{"phase":"Succeeded","stage":"done","elapsedSeconds":50,"sizeBytes":1288490188,"digest":"sha256:abc","lines":["layer 1 pushed","layer 2 pushed","pushed"]}`,
}

func (f *snapFake) play(events ...string) {
	for _, ev := range events {
		f.events <- ev
	}
}

func TestSnapshotDispatchAndRequests(t *testing.T) {
	f := newSnapFake(t)
	// --detach: create only
	code, out, errs := f.run("snapshot alice-a --name env --tag v1 --no-pause --detach", nil)
	if code != ExitOK || !strings.Contains(out, "s1") || !strings.Contains(out, snapImage) || !strings.Contains(out, "snapshot follow s1") {
		t.Fatalf("detach: %d %q %q", code, out, errs)
	}
	// the default name is the DevPod's name without the caller's prefix
	if code, _, _ := f.run("snapshot alice-a --detach", nil); code != ExitOK {
		t.Fatal("default name")
	}
	// a DevPod named like a verb is reached through `create`
	if code, _, _ := f.run("snapshot create show --detach", nil); code != ExitOK {
		t.Fatal("create show")
	}
	if code, out, _ := f.run("snapshot show s1", nil); code != ExitOK || !strings.Contains(out, "image:") || !strings.Contains(out, "1.2 GiB") {
		t.Fatalf("show: %d %q", code, out)
	}
	if code, out, _ := f.run("snapshot show s1 --json", nil); code != ExitOK || !strings.Contains(out, `"id":"s1"`) {
		t.Fatalf("show --json: %d %q", code, out)
	}
	// --detach --json hands back the API's snapshot view
	if code, out, _ := f.run("snapshot alice-a --detach --json", nil); code != ExitOK || !strings.Contains(out, `"image":"`+snapImage+`"`) {
		t.Fatalf("detach --json: %d %q", code, out)
	}
	want := []string{
		`POST /api/devpods/alice-a/snapshots {"name":"env","tag":"v1","pause":false}`,
		`POST /api/devpods/alice-a/snapshots {"name":"a"}`,
		`POST /api/devpods/show/snapshots {"name":"show"}`,
		`GET /api/snapshots/s1`,
		`GET /api/snapshots/s1`,
		`POST /api/devpods/alice-a/snapshots {"name":"a"}`,
	}
	if got := f.seen(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// usage errors never reach the API
	for _, line := range []string{"snapshot", "snapshot show", "snapshot follow", "snapshot cancel", "snapshot rm", "snapshot create", "snapshot alice-a extra", "snapshot alice-a --bogus"} {
		before := len(f.seen())
		if code, _, errs := f.run(line, nil); code != ExitUsage || !strings.Contains(errs, "usage: snapshot") || len(f.seen()) != before {
			t.Errorf("%q: %d %q", line, code, errs)
		}
	}
	var h bytes.Buffer
	if !Help(&h, "snapshot") || !strings.Contains(h.String(), "Ctrl-C") || !strings.Contains(h.String(), "--detach") {
		t.Fatalf("help: %q", h.String())
	}
}

func TestSnapshotsList(t *testing.T) {
	f := newSnapFake(t)
	f.phase = "Succeeded"
	code, out, _ := f.run("snapshots", nil)
	want := "ID  DEVPOD   IMAGE                                           SIZE     STATUS     AGE\n" +
		"s1  alice-a  " + snapImage + "  1.2 GiB  Succeeded  2m\n"
	if code != ExitOK || out != want {
		t.Fatalf("snapshots: %d\n%q\nwant\n%q", code, out, want)
	}
	if code, out, _ := f.run("images --all --devpod alice-a --json", nil); code != ExitOK || !strings.HasPrefix(out, `{"items":[`) {
		t.Fatalf("images --json: %d %q", code, out)
	}
	if got := f.seen(); !reflect.DeepEqual(got, []string{"GET /api/snapshots", "GET /api/snapshots?scope=all&devpod=alice-a"}) {
		t.Fatalf("requests %v", got)
	}
}

func TestSnapshotNewTag(t *testing.T) {
	// the fake clock is 2026-11-11 12:00 UTC = 20:00 in Shanghai
	for cmd, want := range map[string]string{
		"snapshot alice-a --new-tag --detach":                               `"tag":"20261111-200000"`,
		"snapshot alice-a --image reg.test:5000/t/a:old --new-tag --detach": `"image":"reg.test:5000/t/a:20261111-200000"`,
		"snapshot alice-a --image reg.test:5000/t/a --new-tag --detach":     `"image":"reg.test:5000/t/a:20261111-200000"`,
	} {
		f := newSnapFake(t)
		if code, out, errs := f.run(cmd, nil); code != ExitOK || !strings.Contains(f.lastBody(), want) {
			t.Fatalf("%s: exit %d body %s out %q err %q", cmd, code, f.lastBody(), out, errs)
		}
	}
	f := newSnapFake(t)
	if code, _, errs := f.run("snapshot alice-a --tag v1 --new-tag", nil); code != ExitUsage || !strings.Contains(errs, "exclude") {
		t.Fatalf("--tag with --new-tag: %d %q", code, errs)
	}
}

func TestSnapshotFollow(t *testing.T) {
	t.Run("plain output: stages and new log lines, then the image", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(snapScript...)
		code, out, errs := f.run("snapshot alice-a", nil)
		want := "快照 s1 → " + snapImage + "\n" +
			"[等待开始]\n[提交容器]\ncommitting container abc\n[推送镜像]\nlayer 1 pushed\nlayer 2 pushed\npushed\n" +
			"完成：" + snapImage + "@sha256:abc（1.2 GiB，50s）\n"
		if code != ExitOK || out != want || errs != "" {
			t.Fatalf("code %d\n%q\nwant\n%q\nerr %q", code, out, want, errs)
		}
		if strings.ContainsAny(out, "\r\x1b") {
			t.Fatal("control characters without a terminal")
		}
		if f.count("DELETE") != 0 {
			t.Fatal("a finished snapshot was deleted")
		}
	})
	t.Run("terminal: the push step shows uploaded / total", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(
			`{"phase":"Running","stage":"push","elapsedSeconds":20,"sizeBytes":2147483648,"pushedBytes":268435456,"totalBytes":1073741824,"lines":[]}`,
			snapScript[3],
		)
		code, out, _ := f.run("snapshot alice-a", func(e *Env) { e.TTY = true })
		if code != ExitOK || !strings.Contains(out, "[推送镜像] 20s 256.0 MiB / 1.0 GiB (25%)") {
			t.Fatalf("push progress: %d %q", code, out)
		}
	})
	t.Run("terminal: one status line updated in place", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(snapScript...)
		code, out, _ := f.run("snapshot alice-a", func(e *Env) { e.TTY = true })
		want := "快照 s1 → " + snapImage + "\n" +
			"[等待开始] 0s" +
			"\r\x1b[Kcommitting container abc\n[提交容器] 12s" +
			"\r\x1b[Klayer 1 pushed\nlayer 2 pushed\n[推送镜像] 48s 1.2 GiB" +
			"\r\x1b[Kpushed\n" +
			"完成：" + snapImage + "@sha256:abc（1.2 GiB，50s）\n"
		if code != ExitOK || out != want {
			t.Fatalf("tty: %d %q", code, out)
		}
	})
	t.Run("--json: one progress object per line, with the id and image", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(snapScript...)
		code, out, _ := f.run("snapshot alice-a --json", nil)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if code != ExitOK || len(lines) != len(snapScript) {
			t.Fatalf("ndjson: %d %q", code, out)
		}
		for i, l := range lines {
			var p map[string]any
			if err := json.Unmarshal([]byte(l), &p); err != nil || p["id"] != "s1" || p["image"] != snapImage || p["stage"] == nil {
				t.Fatalf("line %d: %q %v", i, l, err)
			}
		}
	})
	t.Run("failure: the message, exit 1", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(snapScript[0], `{"phase":"Failed","stage":"error","elapsedSeconds":3,"message":"push denied","lines":["denied: no access"]}`)
		code, out, errs := f.run("snapshot follow s1", nil)
		if code != ExitAPI || !strings.Contains(out, "denied: no access") || errs != "失败：push denied\n" {
			t.Fatalf("failure: %d %q %q", code, out, errs)
		}
	})
	t.Run("a silent stream is given up on; the snapshot goes on", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(snapScript[0])
		code, _, errs := f.run("snapshot alice-a", func(e *Env) { e.FollowIdle = 100 * time.Millisecond })
		if code != ExitAPI || !strings.Contains(errs, "snapshot follow s1") || f.count("DELETE") != 0 {
			t.Fatalf("idle: %d %q %v", code, errs, f.seen())
		}
	})
	t.Run("the stream ending early is not a success", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(snapScript[0])
		close(f.events)
		code, _, errs := f.run("snapshot follow s1", nil)
		if code != ExitAPI || !strings.Contains(errs, "snapshot follow s1") {
			t.Fatalf("early end: %d %q", code, errs)
		}
	})
	t.Run("log lines cannot drive the terminal", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(`{"phase":"Running","stage":"commit","elapsedSeconds":1,"lines":["evil\u001b[2Jline\rx"]}`, snapScript[3])
		_, out, _ := f.run("snapshot follow s1", nil)
		if strings.ContainsAny(out, "\x1b\r") || !strings.Contains(out, "evil[2Jlinex") {
			t.Fatalf("unsanitised: %q", out)
		}
	})
}

func TestSnapshotInterrupt(t *testing.T) {
	interrupted := func(f *snapFake) func(*Env) {
		return func(e *Env) {
			ch := make(chan struct{})
			e.Interrupt = ch
			go func() { // interrupt once the stream is being followed
				for f.count("GET /api/snapshots/s1/progress") == 0 {
					time.Sleep(5 * time.Millisecond)
				}
				time.Sleep(20 * time.Millisecond)
				close(ch)
			}()
		}
	}
	t.Run("creating and following: an interrupt cancels", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(snapScript[:2]...)
		code, out, _ := f.run("snapshot alice-a", interrupted(f))
		if code != ExitInterrupted || !strings.HasSuffix(out, "已取消\n") || f.count("DELETE /api/snapshots/s1") != 1 {
			t.Fatalf("cancel: %d %q %v", code, out, f.seen())
		}
	})
	t.Run("re-attached with follow: an interrupt only stops following", func(t *testing.T) {
		f := newSnapFake(t)
		f.play(snapScript[:2]...)
		code, _, errs := f.run("snapshot follow s1", interrupted(f))
		if code != ExitInterrupted || f.count("DELETE") != 0 || !strings.Contains(errs, "snapshot cancel s1") {
			t.Fatalf("follow: %d %q %v", code, errs, f.seen())
		}
	})
}

func TestSnapshotCancelAndRemove(t *testing.T) {
	f := newSnapFake(t)
	if code, out, _ := f.run("snapshot cancel s1", nil); code != ExitOK || out != "已取消\n" {
		t.Fatalf("cancel running: %d %q", code, out)
	}
	// rm refuses a snapshot still in progress (that would cancel it)
	if code, _, errs := f.run("snapshot rm s1", nil); code != ExitAPI || !strings.Contains(errs, "snapshot cancel s1") {
		t.Fatalf("rm running: %d %q", code, errs)
	}
	f.phase = "Succeeded"
	// cancel refuses a finished one (that would delete its record)
	if code, _, errs := f.run("snapshot cancel s1", nil); code != ExitAPI || !strings.Contains(errs, "snapshot rm s1") {
		t.Fatalf("cancel finished: %d %q", code, errs)
	}
	if code, out, _ := f.run("snapshot rm s1", nil); code != ExitOK || !strings.Contains(out, "removed") {
		t.Fatalf("rm finished: %d %q", code, out)
	}
	if n := f.count("DELETE /api/snapshots/s1"); n != 2 {
		t.Fatalf("DELETEs: %d %v", n, f.seen())
	}
	if code, _, errs := f.run("snapshot show nope", nil); code != ExitAPI || !strings.Contains(errs, "NOT_FOUND") {
		t.Fatalf("unknown id: %d %q", code, errs)
	}
}

func TestNewLines(t *testing.T) {
	for _, c := range []struct{ prev, cur, want []string }{
		{nil, []string{"a", "b"}, []string{"a", "b"}},
		{[]string{"a", "b"}, []string{"a", "b"}, nil},
		{[]string{"a", "b"}, []string{"a", "b", "c"}, []string{"c"}},
		{[]string{"a", "b", "c"}, []string{"b", "c", "d", "e"}, []string{"d", "e"}}, // the tail slid
		{[]string{"a", "b"}, []string{"x", "y"}, []string{"x", "y"}},                // nothing in common
	} {
		if got := newLines(c.prev, c.cur); !reflect.DeepEqual(got, c.want) {
			t.Errorf("newLines(%v, %v) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

// Over a real SSH connection: what counts as an interrupt.
func TestSnapshotInterruptOverSSH(t *testing.T) {
	waitFor := func(t *testing.T, what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	rig := func(t *testing.T) (*snapFake, *ssh.Client) {
		f := newSnapFake(t)
		f.play(snapScript[:2]...)
		return f, sshRig(t, &Server{API: NewAPI(f.srv.URL, "k3y")})
	}
	following := func(f *snapFake) func() bool {
		return func() bool { return f.count("GET /api/snapshots/s1/progress") == 1 }
	}
	deleted := func(f *snapFake) func() bool { return func() bool { return f.count("DELETE /api/snapshots/s1") == 1 } }

	t.Run("^C on a terminal", func(t *testing.T) {
		f, cl := rig(t)
		s, _ := cl.NewSession()
		defer s.Close()
		in, _ := s.StdinPipe()
		var o bytes.Buffer
		s.Stdout = &o
		if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
		if err := s.Start("snapshot alice-a"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the follow", following(f))
		_, _ = in.Write([]byte{3})
		if code := exitCode(s.Wait()); code != ExitInterrupted || !strings.Contains(o.String(), "已取消") {
			t.Fatalf("^C: %d %q", code, o.String())
		}
		if n := f.count("DELETE"); n != 1 {
			t.Fatalf("DELETEs: %d", n)
		}
	})
	t.Run("an SSH signal", func(t *testing.T) {
		f, cl := rig(t)
		s, _ := cl.NewSession()
		defer s.Close()
		if err := s.Start("snapshot alice-a"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the follow", following(f))
		if err := s.Signal(ssh.SIGINT); err != nil {
			t.Fatal(err)
		}
		if code := exitCode(s.Wait()); code != ExitInterrupted || f.count("DELETE") != 1 {
			t.Fatalf("signal: %d %v", code, f.seen())
		}
	})
	t.Run("closing stdin is not an interrupt; closing the channel is", func(t *testing.T) {
		f, cl := rig(t)
		s, _ := cl.NewSession()
		in, _ := s.StdinPipe()
		if err := s.Start("snapshot alice-a"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the follow", following(f))
		_ = in.Close() // e.g. `ssh -n`, or an agent without stdin
		time.Sleep(150 * time.Millisecond)
		if f.count("DELETE") != 0 {
			t.Fatal("EOF on stdin cancelled the snapshot")
		}
		_ = s.Close()
		waitFor(t, "the cancel", deleted(f))
		time.Sleep(100 * time.Millisecond)
		if n := f.count("DELETE"); n != 1 {
			t.Fatalf("DELETEs: %d", n)
		}
	})
	t.Run("the connection going away", func(t *testing.T) {
		f, cl := rig(t)
		s, _ := cl.NewSession()
		if err := s.Start("snapshot alice-a"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the follow", following(f))
		_ = cl.Close()
		waitFor(t, "the cancel", deleted(f))
	})
	t.Run("--detach: disconnecting cancels nothing", func(t *testing.T) {
		f, cl := rig(t)
		s, _ := cl.NewSession()
		if err := s.Run("snapshot alice-a --detach"); err != nil {
			t.Fatal(err)
		}
		_ = cl.Close()
		time.Sleep(150 * time.Millisecond)
		if f.count("DELETE") != 0 || f.count("GET /api/snapshots/s1/progress") != 0 {
			t.Fatalf("detached: %v", f.seen())
		}
	})
	t.Run("at the prompt: ^C cancels and the prompt comes back", func(t *testing.T) {
		f, cl := rig(t)
		s, _ := cl.NewSession()
		defer s.Close()
		in, _ := s.StdinPipe()
		var o syncBuffer
		s.Stdout = &o
		if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
		if err := s.Shell(); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(in, "snapshot alice-a\r")
		waitFor(t, "the follow", following(f))
		_, _ = in.Write([]byte{3})
		waitFor(t, "the cancel", deleted(f))
		waitFor(t, "the prompt", func() bool { return strings.Count(o.String(), "devpod> ") >= 2 })
		_, _ = io.WriteString(in, "whoami --bogus\rexit\r") // the session still takes commands
		if code := exitCode(s.Wait()); code != ExitUsage || !strings.Contains(o.String(), "已取消") || !strings.Contains(o.String(), "unknown flag") {
			t.Fatalf("prompt: %d %q", code, o.String())
		}
	})
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A custom image address takes its own credentials; the password is
// typed without echo or read from standard input, never an argument.
func TestSnapshotCustomImageCredentials(t *testing.T) {
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
	wantBody := `POST /api/devpods/alice-a/snapshots {"image":"reg.test/me/env:v1","pushAuth":{"username":"bob","password":"` + secret + `"}}`

	t.Run("terminal: --username, then the password without echo", func(t *testing.T) {
		f := newSnapFake(t)
		var asked []ask
		code, out, errs := f.run("snapshot alice-a --image reg.test/me/env:v1 --username bob --detach --json", prompts(&asked, secret))
		if code != ExitOK || len(asked) != 1 || asked[0].echo || !strings.Contains(asked[0].prompt, "密码") {
			t.Fatalf("prompt: %d %+v %q", code, asked, errs)
		}
		if got := f.seen(); !reflect.DeepEqual(got, []string{wantBody}) {
			t.Fatalf("request %v", got)
		}
		if strings.Contains(out+errs, secret) {
			t.Fatalf("the password is in the output: %q %q", out, errs)
		}
	})
	t.Run("no terminal: the first line of standard input", func(t *testing.T) {
		f := newSnapFake(t)
		code, out, errs := f.run("snapshot alice-a --image reg.test/me/env:v1 --username bob --detach", func(e *Env) {
			e.StdinLine = func() (string, error) { return secret, nil }
		})
		if code != ExitOK || !reflect.DeepEqual(f.seen(), []string{wantBody}) || strings.Contains(out+errs, secret) {
			t.Fatalf("stdin: %d %v %q %q", code, f.seen(), out, errs)
		}
		// --password-stdin forces that path on a terminal too
		f = newSnapFake(t)
		var asked []ask
		code, _, _ = f.run("snapshot alice-a --image reg.test/me/env:v1 --username bob --password-stdin --detach", func(e *Env) {
			prompts(&asked)(e)
			e.StdinLine = func() (string, error) { return secret, nil }
		})
		if code != ExitOK || len(asked) != 0 || !reflect.DeepEqual(f.seen(), []string{wantBody}) {
			t.Fatalf("--password-stdin: %d %+v %v", code, asked, f.seen())
		}
	})
	t.Run("PUSH_AUTH_REQUIRED on a terminal: ask, then retry once", func(t *testing.T) {
		f := newSnapFake(t)
		var asked []ask
		code, _, errs := f.run("snapshot alice-a --image reg.test/me/env:v1 --detach", prompts(&asked, "bob", secret))
		if code != ExitOK || len(asked) != 2 || !asked[0].echo || asked[1].echo {
			t.Fatalf("retry: %d %+v %q", code, asked, errs)
		}
		if got := f.seen(); !reflect.DeepEqual(got, []string{`POST /api/devpods/alice-a/snapshots {"image":"reg.test/me/env:v1"}`, wantBody}) {
			t.Fatalf("requests %v", got)
		}
	})
	t.Run("PUSH_AUTH_REQUIRED without a terminal: say what is missing", func(t *testing.T) {
		f := newSnapFake(t)
		code, _, errs := f.run("snapshot alice-a --image reg.test/me/env:v1 --detach", nil)
		if code != ExitAPI || !strings.Contains(errs, "--username") || !strings.Contains(errs, "PUSH_AUTH_REQUIRED") || len(f.seen()) != 1 {
			t.Fatalf("no tty: %d %q %v", code, errs, f.seen())
		}
	})
	t.Run("an address under the default path sends no credentials", func(t *testing.T) {
		f := newSnapFake(t)
		var asked []ask
		code, _, _ := f.run("snapshot alice-a --image harbor.test/devpod-users/alice/a:v2 --detach", prompts(&asked))
		if code != ExitOK || len(asked) != 0 || !reflect.DeepEqual(f.seen(), []string{`POST /api/devpods/alice-a/snapshots {"image":"harbor.test/devpod-users/alice/a:v2"}`}) {
			t.Fatalf("default path: %d %v", code, f.seen())
		}
	})
	t.Run("usage: credentials need --image; stdin needs --username; there is no --password", func(t *testing.T) {
		f := newSnapFake(t)
		for _, line := range []string{
			"snapshot alice-a --username bob",
			"snapshot alice-a --password-stdin",
			"snapshot alice-a --image reg.test/x:1 --password-stdin",
			"snapshot alice-a --image reg.test/x:1 --password hunter2",
		} {
			if code, _, _ := f.run(line, nil); code != ExitUsage || len(f.seen()) != 0 {
				t.Errorf("%q: %d %v", line, code, f.seen())
			}
		}
	})

	t.Run("over SSH: typed at a terminal it is not echoed, and never logged", func(t *testing.T) {
		f := newSnapFake(t)
		var logs syncBuffer
		cl := sshRig(t, &Server{API: NewAPI(f.srv.URL, "k3y"), Log: slog.New(slog.NewTextHandler(&logs, nil))})
		s, _ := cl.NewSession()
		defer s.Close()
		in, _ := s.StdinPipe()
		var o syncBuffer
		s.Stdout = &o
		if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
		if err := s.Start("snapshot alice-a --image reg.test/me/env:v1 --detach"); err != nil {
			t.Fatal(err)
		}
		wait := func(what string) {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(o.String(), what) {
				if time.Now().After(deadline) {
					t.Fatalf("no %q in %q", what, o.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		wait("仓库用户名")
		_, _ = io.WriteString(in, "bob\r")
		wait("仓库密码")
		_, _ = io.WriteString(in, secret+"\r")
		if code := exitCode(s.Wait()); code != ExitOK {
			t.Fatalf("exit %d %q", code, o.String())
		}
		if !strings.Contains(o.String(), "bob") || strings.Contains(o.String(), secret) {
			t.Fatalf("echo: the user name should be echoed, the password not: %q", o.String())
		}
		if f.count(wantBody) != 1 {
			t.Fatalf("requests %v", f.seen())
		}
		if !strings.Contains(logs.String(), "cli_command") || strings.Contains(logs.String(), secret) {
			t.Fatalf("log: %q", logs.String())
		}
	})
	t.Run("over SSH without a terminal: piped in", func(t *testing.T) {
		f := newSnapFake(t)
		var logs syncBuffer
		cl := sshRig(t, &Server{API: NewAPI(f.srv.URL, "k3y"), Log: slog.New(slog.NewTextHandler(&logs, nil))})
		s, _ := cl.NewSession()
		defer s.Close()
		s.Stdin = strings.NewReader(secret + "\n")
		var o, e bytes.Buffer
		s.Stdout, s.Stderr = &o, &e
		if code := exitCode(s.Run("snapshot alice-a --image reg.test/me/env:v1 --username bob --detach")); code != ExitOK || f.count(wantBody) != 1 {
			t.Fatalf("piped: %d %q %v", code, e.String(), f.seen())
		}
		if strings.Contains(o.String()+e.String()+logs.String(), secret) {
			t.Fatal("the password leaked into output or logs")
		}
	})
}

func TestRedactArgs(t *testing.T) {
	got := strings.Join(redactArgs([]string{"snapshot", "a", "--password", "hunter2", "--token=abc", "--username", "bob", "--password-stdin", "--detach"}), " ")
	if got != "snapshot a --password <redacted> --token=<redacted> --username bob --password-stdin --detach" {
		t.Fatalf("redacted: %q", got)
	}
}
