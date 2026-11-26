// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// startedJob: user's job of 1 × gpus on the only machine, Starting.
func startedJob(t *testing.T, p *placeRig, user string, gpus int, preemptible bool) string {
	t.Helper()
	id := submit(t, p, user, 1, gpus, map[string]any{"timeLimitSeconds": 7200, "preemptible": preemptible})
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranksBecome(t, id, 1)
	return id
}

// A job that may not be stopped holds what it runs on, for as long as it
// may run: a reservation there and then must leave it room.
func TestReservationBlockedByJob(t *testing.T) {
	p, _ := queueRig(t, "rj", "rj-alice")
	startedJob(t, p, "rj-alice", 4, false)
	bob := p.as("rj-bob")
	if code, v := reserveAs(t, p.s, bob, p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusConflict || v.Code != "NO_ROOM" {
		t.Fatalf("whole over a running job: %d %+v", code, v)
	}
	if code, v := reserveWith(t, p.s, bob, p.n1, p.slot, p.slot.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("the other half: %d %+v", code, v)
	}
}

// A preemptible job holds nothing against a reservation.
func TestReservationNotBlockedByPreemptibleJob(t *testing.T) {
	p, _ := queueRig(t, "rn", "rn-alice")
	startedJob(t, p, "rn-alice", 4, true)
	if code, v := reserveAs(t, p.s, p.as("rn-bob"), p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("whole over a preemptible job: %d %+v", code, v)
	}
}

// A reservation starting stops a preemptible job in its way at once — it
// waits in the queue again — with no notice marks.
func TestReservationStartRequeuesPreemptible(t *testing.T) {
	p, _ := queueRig(t, "rq", "rq-alice")
	id := startedJob(t, p, "rq-alice", 4, true)
	rank := ranksBecome(t, id, 1)[0].Name
	if code, v := reserveAs(t, p.s, p.as("rq-bob"), p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	ctx := context.Background()
	waitFor(t, func() bool {
		p.s.EnforceReservations(ctx)
		j := jobNow(t, id)
		return j.Status.Phase == devpodv1alpha1.JobQueued && j.Status.Reason == "preempted"
	})
	ranksBecome(t, id, 0)
	var dp devpodv1alpha1.DevPod
	if err := p.s.Reader.Get(ctx, client_key(rank), &dp); err == nil {
		if _, marked := dp.Annotations[devpodv1alpha1.ClearAtAnnotation]; marked {
			t.Fatal("the rank was marked, not requeued")
		}
	}
}

func client_key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "devpods", Name: name}
}
