// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
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

// A job that gave way to a reservation made as it started says so.
func TestJobStateReserved(t *testing.T) {
	now := time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC)
	if s := jobState(jobView{Phase: "Queued", Position: 2, Reason: "reserved"}, now); s != "#2 a reservation took its place; waiting again" {
		t.Fatalf("%q", s)
	}
	if st := map[string]string{"reserved": "(Reservation)"}; squeueReason("reserved") != st["reserved"] {
		t.Fatalf("squeue: %q", squeueReason("reserved"))
	}
}

// An earlier start's output, as kept.
func TestJobLogsAttempt(t *testing.T) {
	f := newFakeAPI(t)
	f.raw["GET /api/jobs/abc12/logs?rank=0&attempt=1"] = "before it was preempted\n"
	if code, out, errs := f.run("job logs --attempt 1 abc12"); code != 0 || out != "before it was preempted\n" {
		t.Fatalf("--attempt 1: %d %q %q", code, out, errs)
	}
	if code, _, errs := f.run("job logs -f --attempt 1 abc12"); code != ExitUsage || !strings.Contains(errs, "kept") {
		t.Fatalf("-f --attempt: %d %q", code, errs)
	}
}

// From a DevPod a job takes the shell's environment (as sbatch's
// --export=ALL): PATH as DEVPOD_SUBMIT_PATH (the ranks' login shell resets
// PATH), --env over it; --export NONE takes only --env.
func TestJobSubmitExportsEnv(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st"}
	o := &Origin{Cluster: "st", DevPod: "alice-dev", Cwd: "/home/alice/x", Env: map[string]string{"PATH": "/opt/conda/bin:/usr/bin", "OMP_NUM_THREADS": "8", "FOO": "shell"}}
	if code, _, errs := f.runFrom("job submit --env FOO=flag -- python t.py", o, nil); code != 0 {
		t.Fatalf("submit: %d %q", code, errs)
	}
	env, _ := lastBody(t, f, "POST", "/api/clusters/st/jobs")["env"].(map[string]any)
	if env["DEVPOD_SUBMIT_PATH"] != "/opt/conda/bin:/usr/bin" || env["OMP_NUM_THREADS"] != "8" || env["FOO"] != "flag" || env["PATH"] != nil {
		t.Fatalf("env %v", env)
	}
	if code, _, errs := f.runFrom("job submit --export NONE --env FOO=flag -- true", o, nil); code != 0 {
		t.Fatalf("submit NONE: %d %q", code, errs)
	}
	if env, _ := lastBody(t, f, "POST", "/api/clusters/st/jobs")["env"].(map[string]any); len(env) != 1 || env["FOO"] != "flag" {
		t.Fatalf("NONE: %v", env)
	}
	if code, _, errs := f.runFrom("sbatch train.sh", o, map[string]string{"train.sh": trainScript}); code != 0 {
		t.Fatalf("sbatch: %d %q", code, errs)
	}
	if env, _ := lastBody(t, f, "POST", "/api/clusters/st/jobs")["env"].(map[string]any); env["OMP_NUM_THREADS"] != "8" {
		t.Fatalf("sbatch env %v", env)
	}
	if code, _, errs := f.runFrom("sbatch --export=NONE train.sh", o, map[string]string{"train.sh": trainScript}); code != 0 {
		t.Fatalf("sbatch NONE: %d %q", code, errs)
	}
	if env := lastBody(t, f, "POST", "/api/clusters/st/jobs")["env"]; env != nil {
		t.Fatalf("sbatch NONE env %v", env)
	}
}

// runWait runs line from a DevPod with an interrupt channel.
func (f *fakeAPI) runWait(line string, interrupt <-chan struct{}) (int, string, string) {
	f.t.Helper()
	argv, err := Split(line)
	if err != nil {
		f.t.Fatal(err)
	}
	var out, errw syncBuffer
	code := Run(&Env{Ctx: context.Background(), Session: context.Background(), User: "alice", API: NewAPI(f.srv.URL, "k3y"), Out: &out, Err: &errw, Origin: origin,
		Interrupt: interrupt, Now: func() time.Time { return time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC) }}, argv)
	return code, out.String(), errw.String()
}

// job submit --wait is srun's: it waits, prints every rank's output as it
// comes (marked by rank when several), and ends with the job's exit code.
func TestJobSubmitWait(t *testing.T) {
	saved := waitPoll
	waitPoll = 20 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 2}
	n := 0
	f.dyn["GET /api/jobs/abc12"] = func() any {
		n++
		switch {
		case n == 1:
			return map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 2, "position": 1, "reason": "behind", "detail": map[string]any{"head": "zzz99"}}
		case n < 4:
			return map[string]any{"id": "abc12", "cluster": "st", "phase": "Running", "nodes": 2, "ranks": []any{map[string]any{"rank": 0, "node": "m700"}, map[string]any{"rank": 1, "node": "m701"}}}
		}
		return map[string]any{"id": "abc12", "cluster": "st", "phase": "Failed", "nodes": 2, "reason": "rank-failed", "detail": map[string]any{"rank": "1", "exit": "3"}}
	}
	f.raw["GET /api/jobs/abc12/logs?rank=0&follow=1"] = "data: hello from 0\n\n"
	f.raw["GET /api/jobs/abc12/logs?rank=1&follow=1"] = "data: hello from 1\n\n"
	code, out, errs := f.runWait("job submit --wait -N 2 --gpus 1 -- torchrun t.py", nil)
	if code != 3 || !strings.Contains(out, "0: hello from 0\n") || !strings.Contains(out, "1: hello from 1\n") {
		t.Fatalf("--wait: %d out %q err %q", code, out, errs)
	}
	if !strings.Contains(errs, "abc12") || !strings.Contains(errs, "m700") {
		t.Fatalf("progress: %q", errs)
	}
}

// Interrupted (Ctrl-C, or dp's caller gone), --wait cancels the job.
func TestJobSubmitWaitInterrupted(t *testing.T) {
	saved := waitPoll
	waitPoll = 20 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 1}
	f.dyn["GET /api/jobs/abc12"] = func() any {
		return map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 1, "position": 3}
	}
	f.status["DELETE /api/jobs/abc12"] = 204
	f.routes["DELETE /api/jobs/abc12"] = nil
	stop := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(stop) })
	code, _, errs := f.runWait("job submit --wait --gpus 1 -- sleep 100", stop)
	if code != 130 || !strings.Contains(errs, "cancelled") {
		t.Fatalf("interrupted: %d %q", code, errs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.calls, "DELETE /api/jobs/abc12") {
		t.Fatalf("calls %v", f.calls)
	}
}

// Requeued (preempted, say), --wait follows the next attempt's output too.
func TestJobSubmitWaitRequeued(t *testing.T) {
	saved := waitPoll
	waitPoll = 20 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 1}
	n := 0
	f.dyn["GET /api/jobs/abc12"] = func() any {
		n++
		phase := map[int]string{1: "Running", 2: "Queued", 3: "Running"}[n]
		if phase == "" {
			phase = "Succeeded"
		}
		return map[string]any{"id": "abc12", "cluster": "st", "phase": phase, "nodes": 1, "ranks": []any{map[string]any{"rank": 0, "node": "m700"}}}
	}
	f.raw["GET /api/jobs/abc12/logs?rank=0&follow=1"] = "data: an attempt's line\n\n"
	code, out, errs := f.runWait("job submit --wait --gpus 1 -- true", nil)
	if code != 0 || strings.Count(out, "an attempt's line\n") != 2 {
		t.Fatalf("requeued: %d out %q err %q", code, out, errs)
	}
}

// A job that ended without ever running (cancelled while waiting) ends
// --wait at once: there is no output to look for.
func TestJobSubmitWaitNeverRan(t *testing.T) {
	saved := waitPoll
	waitPoll = 20 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 2}
	f.dyn["GET /api/jobs/abc12"] = func() any {
		return map[string]any{"id": "abc12", "cluster": "st", "phase": "Cancelled", "nodes": 2, "reason": "cancelled"}
	}
	start := time.Now()
	code, _, errs := f.runWait("job submit --wait -N 2 --gpus 1 -- true", nil)
	if code != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("never ran: %d after %s, %q", code, time.Since(start), errs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, "/logs") {
			t.Fatalf("looked for output: %v", f.calls)
		}
	}
}
