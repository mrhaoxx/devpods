// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gatewaycli"
)

// The DevPod CLI runs here, in the console: `ssh <user>+devpod@gateway`
// is signed in by the gateway, which then opens a WebSocket on the
// console's HTTP port (GET /gateway/v1/cli, signed as the gateway's, the
// user and where they came from in the signed address) and speaks SSH on
// it — logging in as that user with the key it signs into DevPods with,
// which the console trusts as a DevPod does. The gateway proxies and
// records the session as it does one of a DevPod's. The CLI answers by
// calling this console's own API over loopback (CLILoopbackKey).

// handleGatewayCLI: GET /gateway/v1/cli?user=&remote=&auth=&alias=.
func (s *Server) handleGatewayCLI(w http.ResponseWriter, r *http.Request) {
	name, _, ok := s.asGateway(w, r)
	if !ok {
		return
	}
	if s.CLI == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	user := q.Get("user")
	if !shareNameRE.MatchString(user) {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "not a user name", nil)
		return
	}
	var g devpodv1alpha1.Gateway
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: name}, &g); err != nil {
		s.writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	gwKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(g.Status.InternalKey))
	if err != nil {
		s.writeErr(w, http.StatusConflict, "NO_KEY", "the gateway has not said which key it signs in with", nil)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(-1)
	conn := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
	defer func() { _ = conn.Close() }()
	host, err := s.cliHost()
	if err != nil {
		slog.Error("cli_host_key", "err", err)
		return
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() != user || !bytes.Equal(key.Marshal(), gwKey.Marshal()) {
				return nil, errors.New("not the gateway's key, or not the user it named")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(host)
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		slog.Warn("cli_session_refused", "gateway", name, "user", user, "err", err)
		return
	}
	defer func() { _ = sc.Close() }()
	slog.Info("cli_session", "gateway", name, "user", user, "remote", q.Get("remote"))
	s.CLI.Serve(r.Context(), sc, chans, reqs, gatewaycli.Meta{User: user, Remote: q.Get("remote"), AuthKind: q.Get("auth"), Alias: q.Get("alias")})
}

// cliHost is the host key of the console's CLI: made once a process. The
// gateway knows the console by the signed request's answer, not by it.
func (s *Server) cliHost() (ssh.Signer, error) {
	s.cliHostOnce.Do(func() {
		var priv ed25519.PrivateKey
		if _, priv, s.cliHostErr = ed25519.GenerateKey(rand.Reader); s.cliHostErr == nil {
			s.cliHostKey, s.cliHostErr = ssh.NewSignerFromKey(priv)
		}
		if s.cliHostErr != nil {
			s.cliHostErr = fmt.Errorf("cli host key: %w", s.cliHostErr)
		}
	})
	return s.cliHostKey, s.cliHostErr
}

// fromLoopback: the request came from this machine (the console's own CLI).
func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}
