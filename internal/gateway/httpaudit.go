// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/httpauth"
)

// HTTPAudit records what the proxy serves (HTTPProxy.Audit; nil: nothing).
type HTTPAudit interface {
	// Admit says whether a request by user ("" anonymous) is served now
	// and whether it is recorded; serve is false while audit storage is
	// full under a fail-closed policy.
	Admit(ctx context.Context, user string) (serve, record bool)
	// Record keeps one finished request.
	Record(row store.HTTPRequestRow)
}

// record starts the row of a request to port of dp: it returns the writer
// to answer through (it counts the answer, and both directions once the
// connection is upgraded) and what records the request once it has ended,
// as the user it turned out to be.
func (p *HTTPProxy) record(w http.ResponseWriter, r *http.Request, dp *devpodv1alpha1.DevPod, cluster string, port int) (http.ResponseWriter, func(user string)) {
	start := p.Now()
	clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		clientIP = r.RemoteAddr
	}
	row := store.HTTPRequestRow{
		TS: start.UTC(), Cluster: cluster, DevPod: dp.Name, DevPodUID: string(dp.UID), Owner: dp.Spec.Owner,
		Port: uint16(port), Access: httpauth.Access(dp, port), Method: r.Method, Host: r.Host,
		Path: r.URL.Path, Query: r.URL.RawQuery, ClientIP: clientIP, ForwardedFor: r.Header.Get("X-Forwarded-For"),
		UserAgent: r.UserAgent(), RequestID: ulid.MustNew(ulid.Timestamp(start), ulid.DefaultEntropy()).String(),
	}
	cw := &countingWriter{ResponseWriter: w}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &countingBody{ReadCloser: r.Body, n: &cw.in}
	}
	return cw, func(user string) {
		row.User = user
		row.Status = uint16(cw.final())
		row.BytesIn, row.BytesOut = cw.in.Load(), cw.out.Load()
		row.Websocket = cw.upgraded
		row.DurationMS = uint32(p.Now().Sub(start) / time.Millisecond)
		p.Audit.Record(row)
	}
}

// countingWriter is the answer to one request as sent: its status and the
// bytes, both directions of a connection the proxy took over to upgrade.
type countingWriter struct {
	http.ResponseWriter
	status   int
	upgraded bool
	in, out  atomic.Uint64 // in: from the client (body, or an upgraded connection)
}

func (w *countingWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 { // 1xx are not the answer
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.out.Add(uint64(n))
	return n, err
}

// Flush: the proxy streams (FlushInterval -1).
func (w *countingWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

// Hijack hands the connection over counted. The proxy takes a connection
// over only to switch protocols: the answer is 101.
func (w *countingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	w.upgraded = true
	return &countingConn{Conn: conn, w: w}, brw, nil
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// final is the status the client got (200 when nothing was written).
func (w *countingWriter) final() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

type countingConn struct {
	net.Conn
	w *countingWriter
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.w.in.Add(uint64(n))
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.w.out.Add(uint64(n))
	return n, err
}

// countingBody counts the request body as the proxy reads it.
type countingBody struct {
	io.ReadCloser
	n *atomic.Uint64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(uint64(n))
	return n, err
}
