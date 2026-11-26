// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// eventsOf: the events of object, once its actions so far are want.
func eventsOf(t *testing.T, f *fakeAuditStore, object string, want ...string) []store.EventRow {
	t.Helper()
	var rows []store.EventRow
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows = rows[:0]
		for _, e := range f.written() {
			if e.Object == object {
				rows = append(rows, e)
			}
		}
		if strings.Join(actionsOf(rows), ",") == strings.Join(want, ",") {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: actions %v, want %v", object, actionsOf(rows), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A job's life is recorded: who submitted it, where the queue started
// it, how it ended.
func TestEventsJobLifecycle(t *testing.T) {
	p, _ := queueRig(t, "ej", "ej-alice")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	id := submit(t, p, "ej-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	r := ranksBecome(t, id, 1)[0]
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	zero := int32(0)
	setRank(t, r.Name, devpodv1alpha1.DevPodSucceeded, &zero)
	roundUntil(t, p, id, devpodv1alpha1.JobSucceeded)
	rows := eventsOf(t, f, id, "submit", "start", "end")
	node := r.Spec.Pod.Spec.NodeSelector["kubernetes.io/hostname"]
	switch {
	case rows[0].Kind != "job" || rows[0].Actor != "ej-alice" || rows[0].Owner != "ej-alice" || !strings.Contains(rows[0].Detail, "1×1"):
		t.Fatalf("submit %+v", rows[0])
	case rows[1].Actor != "system:queue" || rows[1].Via != "system" || !strings.Contains(rows[1].Detail, node):
		t.Fatalf("start %+v (node %s)", rows[1], node)
	case rows[2].Actor != "system:queue" || !strings.Contains(rows[2].Detail, "Succeeded"):
		t.Fatalf("end %+v", rows[2])
	}
}

// A job stopped for another is recorded as preempted, then as waiting
// again; its owner's cancel of it is the owner's.
func TestEventsJobPreempted(t *testing.T) {
	p, _ := queueRig(t, "eq", "eq-alice")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	priorityRules(t, map[string]devpodv1alpha1.JobPriorityRuleSpec{"eq-alice": {Condition: "user = eq-alice", MaxPriority: 100}})
	low := submit(t, p, "eq-alice", 1, 8, map[string]any{"preemptible": true})
	roundUntil(t, p, low, devpodv1alpha1.JobStarting)
	setRank(t, ranksBecome(t, low, 1)[0].Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, low, devpodv1alpha1.JobRunning)
	high := submit(t, p, "eq-alice", 1, 8, map[string]any{"priority": 100})
	roundUntil(t, p, low, devpodv1alpha1.JobQueued)
	rows := eventsOf(t, f, low, "submit", "start", "preempted", "requeue")
	if !strings.Contains(rows[2].Detail, high) || rows[3].Reason != "preempted" {
		t.Fatalf("preempted %+v, requeue %+v", rows[2], rows[3])
	}
	if rec := doJSON(t, p.s.HandleCancelJobForTest(), "DELETE", "/api/jobs/"+high, map[string]string{"id": high}, p.as("eq-alice"), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rec.Code)
	}
	if rows := eventsOf(t, f, high, "submit", "cancel"); rows[1].Actor != "eq-alice" {
		t.Fatalf("cancel %+v", rows[1])
	}
}

// Who switched a machine's reservations on or off is recorded.
func TestEventsReservableSwitch(t *testing.T) {
	s, sm, _, _ := rsvRig(t, "es")
	f := &fakeAuditStore{}
	s.Audit = &webui.AuditDeps{Store: f}
	s.Admins = map[string]bool{"es-root": true}
	put := func(v bool) int {
		body := `{"reservable":false}`
		if v {
			body = `{"reservable":true}`
		}
		return doJSON(t, s.Routes().ServeHTTP, "PUT", "/api/admin/nodes/es-n2/reservable", nil, forge(sm, "es-root", true), body).Code
	}
	if put(true) != http.StatusOK || put(false) != http.StatusOK {
		t.Fatal("switch")
	}
	rows := eventsOf(t, f, "es-n2", "reservable-on", "reservable-off")
	if r := rows[1]; r.Kind != "machine" || r.Actor != "es-root" || r.Node != "es-n2" {
		t.Fatalf("row %+v", r)
	}
}
