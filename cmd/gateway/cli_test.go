// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

func TestCLIFlags_Build(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)

	// off without an API URL, or with the user name cleared
	for _, f := range []cliFlags{{user: "devpod"}, {user: "", apiURL: "http://webui", keyFile: key}} {
		if d, err := f.build(); d != nil || err != nil {
			t.Fatalf("%+v: %v %v", f, d, err)
		}
	}
	// on: it must have a key
	for _, f := range []cliFlags{{user: "devpod", apiURL: "http://webui"}, {user: "devpod", apiURL: "http://webui", keyFile: empty}} {
		if _, err := f.build(); err == nil {
			t.Fatalf("%+v built without a key", f)
		}
	}
	d, err := (&cliFlags{user: "dp", apiURL: "http://webui/", keyFile: key, maxConns: 7}).build()
	if err != nil || d.user != "dp" || d.srv.API.BaseURL != "http://webui" || d.srv.API.Key != "s3cret" || d.srv.MaxConns != 7 {
		t.Fatalf("built %+v %v", d, err)
	}
}

func TestCLIAuthHint(t *testing.T) {
	if got := cliAuthHint("devpod", "", gateway.ErrCLINeedsUser); !strings.Contains(got, "<用户名>+devpod@") {
		t.Fatalf("bare login: %q", got)
	}
	if got := cliAuthHint("dp", "", gateway.ErrCLINeedsUser); !strings.Contains(got, "<用户名>+dp@") {
		t.Fatalf("bare login, other name: %q", got)
	}
	if got := cliAuthHint("devpod", "alice", errors.New("key mismatch")); !strings.Contains(got, "alice") {
		t.Fatalf("named user: %q", got)
	}
	if got := cliAuthHint("devpod", "", errors.New("other")); got != "" {
		t.Fatalf("nothing to say: %q", got)
	}
}
