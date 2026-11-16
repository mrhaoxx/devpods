// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/mrhaoxx/devpod/internal/gatewaycli"
)

// cliFlags: the key that lets a gateway that still runs the CLI itself
// call the API for a user it authenticated. Gateways of this release hand
// the CLI to the console (consoleCLI) and need no key.
type cliFlags struct{ keyFile string }

func addCLIFlags(fs *flag.FlagSet) *cliFlags {
	f := &cliFlags{}
	fs.StringVar(&f.keyFile, "cli-api-key-file", "", "key shared with the gateway (--cli-api-key-file); enables API calls on behalf of SSH-authenticated users")
	return f
}

func (f *cliFlags) key() ([]byte, error) {
	if f.keyFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(f.keyFile)
	if err != nil {
		return nil, fmt.Errorf("--cli-api-key-file: %w", err)
	}
	if b = bytes.TrimSpace(b); len(b) < 16 {
		return nil, fmt.Errorf("--cli-api-key-file: key too short")
	}
	return b, nil
}

// consoleCLI is the DevPod CLI run in the console for the gateways
// (GET /gateway/v1/cli): it calls this console's API over loopback, at
// listen, with a key made now and held nowhere else.
func consoleCLI(listen string) (*gatewaycli.Server, []byte, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, nil, fmt.Errorf("--listen %q: %w", listen, err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, nil, err
	}
	key := []byte(hex.EncodeToString(b))
	api := gatewaycli.NewAPI("http://"+net.JoinHostPort("127.0.0.1", port), string(key))
	return &gatewaycli.Server{API: api}, key, nil
}
