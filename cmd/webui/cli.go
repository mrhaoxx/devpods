// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
)

// cliFlags: the key that lets the SSH gateway call the API for a user it
// authenticated (the CLI over SSH).
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
