// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package kattach

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"k8s.io/client-go/rest"
)

// apiServer is a fake API server's attach: it records what it is asked and
// what it is sent, echoes stdin back as stdout (and "!"+it as stderr), and
// once stdin is closed says the attach ended (the error channel's status)
// and closes.
type apiServer struct {
	query  chan string
	frames chan []byte
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.query <- r.URL.Path + "?" + r.URL.RawQuery
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Protocol}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	for {
		_, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		a.frames <- msg
		switch {
		case len(msg) > 0 && msg[0] == Stdin:
			_ = c.Write(ctx, websocket.MessageBinary, append([]byte{Stdout}, msg[1:]...))
			_ = c.Write(ctx, websocket.MessageBinary, append([]byte{Stderr, '!'}, msg[1:]...))
		case len(msg) == 2 && msg[0] == 255 && msg[1] == Stdin:
			_ = c.Write(ctx, websocket.MessageBinary, append([]byte{Error}, []byte(`{"status":"Success"}`)...))
			_ = c.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

func TestAttach(t *testing.T) {
	a := &apiServer{query: make(chan string, 1), frames: make(chan []byte, 16)}
	srv := httptest.NewServer(a)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, &rest.Config{Host: srv.URL}, "devpods", "alice-abc12-0", "dev", true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if q := <-a.query; q != "/api/v1/namespaces/devpods/pods/alice-abc12-0/attach?container=dev&stderr=false&stdin=true&stdout=true&tty=true" {
		t.Fatalf("asked %s", q)
	}
	if err := c.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if f := <-a.frames; f[0] != Resize {
		t.Fatalf("resize frame %v", f)
	} else {
		var sz struct{ Width, Height int }
		if json.Unmarshal(f[1:], &sz) != nil || sz.Width != 120 || sz.Height != 40 {
			t.Fatalf("resize %s", f[1:])
		}
	}
	if err := c.Stdin([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if f := <-a.frames; string(f) != "\x00hi" {
		t.Fatalf("stdin frame %q", f)
	}
	for _, want := range []struct {
		ch   byte
		data string
	}{{Stdout, "hi"}, {Stderr, "!hi"}} {
		ch, data, err := c.Read(ctx)
		if err != nil || ch != want.ch || string(data) != want.data {
			t.Fatalf("read %d %q %v, want %d %q", ch, data, err, want.ch, want.data)
		}
	}
	if err := c.CloseStdin(); err != nil {
		t.Fatal(err)
	}
	if f := <-a.frames; len(f) != 2 || f[0] != 255 || f[1] != Stdin {
		t.Fatalf("close frame %v", f)
	}
	if ch, data, err := c.Read(ctx); err != nil || ch != Error || string(data) != `{"status":"Success"}` {
		t.Fatalf("the end's status: %d %q %v", ch, data, err)
	}
	if _, _, err := c.Read(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("after the end: %v", err)
	}
}

// Without a terminal, stderr is asked for apart; an API server that will
// not attach is said so, with its words.
func TestAttachNoTTYAndRefusal(t *testing.T) {
	a := &apiServer{query: make(chan string, 1), frames: make(chan []byte, 16)}
	srv := httptest.NewServer(a)
	defer srv.Close()
	ctx := context.Background()
	c, err := Dial(ctx, &rest.Config{Host: srv.URL}, "devpods", "p", "dev", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if q := <-a.query; q != "/api/v1/namespaces/devpods/pods/p/attach?container=dev&stderr=true&stdin=true&stdout=true&tty=false" {
		t.Fatalf("asked %s", q)
	}
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `pods "p" is forbidden: cannot create resource "pods/attach"`, http.StatusForbidden)
	}))
	defer refuse.Close()
	if _, err := Dial(ctx, &rest.Config{Host: refuse.URL}, "devpods", "p", "dev", false); err == nil || !contains(err.Error(), "pods/attach") {
		t.Fatalf("refused: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
