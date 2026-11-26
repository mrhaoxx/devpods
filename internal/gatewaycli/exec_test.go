// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
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
	// what is not for a DevPod is refused
	out.Reset()
	errw.Reset()
	if code := s.Exec(context.Background(), Meta{User: "alice"}, []string{"delete", "x", "--yes"}, o, nil, &out, &errw); code != ExitUsage || !strings.Contains(errw.String(), "not available from inside a DevPod") {
		t.Fatalf("delete from a DevPod: %d %q", code, errw.String())
	}
}
