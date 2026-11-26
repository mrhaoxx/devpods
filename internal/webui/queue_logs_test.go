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
	objs.mu.Lock()
	got := string(objs.m[key])
	objs.mu.Unlock()
	if got != "output of "+r.Name+"\nline two\n" {
		t.Fatalf("kept %q", got)
	}
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
