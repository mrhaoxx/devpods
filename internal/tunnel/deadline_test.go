// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package tunnel_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mrhaoxx/devpod/internal/tunnel"
)

// An HTTP server on a stream over a WebSocket keeps it for the next
// request: it moves the stream's read deadline after every request (to stop
// its read in the background), which must not end the stream.
func TestStreamKeepsHTTPConnection(t *testing.T) {
	ln := tunnel.NewListener()
	defer ln.Close()
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	}()
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
		ln.Offer(serveSide(nc))
		<-r.Context().Done()
	}))
	defer ws.Close()

	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(ws.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	one := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	dialed := false
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		// the one stream there is: every request must go on it
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if dialed {
				return nil, errors.New("the stream is gone")
			}
			dialed = true
			return one, nil
		},
	}}
	for i := 0; i < 3; i++ {
		resp, err := hc.Get("http://stream/")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(b) != "ok" {
			t.Fatalf("request %d: %q", i, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// serveSide is what the serving end gets of its stream.
func serveSide(nc net.Conn) net.Conn { return tunnel.DeadlineSafe(nc) }
