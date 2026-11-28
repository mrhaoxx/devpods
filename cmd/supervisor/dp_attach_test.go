// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/unix"
)

// fakeConsole answers dp: /dp/v1/cli with the frames cli gives for the
// call's argv; /dp/v1/attach (unless refuse) attaches to a fake rank,
// which writes "out:<rank>" and "err:<rank>", echoes its stdin, keeps what
// it is sent, and ends at the end of its stdin.
type fakeConsole struct {
	cli    func(argv []string) []string
	refuse bool
	mu     sync.Mutex
	hdr    http.Header // the last /dp/v1/cli call's
	got    map[int][][]byte
	ttys   map[int]string
}

func (f *fakeConsole) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-DevPod-UID") != "uid-1" {
		http.Error(w, "not a DevPod's dp", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/dp/v1/cli":
		var req struct{ Argv []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.hdr = r.Header.Clone()
		f.mu.Unlock()
		for _, fr := range f.cli(req.Argv) {
			fmt.Fprintln(w, fr)
			w.(http.Flusher).Flush()
		}
	case "/dp/v1/attach":
		if f.refuse {
			http.Error(w, "this rank is not running", http.StatusConflict)
			return
		}
		if r.URL.Query().Get("probe") == "1" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		rank, _ := strconv.Atoi(r.URL.Query().Get("rank"))
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageBinary, []byte(fmt.Sprintf("\x01out:%d\n", rank)))
		_ = c.Write(ctx, websocket.MessageBinary, []byte(fmt.Sprintf("\x02err:%d\n", rank)))
		for {
			typ, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			f.mu.Lock()
			if typ == websocket.MessageText {
				if f.ttys == nil {
					f.ttys = map[int]string{}
				}
				f.ttys[rank] += string(msg)
			} else {
				if f.got == nil {
					f.got = map[int][][]byte{}
				}
				f.got[rank] = append(f.got[rank], msg)
			}
			f.mu.Unlock()
			if typ == websocket.MessageText && strings.Contains(string(msg), `"eof"`) {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
			if typ == websocket.MessageBinary && !(len(msg) == 1 && msg[0] == 0) {
				_ = c.Write(ctx, websocket.MessageBinary, append([]byte{1}, msg...))
			}
		}
	}
}

func quickRetries(t *testing.T) {
	saved, savedWait := attachTries, attachRetry
	attachTries, attachRetry = 3, 10*time.Millisecond
	t.Cleanup(func() { attachTries, attachRetry = saved, savedWait })
}

func TestDPAttachable(t *testing.T) {
	for argv, want := range map[string]bool{
		"srun bash":                 true,
		"job submit -- python t.py": true,
		"job submit -d -- true":     false,
		"job submit --detach -- x":  false,
		"job submit --dry-run -- x": false,
		"job list":                  false,
		"sbatch x.sh":               false,
	} {
		if got := attachable(strings.Fields(argv)); got != want {
			t.Errorf("%s: %v", argv, got)
		}
	}
	if dpEnv([]string{"srun", "bash"}, []string{"OMP_NUM_THREADS=8"})["OMP_NUM_THREADS"] != "8" {
		t.Error("srun takes the shell's environment along too")
	}
}

func TestDPProbe(t *testing.T) {
	ok := httptest.NewServer(&fakeConsole{})
	defer ok.Close()
	if !probeAttach(ok.URL, "uid-1", "tok") {
		t.Fatal("a console that attaches said it does not")
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "POST only", http.StatusMethodNotAllowed) // an agent of before
	}))
	defer old.Close()
	if probeAttach(old.URL, "uid-1", "tok") {
		t.Fatal("an old agent's way said it attaches")
	}
}

// Told to attach to a two-rank job: each rank's gate gets the sentinel,
// rank 0 the caller's stdin (then its end), rank 1 its end at once; what
// each writes comes out marked by rank, stdout and stderr apart; the code
// is the console's.
func TestDPCallAttachesStreams(t *testing.T) {
	quickRetries(t)
	f := &fakeConsole{cli: func([]string) []string {
		return []string{`{"e":"submitted job abc12\n"}`, `{"a":{"job":"abc12","nodes":2}}`, `{"x":3}`}
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	var out, errw syncBuf
	code := dpCall(srv.URL, "uid-1", "tok", []string{"job", "submit", "-N", "2", "--", "cat"}, "/", nil, nil,
		dpIO{in: strings.NewReader("hello\n"), out: &out, err: &errw, attach: true})
	if code != 3 {
		t.Fatalf("code %d, err %q", code, errw.String())
	}
	for _, want := range []string{"0: out:0\n", "1: out:1\n", "0: hello\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout %q, missing %q", out.String(), want)
		}
	}
	for _, want := range []string{"submitted job abc12", "0: err:0\n", "1: err:1\n"} {
		if !strings.Contains(errw.String(), want) {
			t.Fatalf("stderr %q, missing %q", errw.String(), want)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.got[0]; len(g) < 2 || !bytes.Equal(g[0], []byte{0}) || string(bytes.Join(g[1:], nil)) != "hello\n" {
		t.Fatalf("rank 0 was sent %q", g)
	}
	if g := f.got[1]; len(g) != 1 || !bytes.Equal(g[0], []byte{0}) {
		t.Fatalf("rank 1 was sent %q", g)
	}
}

// On a terminal: rank 0 alone, its output as it comes, unmarked.
func TestDPCallAttachesTTY(t *testing.T) {
	quickRetries(t)
	f := &fakeConsole{cli: func([]string) []string {
		return []string{`{"a":{"job":"abc12","nodes":2,"tty":true}}`, `{"e":"job abc12: succeeded\n"}`, `{"x":0}`}
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	var out, errw syncBuf
	code := dpCall(srv.URL, "uid-1", "tok", []string{"srun", "--pty", "bash"}, "/", nil, nil,
		dpIO{in: strings.NewReader("exit\n"), out: &out, err: &errw, attach: true})
	if code != 0 || !strings.Contains(out.String(), "out:0\nerr:0\n") || strings.Contains(out.String(), "out:1") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errw.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.got[0]; len(g) == 0 || bytes.Equal(g[0], []byte{0}) {
		t.Fatalf("a terminal has no sentinel byte: %q", g)
	}
}

// A rank that cannot be attached to is followed instead.
func TestDPAttachFallsBack(t *testing.T) {
	quickRetries(t)
	f := &fakeConsole{refuse: true, cli: func(argv []string) []string {
		if len(argv) > 1 && argv[1] == "logs" {
			return []string{`{"o":"from the log of ` + argv[len(argv)-2] + ` ` + argv[len(argv)-1] + `\n"}`, `{"x":0}`}
		}
		return []string{`{"a":{"job":"abc12","nodes":1}}`, `{"x":0}`}
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	var out, errw syncBuf
	code := dpCall(srv.URL, "uid-1", "tok", []string{"job", "submit", "--", "true"}, "/", nil, nil, dpIO{in: strings.NewReader(""), out: &out, err: &errw, attach: true})
	if code != 0 || !strings.Contains(out.String(), "from the log of 0 abc12") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errw.String())
	}
}

// syncBuf: a buffer written from several goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// On a real terminal: its size goes first (the rank's gate opens on it) —
// 80×24 when the terminal says none — and the terminal is raw meanwhile.
func TestDPAttachTTYSize(t *testing.T) {
	quickRetries(t)
	for _, c := range []struct {
		cols, rows uint16
		want       string
	}{{0, 0, `"cols":80,"rows":24`}, {132, 43, `"cols":132,"rows":43`}} {
		master, slave, err := openPTY()
		if err != nil {
			t.Skipf("no pty here: %v", err)
		}
		if c.cols > 0 {
			if err := setWinsize(slave, &unix.Winsize{Col: c.cols, Row: c.rows}); err != nil {
				t.Fatal(err)
			}
		}
		f := &fakeConsole{cli: func([]string) []string {
			return []string{`{"a":{"job":"abc12","nodes":1,"tty":true}}`, `{"x":0}`}
		}}
		srv := httptest.NewServer(f)
		var out, errw syncBuf
		code := dpCall(srv.URL, "uid-1", "tok", []string{"srun", "--pty", "bash"}, "/", nil, nil,
			dpIO{in: strings.NewReader("exit\n"), out: &out, err: &errw, attach: true, tty: true, fd: int(slave.Fd()), term: "xterm-256color"})
		srv.Close()
		f.mu.Lock()
		sent, hdr := f.ttys[0], f.hdr
		f.mu.Unlock()
		if code != 0 || !strings.Contains(sent, c.want) {
			t.Fatalf("size %dx%d: code %d, sent %q, err %q", c.cols, c.rows, code, sent, errw.String())
		}
		if hdr.Get("X-DP-TTY") != "1" || hdr.Get("X-DP-Term") != "xterm-256color" {
			t.Fatalf("the terminal was not said: %v", hdr)
		}
		if st, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS); err != nil || st.Lflag&unix.ECHO == 0 {
			t.Fatalf("the terminal is not restored after: %v", err)
		}
		_ = master.Close()
		_ = slave.Close()
	}
}

// recSink keeps what a rank is sent of the caller's stdin.
type recSink struct {
	mu   sync.Mutex
	got  string
	eofs int
}

func (r *recSink) data(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got += string(p)
	return nil
}

func (r *recSink) eof() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eofs++
}

func (r *recSink) seen() (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got, r.eofs
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(end) {
			t.Fatal("never")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The caller's stdin goes to rank 0 of the attempt attached now: a
// requeued job's next attempt takes it over (its terminal can be typed
// at); once stdin has ended, a rank 0 that comes after is told so at once.
func TestStdinRouter(t *testing.T) {
	pr, pw := io.Pipe()
	s := &stdinRouter{in: pr}
	first, second, third := &recSink{}, &recSink{}, &recSink{}
	s.attach(first)
	_, _ = pw.Write([]byte("one"))
	eventually(t, func() bool { got, _ := first.seen(); return got == "one" })
	s.attach(second)
	_, _ = pw.Write([]byte("two"))
	eventually(t, func() bool { got, _ := second.seen(); return got == "two" })
	_ = pw.Close()
	eventually(t, func() bool { _, eofs := second.seen(); return eofs == 1 })
	if got, eofs := first.seen(); got != "one" || eofs != 0 {
		t.Fatalf("the first attempt was sent %q and %d ends", got, eofs)
	}
	s.attach(third)
	if got, eofs := third.seen(); got != "" || eofs != 1 {
		t.Fatalf("after the end: %q, %d ends", got, eofs)
	}
}
