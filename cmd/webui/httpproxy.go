// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/mrhaoxx/devpod/internal/webui"
)

// httpProxyFlags: DevPod ports published through the gateway's HTTP proxy.
type httpProxyFlags struct {
	host, scheme, keyFile string
	port                  int
}

func addHTTPProxyFlags(fs *flag.FlagSet) *httpProxyFlags {
	f := &httpProxyFlags{}
	fs.StringVar(&f.host, "http-proxy-host", "", "hostname pattern of the gateway's HTTP proxy with {id} and {port}, e.g. {id}-{port}-devpod.example.com (enables the ports panel)")
	fs.StringVar(&f.scheme, "http-proxy-scheme", "https", "scheme of proxy URLs")
	fs.StringVar(&f.keyFile, "http-proxy-key-file", "", "key shared with the gateway (--http-auth-key-file)")
	fs.IntVar(&f.port, "http-proxy-port", 0, "port of the proxy's public entry when it is not the scheme's own, e.g. 4430 for https://…:4430 (0: none)")
	return f
}

// build returns nil when the feature is off.
func (f *httpProxyFlags) build() (*webui.HTTPProxy, error) {
	if f.host == "" {
		return nil, nil
	}
	key, err := os.ReadFile(f.keyFile)
	if err != nil {
		return nil, fmt.Errorf("--http-proxy-key-file: %w", err)
	}
	if f.port < 0 || f.port > 65535 {
		return nil, fmt.Errorf("--http-proxy-port: %d is not a port", f.port)
	}
	hp, err := webui.NewHTTPProxy(f.host, f.scheme, bytes.TrimSpace(key))
	if err != nil {
		return nil, err
	}
	hp.Port = f.port
	return hp, nil
}
