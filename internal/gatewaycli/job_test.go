// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// runFrom runs line as dp does from inside a DevPod (o), with files.
func (f *fakeAPI) runFrom(line string, o *Origin, files map[string]string) (int, string, string) {
	f.t.Helper()
	argv, err := Split(line)
	if err != nil {
		f.t.Fatal(err)
	}
	var out, errw bytes.Buffer
	code := Run(&Env{Ctx: context.Background(), User: "alice", API: NewAPI(f.srv.URL, "k3y"), Out: &out, Err: &errw, Origin: o, Files: files,
		Now: func() time.Time { return time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC) }}, argv)
	return code, out.String(), errw.String()
}

// lastBody: the JSON body of the last call to path.
func lastBody(t *testing.T, f *fakeAPI, method, path string) map[string]any {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if rest, ok := strings.CutPrefix(f.calls[i], method+" "+path+" "); ok {
			var b map[string]any
			if err := json.Unmarshal([]byte(rest), &b); err != nil {
				t.Fatalf("body %q: %v", rest, err)
			}
			return b
		}
	}
	t.Fatalf("no %s %s in %v", method, path, f.calls)
	return nil
}

var origin = &Origin{Cluster: "st", DevPod: "alice-dev", Cwd: "/home/alice/x"}

// From a DevPod a job is made from it, runs where dp was asked, in its
// cluster.
func TestJobSubmit(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued"}
	code, out, errs := f.runFrom("job submit -N 2 --gpus 8 -t 1-00:00 --priority 5 --preemptible -- torchrun t.py", origin, nil)
	if code != 0 || !strings.Contains(out, "abc12") {
		t.Fatalf("submit: %d %q %q", code, out, errs)
	}
	b := lastBody(t, f, "POST", "/api/clusters/st/jobs")
	from, _ := b["from"].(map[string]any)
	if from["devpod"] != "alice-dev" || b["workdir"] != "/home/alice/x" || b["nodes"] != float64(2) || b["gpus"] != float64(8) ||
		b["timeLimitSeconds"] != float64(86400) || b["priority"] != float64(5) || b["preemptible"] != true || b["command"] != "torchrun t.py" {
		t.Fatalf("body %v", b)
	}
	// a script named on the command line goes with it
	f.runFrom("job submit --gpus 1 --script train.sh", origin, map[string]string{"train.sh": "#!/bin/bash\necho hi\n"})
	if b := lastBody(t, f, "POST", "/api/clusters/st/jobs"); b["script"] != "#!/bin/bash\necho hi\n" || b["title"] != "train.sh" {
		t.Fatalf("script body %v", b)
	}
}

// Over SSH a job needs to be told what it comes from.
func TestJobSubmitNeedsSource(t *testing.T) {
	f := newFakeAPI(t)
	if code, _, errs := f.run("job submit --gpus 1 -- true"); code != ExitUsage || !strings.Contains(errs, "--from-devpod") {
		t.Fatalf("no source: %d %q", code, errs)
	}
}

func TestParseTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30": 30 * time.Minute, "90:30": 90*time.Minute + 30*time.Second, "2:00:00": 2 * time.Hour,
		"1-12": 36 * time.Hour, "1-00:30": 24*time.Hour + 30*time.Minute, "2-01:00:30": 49*time.Hour + 30*time.Second,
		"4h": 4 * time.Hour, "90m": 90 * time.Minute,
	} {
		if got, err := parseTime(in); err != nil || got != want {
			t.Errorf("%q: %s %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x", "1-", "1:2:3:4", "-5", "0"} {
		if _, err := parseTime(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The queue in columns: a preemptible job marked, a waiting one saying why.
func TestJobList(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["GET /api/jobs"] = map[string]any{"items": []any{
		map[string]any{"cluster": "st", "id": "run01", "owner": "alice", "phase": "Running", "nodes": 2, "gpus": 8, "priority": 200, "timeLimitSeconds": 43200,
			"startedAt": "2026-11-27T01:00:00Z", "preemptible": true},
		map[string]any{"cluster": "st", "id": "wait1", "owner": "carol", "phase": "Queued", "nodes": 4, "gpus": 8, "priority": 500, "timeLimitSeconds": 86400,
			"position": 1, "reason": "capacity", "detail": map[string]any{"need": "32", "free": "18", "fit": "1"}, "expectedStart": "2026-11-27T18:10:00Z"},
	}}
	code, out, errs := f.runFrom("job list", nil, nil)
	if code != 0 {
		t.Fatalf("list: %d %q", code, errs)
	}
	for _, want := range []string{"ID", "USER", "SIZE", "PRIORITY", "LIMIT", "STATE", "st/run01⚡", "2×8 GPU", "running 3h (≤9h)", "#1 capacity: 4×8 GPU, 1 machine has room (18 free)", "~02:10"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

// Following a rank's output prints it as it comes.
func TestJobLogsFollow(t *testing.T) {
	f := newFakeAPI(t)
	f.raw["GET /api/jobs/abc12/logs?rank=1&follow=1"] = "data: step 1\n\ndata: step 2\n\n"
	code, out, errs := f.run("job logs -f --rank 1 abc12")
	if code != 0 || out != "step 1\nstep 2\n" {
		t.Fatalf("logs -f: %d %q %q", code, out, errs)
	}
}
