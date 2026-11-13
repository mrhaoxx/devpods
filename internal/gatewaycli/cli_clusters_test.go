// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// runAs runs a command line with standard input and in the shell or not.
func (f *fakeAPI) runAs(line, stdin string, interactive bool) (code int, out, errs string) {
	f.t.Helper()
	argv, err := Split(line)
	if err != nil {
		f.t.Fatal(err)
	}
	var o, e bytes.Buffer
	lines := strings.SplitAfter(stdin, "\n")
	next := func() (string, error) {
		if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
			return "", io.EOF
		}
		l := strings.TrimSuffix(lines[0], "\n")
		lines = lines[1:]
		return l, nil
	}
	env := &Env{Ctx: context.Background(), User: "alice", API: NewAPI(f.srv.URL, "k3y"), Out: &o, Err: &e, Interactive: interactive,
		Now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }}
	if stdin != "" {
		env.StdinLine = next
	}
	code = Run(env, argv)
	return code, o.String(), e.String()
}

// In a console of several clusters: names say their cluster where needed,
// lists show it, things are made where --cluster says, ssh goes to the
// cluster's gateway.
func TestSeveralClusters(t *testing.T) {
	f := newFakeAPI(t)
	inCluster := func(c string, dp map[string]any) map[string]any { dp["cluster"] = c; return dp }
	f.routes["GET /api/devpods"] = map[string]any{"items": []any{
		inCluster("st2", devpod("code", "Running", "ubuntu:24.04")), inCluster("st", devpod("code", "Stopped", "ubuntu:24.04")),
	}, "pods": map[string]any{"st2/code": map[string]any{"node": "st2-51"}}}
	f.routes["PATCH /api/clusters/st2/devpods/code"] = map[string]any{"devpod": devpod("code", "Running", "x")}
	f.routes["PATCH /api/devpods/code"] = map[string]any{"code": "AMBIGUOUS_CLUSTER", "message": "several clusters have a DevPod named code: say which with /api/clusters/{cluster}/…",
		"detail": []any{map[string]any{"cluster": "st2", "name": "code"}, map[string]any{"cluster": "st", "name": "code"}}}
	f.status["PATCH /api/devpods/code"] = http.StatusConflict
	f.routes["POST /api/clusters/st2/devpods"] = devpod("alice-dev1", "Pending", "x")
	f.routes["POST /api/clusters/st/devpods"] = devpod("alice-dev2", "Pending", "x")
	f.routes["GET /api/clusters/st2/devpods/code"] = map[string]any{"devpod": devpod("alice-code", "Running", "x"), "cluster": "st2"}
	f.routes["GET /api/me"] = map[string]any{"user": "alice", "ssh": map[string]any{"host": "console.test"},
		"clusters": map[string]any{
			"st2": map[string]any{"ssh": map[string]any{"host": "dev.test", "port": 2222, "loginSuffix": "st2"}, "usage": map[string]any{"devpods": 1, "running": 1}},
			"st":  map[string]any{"usage": map[string]any{"devpods": 1}},
		}}
	f.routes["GET /api/clusters"] = map[string]any{"items": []any{
		map[string]any{"name": "st2", "connected": true, "gateway": "st"}, map[string]any{"name": "st", "connected": false, "gateway": "st"},
	}}
	last := func() string { return f.calls[len(f.calls)-1] }

	_, out, _ := f.runAs("list", "", false)
	if !strings.HasPrefix(out, "CLUSTER  NAME") || !strings.Contains(out, "st2      code  alice  Running  st2-51") {
		t.Fatalf("list:\n%s", out)
	}
	if code, out, _ := f.runAs("start st2/code", "", false); code != 0 || out != "st2/code: started\n" || last() != `PATCH /api/clusters/st2/devpods/code {"running":true}` {
		t.Fatalf("start st2/code: %d %q %s", code, out, last())
	}
	if code, _, errs := f.runAs("start code", "", false); code != ExitAPI ||
		errs != "error: code: code is in several clusters (st, st2): say which as st/code, or add --cluster st (AMBIGUOUS_CLUSTER)\n" {
		t.Fatalf("ambiguous: %d %q", code, errs)
	}
	if code, out, _ := f.runAs("create st2/dev1 --image x", "", false); code != 0 || out != "created alice-dev1\n" || !strings.HasPrefix(last(), `POST /api/clusters/st2/devpods {"image":"x","name":"dev1"}`) {
		t.Fatalf("create st2/dev1: %d %q %s", code, out, last())
	}
	if code, _, _ := f.runAs("create dev2 --image x --cluster st", "", false); code != 0 || !strings.HasPrefix(last(), "POST /api/clusters/st/devpods") {
		t.Fatalf("create --cluster: %d %s", code, last())
	}
	if _, out, _ := f.runAs("ssh st2/code", "", false); out != "ssh -p 2222 alice+code+st2@dev.test\n" {
		t.Fatalf("ssh: %q", out)
	}
	if _, out, _ := f.runAs("clusters", "", false); !strings.Contains(out, "st2   connected  st       dev.test:2222  1 (1 running)") || !strings.Contains(out, "st    away") {
		t.Fatalf("clusters:\n%s", out)
	}
	if _, out, _ := f.runAs("help", "", true); !strings.HasPrefix(out, "DevPod CLI — type a command") || !strings.Contains(out, "<cluster>/<name>") {
		t.Fatalf("help in the shell:\n%s", out)
	}
}

// What the console does beside the basics, the CLI does too.
func TestConsoleFeatures(t *testing.T) {
	f := newFakeAPI(t)
	last := func() string { return f.calls[len(f.calls)-1] }
	f.routes["GET /api/devpods/dev1/events"] = map[string]any{"items": []any{map[string]any{"type": "Warning", "reason": "BackOff", "message": "pulling",
		"count": 3, "lastTimestamp": "2026-11-11T11:58:00Z", "involvedObject": map[string]any{"kind": "Pod"}}}}
	if _, out, _ := f.runAs("events dev1", "", false); !strings.Contains(out, "2m   Warning  BackOff (×3)  Pod     pulling") {
		t.Fatalf("events:\n%s", out)
	}
	f.routes["GET /api/devpods/dev1/yaml"] = map[string]any{"yaml": "spec:\n  running: true\n"}
	if _, out, _ := f.runAs("yaml dev1", "", false); out != "spec:\n  running: true\n" {
		t.Fatalf("yaml: %q", out)
	}
	f.routes["PUT /api/devpods/dev1/yaml"] = map[string]any{"rebuild": true}
	if code, out, _ := f.runAs("yaml dev1 --apply", "spec:\n  running: false\n", false); code != 0 || !strings.HasPrefix(out, "dev1: updated") ||
		last() != `PUT /api/devpods/dev1/yaml {"yaml":"spec:\n  running: false\n"}` {
		t.Fatalf("yaml --apply: %d %q %s", code, out, last())
	}
	f.routes["POST /api/devpods/dev1/rebuild-from-config"] = map[string]any{"rebuild": false}
	if code, out, _ := f.runAs("rebuild dev1", "", false); code != 0 || out != "dev1: rebuilt from its config\n" {
		t.Fatalf("rebuild: %d %q", code, out)
	}
	f.routes["GET /api/reservations/rules"] = map[string]any{"granularitySeconds": 1800, "timezone": "Asia/Shanghai"}
	f.routes["POST /api/reservations"] = map[string]any{"id": "r1", "node": "g1", "start": "2026-11-12T01:00:00Z", "end": "2026-11-12T05:00:00Z"}
	if code, out, _ := f.runAs("reserve add g1 --start 2026-11-12T09:00 --for 4h", "", false); code != 0 || !strings.Contains(out, "reserved g1 2026-11-12 09:00 – 13:00 CST (id r1)") ||
		last() != `POST /api/reservations {"end":"2026-11-12T05:00:00Z","node":"g1","start":"2026-11-12T01:00:00Z"}` {
		t.Fatalf("reserve add: %d %q %s", code, out, last())
	}
	f.routes["POST /api/me/tokens"] = map[string]any{"token": "dpt_abc_secret", "info": map[string]any{"id": "abc", "name": "ci", "expiresAt": "2026-12-11T00:00:00Z"}}
	if code, out, _ := f.runAs("token create ci --scopes devpods:read,devpods:write --days 30", "", false); code != 0 || !strings.HasPrefix(out, "dpt_abc_secret\n") ||
		last() != `POST /api/me/tokens {"devpods":[],"expiresInDays":30,"name":"ci","scopes":["devpods:read","devpods:write"]}` {
		t.Fatalf("token create: %d %q %s", code, out, last())
	}
	have := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAt0V7oVJh2yrVOmH5x4ExdPu9RWYYaXmaTXWDaD3EYF old"
	add := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN/gu2lmnq6IFjOnRYK7c6Y3ymL3TylDxZxpiyIvh1dv new"
	f.routes["GET /api/me/pubkeys"] = map[string]any{"pubkeys": []string{have}}
	f.routes["PUT /api/me/pubkeys"] = map[string]any{}
	if code, out, _ := f.runAs("key add", add+"\n", false); code != 0 || out != "added 1 key(s); 2 in all\n" || last() != `PUT /api/me/pubkeys {"pubkeys":["`+have+`","`+add+`"]}` {
		t.Fatalf("key add: %d %q %s", code, out, last())
	}
	if code, out, _ := f.runAs("key rm old", "", false); code != 0 || out != "removed 1 key(s); 0 left\n" || last() != `PUT /api/me/pubkeys {"pubkeys":[]}` {
		t.Fatalf("key rm: %d %q %s", code, out, last())
	}
	f.routes["POST /api/clusters/st2/snapshots/s1/retry"] = map[string]any{"id": "s1"}
	if code, out, _ := f.runAs("snapshot retry st2/s1", "", false); code != 0 || !strings.Contains(out, "snapshot st2/s1 is being taken again") {
		t.Fatalf("snapshot retry: %d %q", code, out)
	}
}
