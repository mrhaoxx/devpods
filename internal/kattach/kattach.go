// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package kattach attaches to a running container through the Kubernetes
// API's WebSocket stream protocol (v5.channel.k8s.io): what the container
// writes, its stdout and stderr apart (one stream with a terminal); its
// stdin, and the end of it; its terminal's size. It is what kubectl attach
// does, over a plain HTTP client — so it goes wherever the console reaches
// an API server (its own, or a cluster's agent).
package kattach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"k8s.io/client-go/rest"
)

// Protocol is the stream protocol spoken: v5 can end one stream (stdin)
// and keep the others.
const Protocol = "v5.channel.k8s.io"

// The protocol's channels: each frame's first byte.
const (
	Stdin  byte = 0
	Stdout byte = 1
	Stderr byte = 2
	// Error carries, last, how the attach ended (a metav1.Status as JSON).
	Error  byte = 3
	Resize byte = 4
	// closeStream, then a channel: that stream has ended (v5).
	closeStream byte = 255
)

// maxFrame bounds a frame read: the API server sends output in pieces of
// at most 32 KiB, a status is small.
const maxFrame = 4 << 20

// Conn is one attach.
type Conn struct {
	ws  *websocket.Conn
	wmu sync.Mutex
}

// Dial attaches to container of pod in namespace ns through cfg, with a
// terminal (tty: one output stream) or without.
func Dial(ctx context.Context, cfg *rest.Config, ns, pod, container string, tty bool) (*Conn, error) {
	u, err := attachURL(cfg.Host, ns, pod, container, tty)
	if err != nil {
		return nil, err
	}
	rt, err := transportFor(cfg)
	if err != nil {
		return nil, err
	}
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: &http.Client{Transport: rt}, Subprotocols: []string{Protocol}})
	if err != nil {
		if resp != nil && resp.Body != nil {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if s := strings.TrimSpace(string(msg)); s != "" {
				return nil, fmt.Errorf("attach %s/%s: %s (%d)", ns, pod, s, resp.StatusCode)
			}
		}
		return nil, fmt.Errorf("attach %s/%s: %w", ns, pod, err)
	}
	if ws.Subprotocol() != Protocol {
		_ = ws.Close(websocket.StatusProtocolError, "")
		return nil, fmt.Errorf("attach %s/%s: the API server does not speak %s", ns, pod, Protocol)
	}
	ws.SetReadLimit(maxFrame)
	return &Conn{ws: ws}, nil
}

// attachURL: the attach subresource of the pod at host, asked for stdin,
// stdout, and stderr apart unless there is a terminal.
func attachURL(host, ns, pod, container string, tty bool) (string, error) {
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	base, err := url.Parse(host)
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimRight(base.Path, "/") + fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/attach", url.PathEscape(ns), url.PathEscape(pod))
	q := url.Values{}
	q.Set("container", container)
	q.Set("stdin", "true")
	q.Set("stdout", "true")
	q.Set("stderr", fmt.Sprint(!tty))
	q.Set("tty", fmt.Sprint(tty))
	base.RawQuery = q.Encode()
	return base.String(), nil
}

// transportFor: cfg's way to its API server (its dialer, its TLS, its
// credentials) over HTTP/1.1 — an upgrade is not made over HTTP/2.
func transportFor(cfg *rest.Config) (http.RoundTripper, error) {
	tlsCfg, err := rest.TLSConfigFor(cfg)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		tlsCfg = tlsCfg.Clone()
		tlsCfg.NextProtos = []string{"http/1.1"}
	}
	tr := &http.Transport{TLSClientConfig: tlsCfg}
	if cfg.Dial != nil {
		tr.DialContext = cfg.Dial
	}
	return rest.HTTPWrappersForConfig(cfg, tr)
}

// write sends one frame.
func (c *Conn) write(frame []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.Write(context.Background(), websocket.MessageBinary, frame)
}

// Stdin writes p to the container's stdin.
func (c *Conn) Stdin(p []byte) error {
	return c.write(append([]byte{Stdin}, p...))
}

// CloseStdin ends the container's stdin (it reads EOF when its stdin is
// once-only).
func (c *Conn) CloseStdin() error {
	return c.write([]byte{closeStream, Stdin})
}

// Resize sets the container's terminal to cols × rows.
func (c *Conn) Resize(cols, rows uint16) error {
	b, _ := json.Marshal(struct{ Width, Height uint16 }{cols, rows})
	return c.write(append([]byte{Resize}, b...))
}

// Read is the next frame from the container: its channel (Stdout, Stderr,
// or Error: how the attach ended) and what it carries; io.EOF once the
// attach has ended.
func (c *Conn) Read(ctx context.Context) (byte, []byte, error) {
	for {
		_, msg, err := c.ws.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure || errors.Is(err, io.EOF) {
				return 0, nil, io.EOF
			}
			return 0, nil, err
		}
		if len(msg) == 0 || msg[0] == closeStream {
			continue
		}
		return msg[0], bytes.Clone(msg[1:]), nil
	}
}

// Close ends the attach (the container goes on).
func (c *Conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
