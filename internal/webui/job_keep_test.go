// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// jobGone: job id is no longer in the cluster.
func jobGone(t *testing.T, id string) bool {
	t.Helper()
	var j devpodv1alpha1.Job
	err := k8sManager.GetAPIReader().Get(context.Background(), types.NamespacedName{Name: id}, &j)
	return apierrors.IsNotFound(err)
}

// endWaiting submits user's job (command cmd) and cancels it while it
// waits: ended, with no ranks to clean up.
func endWaiting(t *testing.T, p *placeRig, user, cmd string) string {
	t.Helper()
	id := submit(t, p, user, 1, 8, map[string]any{"command": cmd})
	if rec := route(t, p.s, "DELETE", "/api/jobs/"+id, p.as(user), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	return id
}

// An ended job leaves the cluster once the audit store keeps it — whole:
// it is still found, listed among the ended, shown with its command and
// its history, and submitted again from what was kept; it changes no more.
func TestEndedJobKeptThenReleased(t *testing.T) {
	p := jobRig(t, "ka", "ka-alice")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	ctx := context.Background()
	id := endWaiting(t, p, "ka-alice", "python keep.py")
	waitFor(t, func() bool {
		p.s.QueueRound(ctx)
		return jobGone(t, id)
	})
	if kept := f.keptJobs(); len(kept) != 1 || kept[0].ID != id || kept[0].Phase != "Cancelled" || kept[0].Owner != "ka-alice" {
		t.Fatalf("kept %+v", kept)
	}
	alice := p.as("ka-alice")
	rec := route(t, p.s, "GET", "/api/jobs/"+id, alice, "")
	var v jobV
	if _ = json.Unmarshal(rec.Body.Bytes(), &v); rec.Code != http.StatusOK || v.Phase != "Cancelled" || v.Command != "python keep.py" {
		t.Fatalf("shown: %d %s", rec.Code, rec.Body)
	}
	listed := false
	for _, j := range listJobs(t, p.s, alice, "?ended=1") {
		listed = listed || j.ID == id
	}
	if !listed {
		t.Fatal("not among the ended")
	}
	if code, v := patchJob(t, p, "ka-alice", false, id, `{"priority":1}`); code != http.StatusConflict || v.Code != "JOB_ENDED" {
		t.Fatalf("changed: %d %+v", code, v)
	}
	if rec := route(t, p.s, "GET", "/api/jobs/"+id+"/history", alice, ""); rec.Code != http.StatusOK {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	rec = route(t, p.s, "POST", "/api/jobs/"+id+"/resubmit", alice, "")
	var again jobV
	if _ = json.Unmarshal(rec.Body.Bytes(), &again); rec.Code != http.StatusCreated || again.ID == id || again.Command != "python keep.py" {
		t.Fatalf("submitted again: %d %s", rec.Code, rec.Body)
	}
}

// A job the store does not take stays in the cluster until it does.
func TestEndedJobStaysUntilKept(t *testing.T) {
	p := jobRig(t, "kb", "kb-alice")
	f := &fakeAuditStore{jobErrs: 1}
	p.s.Audit = &webui.AuditDeps{Store: f}
	ctx := context.Background()
	id := endWaiting(t, p, "kb-alice", "true")
	waitFor(t, func() bool {
		p.s.QueueRound(ctx)
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.jobErrs == 0
	})
	if jobGone(t, id) {
		t.Fatal("released though the store did not keep it")
	}
	waitFor(t, func() bool {
		p.s.QueueRound(ctx)
		return jobGone(t, id)
	})
	if kept := f.keptJobs(); len(kept) != 1 || kept[0].ID != id {
		t.Fatalf("kept %+v", kept)
	}
}

// With no audit store to keep it, an ended job stays in the cluster.
func TestEndedJobStaysWithoutStore(t *testing.T) {
	p := jobRig(t, "kc", "kc-alice")
	ctx := context.Background()
	id := endWaiting(t, p, "kc-alice", "true")
	for range 3 {
		p.s.QueueRound(ctx)
	}
	if jobGone(t, id) {
		t.Fatal("an ended job went with nowhere to keep it")
	}
}
