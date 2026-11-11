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

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/gateway"
	"github.com/mrhaoxx/devpod/internal/gatewaycli"
)

// The DevPod CLI over SSH: `ssh <cli-user>@gateway <command>` runs
// commands against the console's REST API as the caller (docs/cli.md).
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
	srv  *gatewaycli.Server
}

func (f *cliFlags) build() (*cliDeps, error) {
	if f.user == "" || f.apiURL == "" {
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
func cliAuth(ctx context.Context, authn *gateway.Authenticator, id uint64, meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error, bool) {
	if cli == nil {
		return nil, nil, false
	}
	named, isCLI := gateway.ParseCLILogin(cli.user, meta.User())
	if !isCLI {
		return nil, nil, false
	}
	user, ap, err := authn.AuthenticateCLI(ctx, named, key)
	if err != nil {
		reason := classifyAuthErr(err)
		slog.Warn("cli_auth_rejected", "id", id, "login", meta.User(), "reason", err)
		gateway.AuthFailure(slog.Default(), reason, ap.Kind, ap.Alias, ssh.FingerprintSHA256(key), named, "", ap.LastSourceErr)
		gateway.AuthFailuresTotal.WithLabelValues(reason, ap.Kind).Inc()
		return nil, err, true
	}
	slog.Info("cli_auth_ok", "id", id, "user", user, "auth_path", ap.Kind, "proxy_alias", ap.Alias)
	return &ssh.Permissions{Extensions: map[string]string{
		cliExtension: "1", "devpod.io/user": user, "devpod.io/cli-auth": ap.Kind, "devpod.io/cli-alias": ap.Alias,
	}}, nil, true
}

// serveCLI runs an authenticated CLI connection; it reports whether the
// connection was one.
func serveCLI(ctx context.Context, conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) bool {
	ext := conn.Permissions.Extensions
	if cli == nil || ext[cliExtension] != "1" {
		return false
	}
	cli.srv.Serve(ctx, conn, chans, reqs, gatewaycli.Meta{
		User: ext["devpod.io/user"], Remote: conn.RemoteAddr().String(), AuthKind: ext["devpod.io/cli-auth"], Alias: ext["devpod.io/cli-alias"],
	})
	return true
}
