// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// patchJob PATCHes job id as user (an admin's cookie when admin) with body.
func patchJob(t *testing.T, p *placeRig, user string, admin bool, id, body string) (int, jobV) {
	t.Helper()
	rec := route(t, p.s, "PATCH", "/api/jobs/"+id, forge(p.sm, user, admin), body)
	var v jobV
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, v
}

// A job's owner changes its priority within their cap — up as well as
// down, waiting or running; an admin past any cap; nobody else; an ended
// job's stays. Each change is recorded.
func TestJobPriorityChange(t *testing.T) {
	p := jobRig(t, "pc", "pc-alice")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	priorityRules(t, map[string]devpodv1alpha1.JobPriorityRuleSpec{"pc-alice": {Condition: "user = pc-alice", MaxPriority: 5}})
	id := submit(t, p, "pc-alice", 1, 8, nil)
	for _, c := range []struct {
		user     string
		admin    bool
		body     string
		want     int
		code     string
		priority int32
	}{
		{"pc-alice", false, `{"priority":5}`, http.StatusOK, "", 5},
		{"pc-alice", false, `{"priority":6}`, http.StatusForbidden, "PRIORITY_TOO_HIGH", 0},
		{"pc-alice", false, `{"priority":-1}`, http.StatusBadRequest, "", 0},
		{"pc-bob", false, `{"priority":1}`, http.StatusForbidden, "FORBIDDEN", 0},
		{"pc-root", true, `{"priority":50}`, http.StatusOK, "", 50},
		{"pc-alice", false, `{"priority":1}`, http.StatusOK, "", 1},
	} {
		code, v := patchJob(t, p, c.user, c.admin, id, c.body)
		if code != c.want || (c.code != "" && v.Code != c.code) || (code == http.StatusOK && v.Priority != c.priority) {
			t.Fatalf("%s %s: %d %+v", c.user, c.body, code, v)
		}
	}
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	if code, v := patchJob(t, p, "pc-alice", false, id, `{"priority":3}`); code != http.StatusOK || v.Priority != 3 || jobNow(t, id).Spec.Priority != 3 {
		t.Fatalf("running: %d %+v", code, v)
	}
	rows := eventsOf(t, f, id, "submit", "priority", "priority", "priority", "start", "priority")
	if rows[1].Detail != "0→5" || rows[2].Actor != "pc-root" || rows[5].Detail != "1→3" {
		t.Fatalf("records %+v", rows)
	}
	ended := submit(t, p, "pc-alice", 1, 1, nil)
	if rec := route(t, p.s, "DELETE", "/api/jobs/"+ended, p.as("pc-alice"), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if code, v := patchJob(t, p, "pc-alice", false, ended, `{"priority":2}`); code != http.StatusConflict || v.Code != "JOB_ENDED" {
		t.Fatalf("ended: %d %+v", code, v)
	}
}

// A held job waits out of the queue's order — it holds no machine, and a
// job behind it starts — until it is released; only a waiting job is
// held.
func TestJobHoldRelease(t *testing.T) {
	p := jobRig(t, "ph", "ph-alice")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	ctx := context.Background()
	id := submit(t, p, "ph-alice", 1, 8, nil)
	if code, v := patchJob(t, p, "ph-alice", false, id, `{"held":true}`); code != http.StatusOK || !v.Held {
		t.Fatalf("hold: %d %+v", code, v)
	}
	p.s.QueueRound(ctx)
	if j := jobNow(t, id); j.Status.Phase != devpodv1alpha1.JobQueued || j.Status.Reason != "held" || j.Status.Position != 0 {
		t.Fatalf("held: %+v", j.Status)
	}
	next := submit(t, p, "ph-alice", 1, 8, nil)
	roundUntil(t, p, next, devpodv1alpha1.JobStarting)
	if j := jobNow(t, id); j.Status.Phase != devpodv1alpha1.JobQueued || j.Status.Reason != "held" {
		t.Fatalf("started though held: %+v", j.Status)
	}
	if code, v := patchJob(t, p, "ph-alice", false, next, `{"held":true}`); code != http.StatusConflict || v.Code != "JOB_NOT_WAITING" {
		t.Fatalf("holding a running job: %d %+v", code, v)
	}
	if code, v := patchJob(t, p, "ph-alice", false, id, `{"held":false}`); code != http.StatusOK || v.Held {
		t.Fatalf("release: %d %+v", code, v)
	}
	waitFor(t, func() bool {
		p.s.QueueRound(ctx)
		j := jobNow(t, id)
		return j.Status.Reason != "held" && j.Status.Position >= 1
	})
	eventsOf(t, f, id, "submit", "hold", "release")
}

// A DevPod waiting in line has its priority changed within its owner's
// cap; one not in line has none to change.
func TestDevPodQueuePriorityChange(t *testing.T) {
	p := jobRig(t, "pq", "")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	priorityRules(t, map[string]devpodv1alpha1.JobPriorityRuleSpec{"pq-alice": {Condition: "user = pq-alice", MaxPriority: 4}})
	name := p.create("pq-alice", http.StatusCreated)
	if code, _, ec := patchAs(t, p, "pq-alice", name, `{"priority":4}`); code != http.StatusOK {
		t.Fatalf("4: %d %q", code, ec)
	}
	devPodWhen(t, p, name, "at 4", func(dp *devpodv1alpha1.DevPod) bool {
		return dp.Annotations[devpodv1alpha1.QueuePriorityAnnotation] == "4"
	})
	if code, _, ec := patchAs(t, p, "pq-alice", name, `{"priority":5}`); code != http.StatusForbidden || ec != "PRIORITY_TOO_HIGH" {
		t.Fatalf("5 over the cap: %d %q", code, ec)
	}
	p.s.QueueRound(context.Background())
	devPodWhen(t, p, name, "started", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running })
	if code, _, ec := patchAs(t, p, "pq-alice", name, `{"priority":1}`); code != http.StatusConflict || ec != "NOT_QUEUED" {
		t.Fatalf("running: %d %q", code, ec)
	}
	if rows := eventsOf(t, f, name, "create", "queue-priority", "queue-start"); rows[1].Detail != "0→4" {
		t.Fatalf("records %+v", rows)
	}
}
