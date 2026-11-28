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

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Where reservations of part of a machine are not allowed, a reservation
// is the whole machine: amounts are refused, saying so, and reservations
// of one machine do not overlap; allowed, amounts are taken; turned off
// after, a reservation made of part keeps it, and may still be changed.
func TestReservationsWholeUnlessAllowed(t *testing.T) {
	s, sm, _, node := rsvRig(t, "rw")
	s.Reservations.Partial = false
	alice, bob := forge(sm, "rw-alice", false), forge(sm, "rw-bob", false)
	slot := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	partial := func() bool {
		rec := doJSON(t, s.HandleListReservationsForTest(), "GET", "/api/reservations", nil, alice, "")
		var out struct{ Partial map[string]bool }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		v, ok := out.Partial[""]
		if !ok {
			t.Fatalf("the timeline says nothing of partial reservations: %s", rec.Body)
		}
		return v
	}
	if partial() {
		t.Fatal("the timeline says partial reservations are on")
	}
	if code, v := reserveWith(t, s, alice, node, slot, slot.Add(time.Hour), map[string]any{"gpu": 2}); code != http.StatusBadRequest || !strings.Contains(v.Message, "whole") {
		t.Fatalf("2 cards while off: %d %+v", code, v)
	}
	code, a := reserveWith(t, s, alice, node, slot, slot.Add(time.Hour), nil)
	if code != http.StatusCreated {
		t.Fatalf("a whole one: %d %+v", code, a)
	}
	if code, v := reserveWith(t, s, bob, node, slot.Add(30*time.Minute), slot.Add(2*time.Hour), nil); code != http.StatusConflict {
		t.Fatalf("one over it: %d %+v", code, v)
	}
	// allowed: a part of the machine, beside another part
	s.Reservations.Partial = true
	if !partial() {
		t.Fatal("the timeline says partial reservations are off")
	}
	later := slot.Add(3 * time.Hour)
	code, b := reserveWith(t, s, bob, node, later, later.Add(time.Hour), map[string]any{"gpu": 2})
	if code != http.StatusCreated {
		t.Fatalf("2 cards while on: %d %+v", code, b)
	}
	// turned off: its share stays, and a change that says no amount is had
	s.Reservations.Partial = false
	rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+b.ID, map[string]string{"id": b.ID}, bob, `{"purpose":"eval"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a purpose for the partial one: %d %s", rec.Code, rec.Body)
	}
	var got devpodv1alpha1.NodeReservation
	if err := k8sManager.GetAPIReader().Get(context.Background(), client_key_cluster(b.ID), &got); err != nil || got.Spec.GPU == nil || *got.Spec.GPU != 2 {
		t.Fatalf("its share after the change: %+v %v", got.Spec, err)
	}
	rec = doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+b.ID, map[string]string{"id": b.ID}, bob, `{"gpu":4}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "whole") {
		t.Fatalf("new amounts while off: %d %s", rec.Code, rec.Body)
	}
}

// Where parts are not allowed, one made of part before may still be made
// the whole machine (whole: true).
func TestReservationMadeWholeWhileOff(t *testing.T) {
	s, sm, _, node := rsvRig(t, "rx")
	bob := forge(sm, "rx-bob", false)
	slot := time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)
	code, b := reserveWith(t, s, bob, node, slot, slot.Add(time.Hour), map[string]any{"gpu": 2})
	if code != http.StatusCreated {
		t.Fatalf("2 cards while on: %d %+v", code, b)
	}
	s.Reservations.Partial = false
	rec := doJSON(t, s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+b.ID, map[string]string{"id": b.ID}, bob, `{"whole":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("made whole while off: %d %s", rec.Code, rec.Body)
	}
	var got devpodv1alpha1.NodeReservation
	if err := k8sManager.GetAPIReader().Get(context.Background(), client_key_cluster(b.ID), &got); err != nil || got.Spec.GPU != nil || got.Spec.CPU != nil {
		t.Fatalf("after: %+v %v", got.Spec, err)
	}
}
