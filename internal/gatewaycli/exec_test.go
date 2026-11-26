// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// From a DevPod a command runs as its owner, knowing where it was asked
// from — its cluster, its working directory, the files it named.
func TestExecOrigin(t *testing.T) {
	var got *Env
	saved, savedOrigin := commands, OriginCommands
	t.Cleanup(func() { commands, OriginCommands = saved, savedOrigin })
	commands = append(slices.Clone(commands), &command{name: "probe", run: func(e *Env, _ *parsed) int {
		got = e
		fmt.Fprint(e.Out, "seen")
		return 0
	}})
	OriginCommands = append(slices.Clone(OriginCommands), "probe")
	s := &Server{API: NewAPI("http://127.0.0.1:1", "k")}
	o := &Origin{Cluster: "st", DevPod: "alice-dev", Cwd: "/home/alice/x"}
	var out, errw bytes.Buffer
	if code := s.Exec(context.Background(), Meta{User: "alice"}, []string{"probe"}, o, map[string]string{"t.sh": "echo hi"}, &out, &errw); code != 0 || out.String() != "seen" {
		t.Fatalf("probe: %d %q %q", code, out.String(), errw.String())
	}
	if got.User != "alice" || got.Origin == nil || got.Origin.Cwd != "/home/alice/x" || got.Files["t.sh"] != "echo hi" || got.Cluster != "st" {
		t.Fatalf("env: user %q origin %+v files %v cluster %q", got.User, got.Origin, got.Files, got.Cluster)
	}
	// its owner's own rights, never an admin's (the server's API stays as it was)
	if !got.API.DropAdmin || s.API.DropAdmin {
		t.Fatalf("drop admin: env %v server %v", got.API.DropAdmin, s.API.DropAdmin)
	}
	// what is not for a DevPod is refused
	out.Reset()
	errw.Reset()
	if code := s.Exec(context.Background(), Meta{User: "alice"}, []string{"delete", "x", "--yes"}, o, nil, &out, &errw); code != ExitUsage || !strings.Contains(errw.String(), "not available from inside a DevPod") {
		t.Fatalf("delete from a DevPod: %d %q", code, errw.String())
	}
}

// An API that drops admin says so on every request, streamed ones too.
func TestAPIDropAdmin(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(DropAdminHeader))
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: x\n\n")
			return
		}
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	api := NewAPI(srv.URL, "k")
	if _, err := api.Do(context.Background(), "alice", "GET", "/api/me", nil); err != nil {
		t.Fatal(err)
	}
	api.DropAdmin = true
	if _, err := api.Do(context.Background(), "alice", "GET", "/api/me", nil); err != nil {
		t.Fatal(err)
	}
	if err := api.Stream(context.Background(), "alice", "/api/x", time.Second, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"", "1", "1"}) {
		t.Fatalf("headers %q", seen)
	}
}
