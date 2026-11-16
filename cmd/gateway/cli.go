// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/audit"
	"github.com/mrhaoxx/devpod/internal/gateway"
	"github.com/mrhaoxx/devpod/internal/gatewaycli"
	"github.com/mrhaoxx/devpod/internal/gwlink"
)

// The DevPod CLI over SSH: `ssh <user>+<cli-user>@gateway <command>`.
// A gateway of several clusters hands it to the console, which runs it
// (GET /gateway/v1/cli): the gateway signs the user in, logs in to the
// console's CLI as them with its internal key, and proxies and records
// the session as it does one of a DevPod's. A gateway of one cluster runs
// it itself, against the console's REST API with a shared key
// (--cli-api-url, --cli-api-key-file).
type cliFlags struct {
	user, apiURL, keyFile string
	maxConns              int
}

func addCLIFlags(fs *flag.FlagSet) *cliFlags {
	f := &cliFlags{}
	fs.StringVar(&f.user, "cli-user", "devpod", "SSH login name of the DevPod CLI (ssh <name>@gateway <command>); empty disables it. The name is then not usable as a DevPod user name")
	fs.StringVar(&f.apiURL, "cli-api-url", "", "console (webui) base URL the CLI calls, e.g. http://devpod-webui.devpod-system.svc:80; the CLI is off without it")
	fs.StringVar(&f.keyFile, "cli-api-key-file", "", "key shared with the console that lets the gateway call its API on a user's behalf")
	fs.IntVar(&f.maxConns, "cli-max-conns", 64, "concurrent CLI connections on this gateway")
	return f
}

// cliDeps is the enabled CLI; nil when it is off.
type cliDeps struct {
	user string
	srv  *gatewaycli.Server // run here (a gateway of one cluster)
	hub  *hub               // handed to the console (a gateway of several)
}

func (f *cliFlags) build(h *hub) (*cliDeps, error) {
	if f.user == "" {
		return nil, nil
	}
	if h != nil {
		return &cliDeps{user: f.user, hub: h}, nil
	}
	if f.apiURL == "" {
		return nil, nil
	}
	key, err := os.ReadFile(f.keyFile)
	if err != nil {
		return nil, fmt.Errorf("--cli-api-key-file: %w", err)
	}
	if key = bytes.TrimSpace(key); len(key) == 0 {
		return nil, errors.New("--cli-api-key-file is empty")
	}
	return &cliDeps{user: f.user, srv: &gatewaycli.Server{API: gatewaycli.NewAPI(f.apiURL, string(key)), MaxConns: f.maxConns}}, nil
}

// cli is set once at startup, before the listener accepts.
var cli *cliDeps

const cliExtension = "devpod.io/cli"

// cliAuth handles the public-key step of a CLI login; ok is false when
// the login is an ordinary DevPod login.
func cliAuth(ctx context.Context, authn *gateway.Authenticator, id uint64, meta ssh.ConnMetadata, key ssh.PublicKey) (_ *ssh.Permissions, ok bool, _ error) {
	if cli == nil {
		return nil, false, nil
	}
	named, isCLI := gateway.ParseCLILogin(cli.user, meta.User())
	if !isCLI {
		return nil, false, nil
	}
	user, ap, err := authn.AuthenticateCLI(ctx, named, key)
	if err != nil {
		reason := classifyAuthErr(err)
		slog.Warn("cli_auth_rejected", "id", id, "login", meta.User(), "reason", err)
		gateway.AuthFailure(slog.Default(), reason, ap.Kind, ap.Alias, ssh.FingerprintSHA256(key), named, "", ap.LastSourceErr)
		gateway.AuthFailuresTotal.WithLabelValues(reason, ap.Kind).Inc()
		return nil, true, err
	}
	slog.Info("cli_auth_ok", "id", id, "user", user, "auth_path", ap.Kind, "proxy_alias", ap.Alias)
	return &ssh.Permissions{Extensions: map[string]string{
		cliExtension: "1", "devpod.io/user": user, "devpod.io/cli-auth": ap.Kind, "devpod.io/cli-alias": ap.Alias,
		"devpod.io/pubkey-fp": ssh.FingerprintSHA256(key), "devpod.io/pubkey": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
	}}, true, nil
}

// serveCLI runs an authenticated CLI connection; it reports whether the
// connection was one.
func serveCLI(ctx context.Context, conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, dialer *gateway.Dialer, ad *auditDeps) bool {
	ext := conn.Permissions.Extensions
	if cli == nil || ext[cliExtension] != "1" {
		return false
	}
	if cli.hub != nil {
		proxyCLI(ctx, conn, chans, reqs, dialer, ad)
		return true
	}
	cli.srv.Serve(ctx, conn, chans, reqs, gatewaycli.Meta{
		User: ext["devpod.io/user"], Remote: conn.RemoteAddr().String(), AuthKind: ext["devpod.io/cli-auth"], Alias: ext["devpod.io/cli-alias"],
	})
	return true
}

// proxyCLI hands a signed-in CLI connection to the console's CLI and
// proxies it there, recorded as a DevPod's session is (no DevPod named).
func proxyCLI(ctx context.Context, conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, dialer *gateway.Dialer, ad *auditDeps) {
	ext := conn.Permissions.Extensions
	ap := gateway.AuthPath{User: ext["devpod.io/user"], Kind: ext["devpod.io/cli-auth"], Alias: ext["devpod.io/cli-alias"]}
	clientIP := conn.RemoteAddr().String()
	fp := ext["devpod.io/pubkey-fp"]
	start := time.Now()
	id := audit.NewSessionID(start)
	gateway.SessionOpen(slog.Default(), id, ap, clientIP, fp)
	hooks, sess := ad.startSession(ctx, audit.SessionMeta{
		ID: id, User: ap.User, Owner: ap.User, ClientIP: clientIP, PubkeyFP: fp, Pubkey: ext["devpod.io/pubkey"],
		AuthKind: ap.Kind, ProxyAlias: ap.Alias, OpenedAt: start, Gateway: ad.gatewayName,
	})
	stats := &gateway.ProxyStats{}
	if sess != nil {
		sess.SetBytes(func() (int64, int64) { return stats.BytesClientToBackend.Load(), stats.BytesBackendToClient.Load() })
	}
	reason := "client_disconnect"
	defer func() {
		gateway.SessionClose(slog.Default(), id, time.Since(start), stats.BytesClientToBackend.Load(), stats.BytesBackendToClient.Load(), reason)
		hooks.Close(reason, stats.BytesClientToBackend.Load(), stats.BytesBackendToClient.Load())
	}()
	raw, err := cli.hub.link.DialCLI(ctx, gwlink.CLIRequest{User: ap.User, Remote: clientIP, AuthKind: ap.Kind, Alias: ap.Alias})
	var back ssh.Conn
	var backChans <-chan ssh.NewChannel
	var backReqs <-chan *ssh.Request
	if err == nil {
		back, backChans, backReqs, err = dialer.Handshake(raw, "console", ap.User)
	}
	if err != nil {
		slog.Warn("cli_console_unreachable", "user", ap.User, "err", err)
		reason = "dial_failed"
		refuseWith(conn, chans, reqs, "the DevPod console cannot be reached right now; try again in a moment\n")
		return
	}
	defer back.Close()
	defer gateway.OpenSessions.Add(gwlink.Session{ID: id, User: ap.User, Client: clientIP, Kind: ap.Kind, PubkeyFP: fp, Since: start}, stats)()
	if err := gateway.Proxy(conn, chans, reqs, back, backChans, backReqs, stats, hooks); err != nil && !isCleanDisconnect(err) {
		reason = "proxy_error"
	}
	if sess != nil && sess.KillReason() != "" {
		reason = sess.KillReason()
	}
}
