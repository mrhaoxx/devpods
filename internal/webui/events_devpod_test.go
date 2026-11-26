// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// recorded: the events f got, once there are at least n.
func recorded(t *testing.T, f *fakeAuditStore, n int) []store.EventRow {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(f.written()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d events, want %d: %v", len(f.written()), n, actionsOf(f.written()))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f.written()
}

func actionsOf(rows []store.EventRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Action
	}
	return out
}

// What people do to a DevPod is recorded, in order, as theirs, with what
// changed.
func TestEventsDevPodLifecycle(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	f := &fakeAuditStore{}
	s.Audit = &webui.AuditDeps{Store: f}
	t.Cleanup(func() { cleanupDevPods(t) })
	al := forge(sm, "ev-al", false)
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, al, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	dp := waitDevPod(t, "ev-al-dev1")
	name := map[string]string{"name": "ev-al-dev1"}
	// made running: stopped, started, given more cores
	for _, body := range []string{`{"running":false}`, `{"running":true}`, `{"cpu":"4"}`} {
		if rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/ev-al-dev1", name, al, body); rec.Code != http.StatusOK {
			t.Fatalf("patch %s: %d %s", body, rec.Code, rec.Body)
		}
		time.Sleep(300 * time.Millisecond) // the cache catches up
	}
	if rec := doJSON(t, s.HandleDeleteDevPodForTest(), "DELETE", "/api/devpods/ev-al-dev1", name, al, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rows := recorded(t, f, 5)
	if got := strings.Join(actionsOf(rows), ","); got != "create,stop,start,edit,delete" {
		t.Fatalf("actions %s", got)
	}
	for _, r := range rows {
		if r.Kind != "devpod" || r.Object != "ev-al-dev1" || r.ObjectUID != string(dp.UID) || r.Owner != "ev-al" || r.Actor != "ev-al" || r.Via != "web" {
			t.Fatalf("row %+v", r)
		}
	}
	if !strings.Contains(rows[0].Detail, "running") || !strings.Contains(rows[3].Detail, "cpu 2→4") {
		t.Fatalf("details: create %q, edit %q", rows[0].Detail, rows[3].Detail)
	}
}

// What an edit changed names environment variables, never their values.
func TestEventsEditHidesEnvValues(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	f := &fakeAuditStore{}
	s.Audit = &webui.AuditDeps{Store: f}
	t.Cleanup(func() { cleanupDevPods(t) })
	al := forge(sm, "ev-env", false)
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, al, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	name := map[string]string{"name": "ev-env-dev1"}
	for _, v := range []string{"s3cret-one", "s3cret-two"} {
		dp := waitDevPod(t, "ev-env-dev1")
		dp.Spec.Pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "FOO", Value: v}}
		text, _ := yaml.Marshal(&dp)
		body, _ := json.Marshal(map[string]string{"yaml": string(text)})
		if rec := doJSON(t, s.HandlePutDevPodYAMLForTest(), "PUT", "/api/devpods/ev-env-dev1/yaml", name, al, string(body)); rec.Code != http.StatusOK {
			t.Fatalf("yaml: %d %s", rec.Code, rec.Body)
		}
		waitFor(t, func() bool {
			d := waitDevPod(t, "ev-env-dev1")
			for _, e := range d.Spec.Pod.Spec.Containers[0].Env {
				if e.Name == "FOO" && e.Value == v {
					return true
				}
			}
			return false
		})
	}
	rows := recorded(t, f, 3)
	last := rows[len(rows)-1]
	if last.Action != "yaml" || !strings.Contains(last.Detail, "env.FOO") || strings.Contains(last.Detail, "s3cret") {
		t.Fatalf("yaml row %+v", last)
	}
}

// An admin acting on someone's DevPod is the actor; it stays the owner's.
func TestEventsAdminActsOnOthers(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	f := &fakeAuditStore{}
	s.Audit = &webui.AuditDeps{Store: f}
	s.Admins = map[string]bool{"ev-root": true}
	t.Cleanup(func() { cleanupDevPods(t) })
	bob := forge(sm, "ev-bob", false)
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, bob, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "ev-bob-dev1")
	name := map[string]string{"name": "ev-bob-dev1"}
	root := forge(sm, "ev-root", true)
	if rec := doJSON(t, s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/ev-bob-dev1", name, root, `{"running":false}`); rec.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body)
	}
	rows := recorded(t, f, 2)
	if r := rows[1]; r.Action != "stop" || r.Actor != "ev-root" || r.Owner != "ev-bob" {
		t.Fatalf("row %+v", r)
	}
}

// A reservation's clearing records the DevPods it hibernated, as the
// system's doing, and for which reservation.
func TestEventsReservationHibernate(t *testing.T) {
	p, clock := clearRig(t, "ce")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	ctx := context.Background()
	p.createGPU("ce-bob", "g2", 2, p.n1, http.StatusCreated)
	start := clock.Add(20 * time.Minute)
	code, r := reserveAs(t, p.s, p.as("ce-alice"), p.n1, start, start.Add(time.Hour))
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, r)
	}
	p.s.EnforceReservations(ctx)
	*clock = clock.Add(30 * time.Minute)
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "ce-bob-g2", "hibernated", func(dp *devpodv1alpha1.DevPod) bool { return !dp.Spec.Running })
	waitFor(t, func() bool {
		for _, e := range f.written() {
			if e.Action == "hibernate" && e.Object == "ce-bob-g2" && e.Actor == "system:reservation" && e.Via == "system" && strings.Contains(e.Reason, r.ID) {
				return true
			}
		}
		return false
	})
}
