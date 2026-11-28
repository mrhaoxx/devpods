// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
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

// A reservation's end hibernates its holder's DevPods on the machine where
// they may run only under one — but not their job's ranks, which the
// queue ran outside any reservation.
func TestReservationEndSparesJobRanks(t *testing.T) {
	p, _ := queueRig(t, "rs", "rs-alice")
	setRules(t, onNode(p.n1, needsReservation))
	if code, v := reserveWith(t, p.s, p.as("rs-alice"), p.n1, p.slot, p.slot.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	id := startedJob(t, p, "rs-alice", 4, false)
	rank := ranksBecome(t, id, 1)[0].Name
	setRank(t, rank, devpodv1alpha1.DevPodRunning, nil)
	end := p.slot.Add(time.Hour + time.Minute)
	p.s.Reservations.Now = func() time.Time { return end }
	ctx := context.Background()
	for range 4 {
		p.s.EnforceReservations(ctx)
		time.Sleep(100 * time.Millisecond)
	}
	var dp devpodv1alpha1.DevPod
	if err := p.s.Reader.Get(ctx, client_key(rank), &dp); err != nil {
		t.Fatal(err)
	}
	if !dp.Spec.Running || dp.Annotations[devpodv1alpha1.HibernatedByAnnotation] != "" {
		t.Fatalf("the rank was stopped: running %v by %q", dp.Spec.Running, dp.Annotations[devpodv1alpha1.HibernatedByAnnotation])
	}
}

// The queue says what it can use now, and the reservation timeline shows
// what running jobs hold, until when at most.
func TestJobsFreeAndHolds(t *testing.T) {
	p, _ := queueRig(t, "rf", "rf-alice")
	id := startedJob(t, p, "rf-alice", 4, false)
	var free struct {
		Items []struct {
			GPU, MaxGPU float64
			CPU         float64
			Machines    int
		}
	}
	rec := doJSON(t, p.s.Routes().ServeHTTP, "GET", "/api/jobs/free", nil, p.as("rf-bob"), "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &free) != nil || len(free.Items) != 1 {
		t.Fatalf("free: %d %s", rec.Code, rec.Body)
	}
	if f := free.Items[0]; f.GPU != 4 || f.MaxGPU != 4 || f.Machines != 1 || f.CPU != 192-4 {
		t.Fatalf("free %+v", f)
	}
	var list struct {
		Jobs []struct {
			ID, Owner, Node string
			GPU             int
			Mine            bool
			Start, End      time.Time
		}
	}
	rec = doJSON(t, p.s.Routes().ServeHTTP, "GET", "/api/reservations", nil, p.as("rf-bob"), "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Jobs) != 1 {
		t.Fatalf("timeline: %d %s", rec.Code, rec.Body)
	}
	if h := list.Jobs[0]; h.ID != id || h.Owner != "rf-alice" || h.Node != p.n1 || h.GPU != 4 || h.Mine || h.End.Sub(h.Start) != 2*time.Hour {
		t.Fatalf("hold %+v", h)
	}
}

// A reservation made between the round's plan and its start of a job
// that may not be stopped is not run over: the job, seeing it once
// started, waits again.
func TestStartGivesWayToReservationMadeMeanwhile(t *testing.T) {
	p, _ := queueRig(t, "rm", "rm-alice")
	id := submit(t, p, "rm-alice", 1, 8, map[string]any{"timeLimitSeconds": 7200})
	made := false
	p.s.SetQueueBeforeStartsForTest(func() {
		if made {
			return
		}
		made = true
		if code, v := reserveAs(t, p.s, p.as("rm-bob"), p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
			t.Errorf("reserve meanwhile: %d %+v", code, v)
		}
	})
	p.s.QueueRound(context.Background())
	j := jobNow(t, id)
	if j.Status.Phase != devpodv1alpha1.JobQueued || j.Status.Reason != "reserved" {
		t.Fatalf("job %s %s", j.Status.Phase, j.Status.Reason)
	}
}

// The free cards are also told by model: what is free and what there is.
func TestJobsFreeModels(t *testing.T) {
	p, _ := queueRig(t, "rm", "rm-alice")
	labelMachine(t, p.n1, map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-80GB-PCIe"}, "", 0)
	startedJob(t, p, "rm-alice", 4, false)
	var free struct {
		Items []struct {
			Models []struct {
				Model    string
				GPU      int64
				Total    int64
				Machines int
			}
		}
	}
	rec := doJSON(t, p.s.Routes().ServeHTTP, "GET", "/api/jobs/free", nil, p.as("rm-alice"), "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &free) != nil || len(free.Items) != 1 {
		t.Fatalf("free: %d %s", rec.Code, rec.Body)
	}
	if ms := free.Items[0].Models; len(ms) != 1 || ms[0].Model != "NVIDIA-A100-80GB-PCIe" || ms[0].GPU != 4 || ms[0].Total != 8 || ms[0].Machines != 1 {
		t.Fatalf("models %+v", ms)
	}
}
