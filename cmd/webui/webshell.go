// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/mrhaoxx/devpod/internal/webui"
)

// webshellFlags configures the browser terminal (UI spec §5.12).
type webshellFlags struct {
	keyFile, gateway, hostKey string
	max, perUser              int
}

func addWebshellFlags(fs *flag.FlagSet) *webshellFlags {
	f := &webshellFlags{}
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "devpod-system"
	}
	fs.StringVar(&f.keyFile, "webshell-key-file", "", "SSH private key the gateway trusts as a proxy (GatewayConfig trustedProxyKeys); empty disables the web terminal")
	fs.StringVar(&f.gateway, "webshell-gateway", "devpod-gateway."+ns+".svc:22", "host:port of the SSH gateway")
	fs.StringVar(&f.hostKey, "webshell-gateway-host-key", "", "the gateway's host public key (authorized_keys form) to pin; empty accepts any")
	fs.IntVar(&f.max, "webshell-max", 64, "concurrent web terminals per webui replica")
	fs.IntVar(&f.perUser, "webshell-max-per-user", 8, "concurrent web terminals per user, per replica")
	return f
}

// build returns nil when the web terminal is off.
func (f *webshellFlags) build() (*webui.Webshell, error) {
	if f.keyFile == "" {
		return nil, nil
	}
	key, err := os.ReadFile(f.keyFile)
	if err != nil {
		return nil, fmt.Errorf("webshell key: %w", err)
	}
	ws, err := webui.NewWebshell(key, f.gateway, f.hostKey, f.max, f.perUser)
	if err != nil {
		return nil, err
	}
	// the line to list under GatewayConfig spec.trustedProxyKeys
	slog.Info("webshell_enabled", "gateway", f.gateway, "pubkey", ws.PublicKey())
	return ws, nil
}
