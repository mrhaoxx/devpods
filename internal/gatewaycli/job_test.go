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
	"sync"
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
// cluster. Detached (-d), the submit says what it made and is done.
func TestJobSubmit(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued"}
	code, out, errs := f.runFrom("job submit --detach -N 2 --gpus 8 -t 1-00:00 --priority 5 --preemptible -- torchrun t.py", origin, nil)
	if code != 0 || !strings.Contains(out, "abc12") {
		t.Fatalf("submit: %d %q %q", code, out, errs)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "GET /api/jobs/abc12") {
			t.Fatalf("detached, it waited: %v", f.calls)
		}
	}
	b := lastBody(t, f, "POST", "/api/clusters/st/jobs")
	from, _ := b["from"].(map[string]any)
	if from["devpod"] != "alice-dev" || b["workdir"] != "/home/alice/x" || b["nodes"] != float64(2) || b["gpus"] != float64(8) ||
		b["timeLimitSeconds"] != float64(86400) || b["priority"] != float64(5) || b["preemptible"] != true || b["command"] != "torchrun t.py" {
		t.Fatalf("body %v", b)
	}
	// a script named on the command line goes with it
	f.runFrom("job submit -d --gpus 1 --script train.sh", origin, map[string]string{"train.sh": "#!/bin/bash\necho hi\n"})
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
	if code, _, errs := f.runFrom("job submit -d --env FOO=flag -- python t.py", o, nil); code != 0 {
		t.Fatalf("submit: %d %q", code, errs)
	}
	env, _ := lastBody(t, f, "POST", "/api/clusters/st/jobs")["env"].(map[string]any)
	if env["DEVPOD_SUBMIT_PATH"] != "/opt/conda/bin:/usr/bin" || env["OMP_NUM_THREADS"] != "8" || env["FOO"] != "flag" || env["PATH"] != nil {
		t.Fatalf("env %v", env)
	}
	if code, _, errs := f.runFrom("job submit -d --export NONE --env FOO=flag -- true", o, nil); code != 0 {
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

// job submit is srun's (unless -d): it waits, prints every rank's output as
// it comes (marked by rank when several), and ends with the job's exit code.
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
	code, out, errs := f.runWait("job submit -N 2 --gpus 1 -- torchrun t.py", nil)
	if code != 3 || !strings.Contains(out, "0: hello from 0\n") || !strings.Contains(out, "1: hello from 1\n") {
		t.Fatalf("attached: %d out %q err %q", code, out, errs)
	}
	if !strings.Contains(errs, "abc12") || !strings.Contains(errs, "m700") {
		t.Fatalf("progress: %q", errs)
	}
}

// Interrupted (Ctrl-C, or dp's caller gone), a waiting submit cancels the job.
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
	code, _, errs := f.runWait("job submit --gpus 1 -- sleep 100", stop)
	if code != 130 || !strings.Contains(errs, "cancelled") {
		t.Fatalf("interrupted: %d %q", code, errs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.calls, "DELETE /api/jobs/abc12") {
		t.Fatalf("calls %v", f.calls)
	}
}

// Requeued (preempted, say), a waiting submit follows the next attempt's output too.
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
	code, out, errs := f.runWait("job submit --gpus 1 -- true", nil)
	if code != 0 || strings.Count(out, "an attempt's line\n") != 2 {
		t.Fatalf("requeued: %d out %q err %q", code, out, errs)
	}
}

// A job that ended without ever running (cancelled while waiting) ends
// the wait at once: there is no output to look for.
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
	code, _, errs := f.runWait("job submit -N 2 --gpus 1 -- true", nil)
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

// A job gone while waited for (deleted by an admin, say) ends the wait.
func TestJobSubmitWaitJobGone(t *testing.T) {
	saved := waitPoll
	waitPoll = 20 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 1}
	f.status["GET /api/jobs/abc12"] = 404
	f.routes["GET /api/jobs/abc12"] = map[string]any{"code": "NOT_FOUND", "message": "no such job"}
	done := make(chan struct{})
	var code int
	var errs string
	go func() { code, _, errs = f.runWait("job submit --gpus 1 -- true", nil); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting for a job that is gone")
	}
	if code != 1 || !strings.Contains(errs, "gone") {
		t.Fatalf("gone: %d %q", code, errs)
	}
}

// job submit says whether a job stopped for another waits again.
func TestJobSubmitRequeueFlags(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st"}
	if code, _, errs := f.runFrom("job submit -d --no-requeue -- true", origin, nil); code != 0 {
		t.Fatalf("--no-requeue: %d %q", code, errs)
	}
	if b := lastBody(t, f, "POST", "/api/clusters/st/jobs"); b["requeue"] != false {
		t.Fatalf("--no-requeue body %v", b)
	}
	if code, _, errs := f.runFrom("job submit -d -- true", origin, nil); code != 0 {
		t.Fatalf("plain: %d %q", code, errs)
	}
	if _, has := lastBody(t, f, "POST", "/api/clusters/st/jobs")["requeue"]; has {
		t.Fatal("requeue sent unasked")
	}
	if code, _, _ := f.runFrom("job submit -d --requeue --no-requeue -- true", origin, nil); code != ExitUsage {
		t.Fatalf("both: %d", code)
	}
}

// The queue's list shows the DevPods waiting in its line, marked so.
func TestJobListShowsDevPods(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["GET /api/jobs"] = map[string]any{"items": []any{
		map[string]any{"id": "abc12", "owner": "bob", "phase": "Queued", "position": 1, "nodes": 1, "gpus": 8, "timeLimitSeconds": 3600, "reason": "capacity"}},
		"devpods": []any{map[string]any{"name": "alice-a", "owner": "alice", "mine": true, "priority": 0, "position": 2, "reason": "behind", "detail": map[string]any{"head": "abc12"}, "gpus": 1, "cpus": 4, "queuedAt": "2026-11-27T01:00:00Z"}}}
	code, out, errs := f.run("job list")
	if code != 0 || !strings.Contains(out, "devpod/alice-a") || strings.Index(out, "abc12") > strings.Index(out, "devpod/alice-a") {
		t.Fatalf("list: %d %q %q", code, out, errs)
	}
	// a DevPod has no time limit: its LIMIT is a dash, not 0m
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "devpod/alice-a") && (strings.Contains(line, "0m") || !strings.Contains(line, "—")) {
			t.Fatalf("the DevPod's row: %q", line)
		}
	}
}

// The queue's jobs are managed from the command line: their priority (a
// DevPod's in line too), held back and released, submitted again; a
// DevPod in line is cancelled out of it.
func TestJobManage(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["PATCH /api/jobs/abc12"] = map[string]any{"id": "abc12", "priority": 5}
	f.routes["PATCH /api/devpods/alice-a"] = map[string]any{"devpod": map[string]any{}}
	f.routes["POST /api/jobs/abc12/resubmit"] = map[string]any{"id": "def34"}
	for _, c := range []struct{ line, call, out string }{
		{"job priority abc12 5", `PATCH /api/jobs/abc12 {"priority":5}`, "abc12: priority 5"},
		{"job priority devpod/alice-a 2", `PATCH /api/devpods/alice-a {"priority":2}`, "devpod/alice-a: priority 2"},
		{"job hold abc12", `PATCH /api/jobs/abc12 {"held":true}`, "abc12: held"},
		{"job release abc12", `PATCH /api/jobs/abc12 {"held":false}`, "abc12: released"},
		{"job resubmit abc12", `POST /api/jobs/abc12/resubmit`, "def34"},
		{"job cancel devpod/alice-a", `PATCH /api/devpods/alice-a {"running":false}`, "devpod/alice-a: cancelled"},
	} {
		f.mu.Lock()
		f.calls = nil
		f.mu.Unlock()
		code, out, errs := f.run(c.line)
		f.mu.Lock()
		calls := append([]string(nil), f.calls...)
		f.mu.Unlock()
		if code != 0 || !strings.Contains(out, c.out) || len(calls) != 1 || calls[0] != c.call {
			t.Fatalf("%s: %d %q %q calls %q", c.line, code, out, errs, calls)
		}
	}
	if code, _, _ := f.run("job priority abc12 -1"); code == 0 {
		t.Fatal("a negative priority was taken")
	}
	if code, _, errs := f.run("job hold devpod/alice-a"); code == 0 || !strings.Contains(errs, "job cancel") {
		t.Fatalf("holding a DevPod: %d %q", code, errs)
	}
}

// A held job says so in the list.
func TestJobListHeld(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["GET /api/jobs"] = map[string]any{"items": []any{
		map[string]any{"id": "abc12", "owner": "alice", "phase": "Queued", "reason": "held", "held": true, "nodes": 1, "gpus": 8, "timeLimitSeconds": 3600}}}
	if code, out, errs := f.run("job list"); code != 0 || !strings.Contains(out, "held") {
		t.Fatalf("list: %d %q %q", code, out, errs)
	}
}

// A warning of the console's (a working directory the machines may not
// share) goes to standard error — unless asked not to: --no-workdir-warning,
// or DP_NO_WORKDIR_WARNING in dp's shell.
func TestJobSubmitWarnings(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/jobs"] = map[string]any{"id": "abc12", "warnings": []string{"the job will run in /root, which the machines may not share"}}
	if code, _, errs := f.run("job submit -d --from-devpod alice-a -- true"); code != 0 || !strings.Contains(errs, "warning: the job will run in /root") || !strings.Contains(errs, "--no-workdir-warning") {
		t.Fatalf("warned: %d %q", code, errs)
	}
	if code, _, errs := f.run("job submit -d --from-devpod alice-a --no-workdir-warning -- true"); code != 0 || strings.Contains(errs, "warning") {
		t.Fatalf("asked not to: %d %q", code, errs)
	}
	quiet := &Origin{DevPod: "alice-a", Cwd: "/root", Env: map[string]string{"DP_NO_WORKDIR_WARNING": "1"}}
	if code, _, errs := f.runFrom("job submit -d -- true", quiet, nil); code != 0 || strings.Contains(errs, "warning") {
		t.Fatalf("its shell asked not to: %d %q", code, errs)
	}
}

// runWaitFrom runs line from the DevPod o says, waiting as srun does.
func (f *fakeAPI) runWaitFrom(line string, o *Origin) (int, string, string) {
	f.t.Helper()
	argv, err := Split(line)
	if err != nil {
		f.t.Fatal(err)
	}
	var out, errw syncBuffer
	code := Run(&Env{Ctx: context.Background(), Session: context.Background(), User: "alice", API: NewAPI(f.srv.URL, "k3y"), Out: &out, Err: &errw, Origin: o,
		Now: func() time.Time { return time.Date(2026, 11, 27, 4, 0, 0, 0, time.UTC) }}, argv)
	return code, out.String(), errw.String()
}

// attachingOrigin: a DevPod's dp that can attach (on a terminal or not),
// the attaches it is told to make kept.
func attachingOrigin(tty bool) (*Origin, func() []AttachTo) {
	var mu sync.Mutex
	var told []AttachTo
	o := &Origin{Cluster: "st", DevPod: "alice-a", Cwd: "/home/alice", TTY: tty, Term: "xterm-256color", Attach: func(a AttachTo) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, a)
	}}
	return o, func() []AttachTo {
		mu.Lock()
		defer mu.Unlock()
		return append([]AttachTo(nil), told...)
	}
}

// From a dp that can attach, a waiting submit asks its ranks to wait for
// the attach (stdin; tty with --pty) and, once they run, tells dp to
// attach — rather than follow their logs — then ends with the job's code.
func TestJobSubmitAttaches(t *testing.T) {
	saved := waitPoll
	waitPoll = 20 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	for _, c := range []struct {
		line, attach string
		tty          bool
	}{
		{"job submit --gpus 1 -- python t.py", "stdin", false},
		{"job submit --pty --gpus 1 -- bash", "tty", true},
		{"srun -N 1 --gpus-per-node 1 -t 10 --pty bash -l", "tty", true},
	} {
		f := newFakeAPI(t)
		f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 1}
		n := 0
		f.dyn["GET /api/jobs/abc12"] = func() any {
			n++
			if n < 4 {
				return map[string]any{"id": "abc12", "cluster": "st", "phase": "Running", "nodes": 1, "attempts": 1, "ranks": []any{map[string]any{"rank": 0, "node": "m800"}}}
			}
			return map[string]any{"id": "abc12", "cluster": "st", "phase": "Failed", "nodes": 1, "detail": map[string]any{"exit": "4"}}
		}
		o, told := attachingOrigin(c.tty)
		code, _, errs := f.runWaitFrom(c.line, o)
		if code != 4 {
			t.Fatalf("%s: %d %q", c.line, code, errs)
		}
		b := lastBody(t, f, "POST", "/api/clusters/st/jobs")
		if b["attach"] != c.attach {
			t.Fatalf("%s: asked attach %v", c.line, b["attach"])
		}
		// a terminal job's terminal is the caller's kind (TERM); others
		// take none along
		env, _ := b["env"].(map[string]any)
		if term := env["TERM"]; (c.attach == "tty") != (term == "xterm-256color") {
			t.Fatalf("%s: TERM %v", c.line, term)
		}
		if got := told(); len(got) != 1 || got[0].Job != "abc12" || got[0].Nodes != 1 || got[0].TTY != c.tty {
			t.Fatalf("%s: told %+v", c.line, got)
		}
		f.mu.Lock()
		followed := slices.ContainsFunc(f.calls, func(s string) bool { return strings.Contains(s, "/logs?") })
		f.mu.Unlock()
		if followed {
			t.Fatalf("%s: followed the logs as well", c.line)
		}
	}
}

// --pty needs a terminal and a dp to attach with; -d attaches to nothing.
func TestJobSubmitPtyNeeds(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 1}
	notTTY, _ := attachingOrigin(false)
	if code, _, errs := f.runWaitFrom("job submit --pty -- bash", notTTY); code != ExitUsage || !strings.Contains(errs, "terminal") {
		t.Fatalf("no terminal: %d %q", code, errs)
	}
	if code, _, errs := f.runWaitFrom("job submit --pty -- bash", &Origin{Cluster: "st", DevPod: "alice-a", TTY: true}); code != ExitUsage || !strings.Contains(errs, "dp") {
		t.Fatalf("no dp to attach with: %d %q", code, errs)
	}
	o, told := attachingOrigin(true)
	if code, _, errs := f.runWaitFrom("job submit -d -- true", o); code != 0 || len(told()) != 0 {
		t.Fatalf("-d: %d %q %v", code, errs, told())
	}
	if b := lastBody(t, f, "POST", "/api/clusters/st/jobs"); b["attach"] != nil {
		t.Fatalf("-d asked attach %v", b["attach"])
	}
}

// job submit asks for ranks on each machine, packed ranks and a GPU
// model; what the console answers the ranks to be is what dp attaches to.
func TestJobSubmitTasksBody(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 1}
	if code, _, errs := f.runWaitFrom("job submit -d -N 1 --ntasks-per-node 2 --gpus 4090:1 -- x", origin); code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	b := lastBody(t, f, "POST", "/api/clusters/st/jobs")
	if b["nodes"] != float64(1) || b["tasksPerNode"] != float64(2) || b["gpus"] != float64(1) || b["gpuModel"] != "4090" {
		t.Fatalf("body %v", b)
	}
	if code, _, errs := f.runWaitFrom("job submit -d -n 3 --gpus 1 -- x", origin); code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if b := lastBody(t, f, "POST", "/api/clusters/st/jobs"); b["nodes"] != float64(0) || b["tasks"] != float64(3) {
		t.Fatalf("packed body %v", b)
	}
	saved := waitPoll
	waitPoll = 20 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	g := newFakeAPI(t)
	g.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued", "nodes": 1, "tasks": 2}
	n := 0
	g.dyn["GET /api/jobs/abc12"] = func() any {
		n++
		if n < 3 {
			return map[string]any{"id": "abc12", "cluster": "st", "phase": "Running", "nodes": 1, "tasks": 2, "attempts": 1}
		}
		return map[string]any{"id": "abc12", "cluster": "st", "phase": "Succeeded", "nodes": 1, "tasks": 2}
	}
	o, told := attachingOrigin(false)
	if code, _, errs := g.runWaitFrom("job submit -N 1 --ntasks-per-node 2 --gpus 1 -- x", o); code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if got := told(); len(got) != 1 || got[0].Nodes != 2 {
		t.Fatalf("told %+v", got)
	}
}

// job free: each cluster's free cards, by model.
func TestJobFree(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["GET /api/jobs/free"] = map[string]any{"items": []any{map[string]any{"cluster": "zjusct", "gpu": 4, "cpu": 512, "maxGPU": 4, "machines": 9,
		"models": []any{map[string]any{"model": "NVIDIA-GeForce-RTX-4090", "gpu": 4, "total": 4, "machines": 1}, map[string]any{"model": "NVIDIA-A100-80GB-PCIe", "gpu": 0, "total": 3, "machines": 3}}}}}
	code, out, errs := f.run("job free")
	if code != 0 || !strings.Contains(out, "zjusct") || !strings.Contains(out, "NVIDIA-GeForce-RTX-4090") || !strings.Contains(out, "4 free of 4") || !strings.Contains(out, "0 free of 3") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
}

// A packed job waiting for room says how many of its ranks fit, not how
// many machines.
func TestJobStatePackedCapacity(t *testing.T) {
	j := jobView{Phase: "Queued", Position: 1, Reason: "capacity", Nodes: 0, Tasks: 6, GPUs: 1,
		Detail: map[string]string{"need": "6", "free": "4", "fit": "4"}}
	if s := jobState(j, time.Now()); !strings.Contains(s, "6 ranks × 1 GPU") || !strings.Contains(s, "4 ranks fit") {
		t.Fatalf("%q", s)
	}
}

// job submit's --gpus and --gpus-per-task are the same thing: both,
// disagreeing, are refused.
func TestJobSubmitTwoGPUOptions(t *testing.T) {
	f := newFakeAPI(t)
	if code, _, errs := f.runWaitFrom("job submit -d --gpus 4 --gpus-per-task 1 -- x", origin); code == 0 || !strings.Contains(errs, "one way") {
		t.Fatalf("%d %s", code, errs)
	}
}
