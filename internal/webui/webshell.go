// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Webshell is the browser terminal (UI spec §5.12): the webui holds an
// SSH key the gateway trusts as a proxy, connects to the gateway as the
// logged-in user and bridges the shell to a WebSocket. The session goes
// through the gateway like any other, so it is recorded, listed, can be
// watched live and can be cut by an admin; its source reads "webui".
type Webshell struct {
	signer  ssh.Signer
	addr    string        // gateway host:port
	hostKey ssh.PublicKey // nil: any host key (in-cluster Service)
	max     int
	perUser int
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)

	mu    sync.Mutex
	total int
	users map[string]int
}

const (
	webshellDialTimeout = 10 * time.Second
	webshellPing        = 30 * time.Second
	webshellReadLimit   = 1 << 20 // one browser frame: a paste
)

// NewWebshell: key is the PEM private key the gateway lists under
// trustedProxyKeys; hostKey (authorized_keys form, optional) pins the
// gateway's host key; at most max shells per replica, perUser per user.
func NewWebshell(key []byte, gatewayAddr, hostKey string, max, perUser int) (*Webshell, error) {
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("webshell key: %w", err)
	}
	w := &Webshell{signer: signer, addr: gatewayAddr, max: max, perUser: perUser, users: map[string]int{},
		dial: (&net.Dialer{Timeout: webshellDialTimeout}).DialContext}
	if strings.TrimSpace(hostKey) != "" {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostKey))
		if err != nil {
			return nil, fmt.Errorf("webshell gateway host key: %w", err)
		}
		w.hostKey = pk
	}
	return w, nil
}

// PublicKey is the authorized_keys line to register on the gateway.
func (w *Webshell) PublicKey() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(w.signer.PublicKey())))
}

func (w *Webshell) acquire(user string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.total >= w.max || w.users[user] >= w.perUser {
		return false
	}
	w.total++
	w.users[user]++
	return true
}

func (w *Webshell) release(user string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total--
	if w.users[user]--; w.users[user] <= 0 {
		delete(w.users, user)
	}
}

// webshellLogin is the gateway login "<user>+<pod>" for user on dp. The
// gateway resolves "<user>-<pod>" first and then the bare name, so the
// owner's prefix is dropped and anyone else names the DevPod in full.
func webshellLogin(user, devpod string) string {
	if rest, ok := strings.CutPrefix(devpod, user+"-"); ok && rest != "" {
		return user + "+" + rest
	}
	return user + "+" + devpod
}

// open connects to the gateway as user and starts a shell on a pty.
func (w *Webshell) open(ctx context.Context, user, devpod string, cols, rows int) (*ssh.Client, *ssh.Session, io.WriteCloser, io.Reader, io.Reader, error) {
	hk := ssh.InsecureIgnoreHostKey() //nolint:gosec // in-cluster Service; pinned when a host key is configured
	if w.hostKey != nil {
		hk = ssh.FixedHostKey(w.hostKey)
	}
	ctx, cancel := context.WithTimeout(ctx, webshellDialTimeout)
	defer cancel()
	conn, err := w.dial(ctx, "tcp", w.addr)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(webshellDialTimeout))
	cc, chans, reqs, err := ssh.NewClientConn(conn, w.addr, &ssh.ClientConfig{
		User: webshellLogin(user, devpod), Auth: []ssh.AuthMethod{ssh.PublicKeys(w.signer)}, HostKeyCallback: hk,
	})
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, chans, reqs)
	fail := func(err error) (*ssh.Client, *ssh.Session, io.WriteCloser, io.Reader, io.Reader, error) {
		_ = client.Close()
		return nil, nil, nil, nil, nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		return fail(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return fail(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return fail(err)
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{}); err != nil {
		return fail(err)
	}
	if err := sess.Shell(); err != nil {
		return fail(err)
	}
	return client, sess, stdin, stdout, stderr, nil
}

func termSize(q url.Values) (cols, rows int) {
	cols, _ = strconv.Atoi(q.Get("cols"))
	rows, _ = strconv.Atoi(q.Get("rows"))
	if cols < 2 || cols > 1000 {
		cols = 80
	}
	if rows < 2 || rows > 1000 {
		rows = 24
	}
	return cols, rows
}

// handleDevPodShell serves GET /api/devpods/{name}/shell as a WebSocket:
// binary frames carry terminal bytes both ways; a text frame is a JSON
// control message, {"type":"resize","cols":N,"rows":N}. With ?probe=1 it
// answers 204 or the reason a shell cannot be opened, without opening one.
func (s *Server) handleDevPodShell(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	ws := s.Webshell
	if ws == nil {
		s.writeErr(w, http.StatusNotFound, "WEBSHELL_DISABLED", "the web terminal is not enabled", nil)
		return
	}
	var dp devpodv1alpha1.DevPod
	err := s.Client.Get(r.Context(), types.NamespacedName{Name: r.PathValue("name"), Namespace: s.NS}, &dp)
	// the gateway's rule: the owner and collaborators, admins included
	allowed := err == nil && (dp.Spec.Owner == sess.User || contains(dp.Spec.Collaborators, sess.User))
	switch {
	case err == nil && !allowed && sess.Admin:
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "only the owner and collaborators can open a shell; add yourself as a collaborator first", nil)
		return
	case err != nil || !allowed:
		s.writeErr(w, http.StatusNotFound, "NOT_FOUND", "devpod not found", nil)
		return
	}
	if dp.Status.Phase != devpodv1alpha1.DevPodRunning {
		s.writeErr(w, http.StatusConflict, "NOT_RUNNING", "the DevPod is not running", nil)
		return
	}
	// ?probe=1 only answers "could I open one": a browser cannot read
	// the status of a failed WebSocket handshake, so the page asks here
	if r.URL.Query().Get("probe") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// a WebSocket upgrade is a GET: the mutation Origin check does not
	// cover it, and a cross-site page must not open a shell with the
	// user's cookie
	if o := r.Header.Get("Origin"); s.Origin != "" && o != s.Origin {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "cross-origin request", nil)
		return
	}
	if !ws.acquire(sess.User) {
		s.writeErr(w, http.StatusTooManyRequests, "TOO_MANY_SHELLS", "too many web terminals are open; close one and try again", nil)
		return
	}
	defer ws.release(sess.User)

	cols, rows := termSize(r.URL.Query())
	client, shell, stdin, stdout, stderr, err := ws.open(r.Context(), sess.User, dp.Name, cols, rows)
	if err != nil {
		s.writeErr(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE", "the SSH gateway refused the connection: "+err.Error(), nil)
		return
	}
	defer client.Close()

	opts := &websocket.AcceptOptions{}
	if s.Origin == "" {
		opts.InsecureSkipVerify = true // dev / tests: no configured origin to hold requests to
	} else if u, err := url.Parse(s.Origin); err == nil {
		opts.OriginPatterns = []string{u.Host}
	}
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		return // Accept wrote the response
	}
	c.SetReadLimit(webshellReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var wg sync.WaitGroup
	for _, src := range []io.Reader{stdout, stderr} {
		wg.Add(1)
		go func(src io.Reader) {
			defer wg.Done()
			buf := make([]byte, 32<<10)
			for {
				n, err := src.Read(buf)
				if n > 0 && c.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					cancel()
					return
				}
				if err != nil {
					return
				}
			}
		}(src)
	}
	go func() { // browser → shell
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := stdin.Write(data); err != nil {
					return
				}
				continue
			}
			var m struct {
				Type       string `json:"type"`
				Cols, Rows int
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols >= 2 && m.Rows >= 2 && m.Cols <= 1000 && m.Rows <= 1000 {
				_ = shell.WindowChange(m.Rows, m.Cols)
			}
		}
	}()
	go func() { // keep proxies from idling the socket out
		t := time.NewTicker(webshellPing)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pc := context.WithTimeout(ctx, webshellPing)
				err := c.Ping(pctx)
				pc()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- shell.Wait() }()
	reason := "connection closed"
	select {
	case err := <-done:
		wg.Wait() // flush what the shell printed last
		reason = "shell exited"
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			reason = fmt.Sprintf("shell exited with status %d", ee.ExitStatus())
		} else if err != nil {
			reason = "session ended: " + err.Error()
		}
	case <-ctx.Done():
	}
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = c.Close(websocket.StatusNormalClosure, reason)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
