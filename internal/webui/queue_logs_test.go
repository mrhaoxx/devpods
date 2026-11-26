// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// memObjects: an object store kept in memory.
type memObjects struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (o *memObjects) Put(_ context.Context, key string, body io.Reader, _ int64) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.m == nil {
		o.m = map[string][]byte{}
	}
	o.m[key] = b
	return nil
}

func (o *memObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.m[key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// fakeLogs: each rank's Pod says "output of <pod>" over two lines.
func fakeLogs(_ context.Context, _, _, pod, _ string, _ bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("output of " + pod + "\nline two\n")), nil
}

// A job's ranks' output is kept when it ends, where the job says.
func TestSaveRankLogs(t *testing.T) {
	p, _ := queueRig(t, "ql", "ql-alice")
	objs := &memObjects{}
	p.s.Audit = &webui.AuditDeps{Store: &fakeAuditStore{}, Objects: objs}
	t.Cleanup(func() { p.s.Audit = nil })
	p.s.PodLogs = fakeLogs
	id := submit(t, p, "ql-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	r := ranksBecome(t, id, 1)[0]
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	zero := int32(0)
	setRank(t, r.Name, devpodv1alpha1.DevPodSucceeded, &zero)
	j := roundUntil(t, p, id, devpodv1alpha1.JobSucceeded)
	key := "jobs/-/" + id + "/rank-0.log"
	if j.Status.Ranks[0].Log != key {
		t.Fatalf("rank log key %q", j.Status.Ranks[0].Log)
	}
	// kept in the background, a moment after
	waitFor(t, func() bool {
		objs.mu.Lock()
		defer objs.mu.Unlock()
		return string(objs.m[key]) == "output of "+r.Name+"\nline two\n"
	})
}

// A rank's output: live while it runs, kept after; its owner's only.
func TestJobLogsEndpoint(t *testing.T) {
	p, _ := queueRig(t, "qo", "qo-alice")
	objs := &memObjects{}
	p.s.Audit = &webui.AuditDeps{Store: &fakeAuditStore{}, Objects: objs}
	t.Cleanup(func() { p.s.Audit = nil })
	p.s.PodLogs = fakeLogs
	id := submit(t, p, "qo-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	r := ranksBecome(t, id, 1)[0]
	logs := func(user, query string) (int, string) {
		rec := doJSON(t, p.s.HandleJobLogsForTest(), "GET", "/api/jobs/"+id+"/logs"+query, map[string]string{"id": id}, p.as(user), "")
		return rec.Code, rec.Body.String()
	}
	if code, body := logs("qo-alice", "?rank=0&follow=1"); code != http.StatusOK || !strings.Contains(body, "data: output of "+r.Name) || !strings.Contains(body, "data: line two") {
		t.Fatalf("live: %d %q", code, body)
	}
	if code, _ := logs("qo-bob", "?rank=0"); code != http.StatusForbidden {
		t.Fatalf("bob: %d", code)
	}
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	three := int32(3)
	setRank(t, r.Name, devpodv1alpha1.DevPodFailed, &three)
	roundUntil(t, p, id, devpodv1alpha1.JobFailed)
	waitFor(t, func() bool { // kept in the background, a moment after
		objs.mu.Lock()
		defer objs.mu.Unlock()
		_, ok := objs.m["jobs/-/"+id+"/rank-0.log"]
		return ok
	})
	p.s.PodLogs = func(context.Context, string, string, string, string, bool) (io.ReadCloser, error) {
		return nil, errors.New("no such pod")
	}
	if code, body := logs("qo-alice", "?rank=0"); code != http.StatusOK || body != "output of "+r.Name+"\nline two\n" {
		t.Fatalf("kept: %d %q", code, body)
	}
	p.s.Audit = nil
	if code, _ := logs("qo-alice", "?rank=0"); code != http.StatusNotFound {
		t.Fatalf("nothing kept: %d", code)
	}
}

// A start of a job stopped for another (preempted) keeps its output, by
// its number; the logs endpoint reads it with ?attempt=.
func TestRequeueKeepsAttemptOutput(t *testing.T) {
	p, _ := queueRig(t, "qa", "qa-alice")
	objs := &memObjects{}
	p.s.Audit = &webui.AuditDeps{Store: &fakeAuditStore{}, Objects: objs}
	t.Cleanup(func() { p.s.Audit = nil })
	p.s.PodLogs = fakeLogs
	priorityRules(t, map[string]devpodv1alpha1.JobPriorityRuleSpec{"qa-alice": {Condition: "user = qa-alice", MaxPriority: 100}})
	low := submit(t, p, "qa-alice", 1, 8, map[string]any{"preemptible": true})
	roundUntil(t, p, low, devpodv1alpha1.JobStarting)
	r := ranksBecome(t, low, 1)[0]
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, low, devpodv1alpha1.JobRunning)
	submit(t, p, "qa-alice", 1, 8, map[string]any{"priority": 100})
	roundUntil(t, p, low, devpodv1alpha1.JobQueued)
	key := "jobs/-/" + low + "/attempt-1/rank-0.log"
	waitFor(t, func() bool {
		objs.mu.Lock()
		defer objs.mu.Unlock()
		return string(objs.m[key]) == "output of "+r.Name+"\nline two\n"
	})
	rec := doJSON(t, p.s.HandleJobLogsForTest(), "GET", "/api/jobs/"+low+"/logs?rank=0&attempt=1", map[string]string{"id": low}, p.as("qa-alice"), "")
	if rec.Code != http.StatusOK || rec.Body.String() != "output of "+r.Name+"\nline two\n" {
		t.Fatalf("attempt 1: %d %q", rec.Code, rec.Body)
	}
	if rec := doJSON(t, p.s.HandleJobLogsForTest(), "GET", "/api/jobs/"+low+"/logs?rank=0&attempt=9", map[string]string{"id": low}, p.as("qa-alice"), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("attempt 9: %d", rec.Code)
	}
}

// afterGone: a rank's output that goes on until its DevPod is gone (what
// it says while shutting down), then ends.
type afterGone struct {
	t    *testing.T
	name string
	done bool
}

func (a *afterGone) Read(p []byte) (int, error) {
	if a.done {
		return 0, io.EOF
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var dp devpodv1alpha1.DevPod
		if err := k8sManager.GetAPIReader().Get(context.Background(), client_key(a.name), &dp); err != nil || dp.DeletionTimestamp != nil {
			a.done = true
			return copy(p, "saving checkpoint\n"), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, io.EOF
}

// A cancelled job's ranks' output is kept to their end: what they say
// while shutting down too (the stream is opened before they are told to
// stop, and read to its end).
func TestEndKeepsShutdownOutput(t *testing.T) {
	p, _ := queueRig(t, "qe", "qe-alice")
	objs := &memObjects{}
	p.s.Audit = &webui.AuditDeps{Store: &fakeAuditStore{}, Objects: objs}
	t.Cleanup(func() { p.s.Audit = nil })
	opened := make(chan bool, 4)
	p.s.PodLogs = func(_ context.Context, _, _, pod, _ string, follow bool) (io.ReadCloser, error) {
		opened <- follow
		return io.NopCloser(&afterGone{t: t, name: pod}), nil
	}
	id := submit(t, p, "qe-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	r := ranksBecome(t, id, 1)[0]
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	if rec := doJSON(t, p.s.HandleCancelJobForTest(), "DELETE", "/api/jobs/"+id, map[string]string{"id": id}, p.as("qe-alice"), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rec.Code)
	}
	roundUntil(t, p, id, devpodv1alpha1.JobCancelled)
	if follow := <-opened; !follow {
		t.Fatal("the output was read as it was, not followed to its end")
	}
	waitFor(t, func() bool {
		objs.mu.Lock()
		defer objs.mu.Unlock()
		return string(objs.m["jobs/-/"+id+"/rank-0.log"]) == "saving checkpoint\n"
	})
}
