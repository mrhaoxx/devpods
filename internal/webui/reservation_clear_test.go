// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// clearRig: a placeRig whose clock the test moves.
func clearRig(t *testing.T, prefix string) (*placeRig, *time.Time) {
	p := newPlaceRig(t, prefix)
	clock := time.Date(2026, 11, 12, 2, 10, 0, 0, time.UTC) // 10:10 +08:00
	p.s.Reservations.Now = func() time.Time { return clock }
	return p, &clock
}

// devPodWhen reads a DevPod (through the cache) until ok says so.
func devPodWhen(t *testing.T, p *placeRig, name, what string, ok func(*devpodv1alpha1.DevPod) bool) devpodv1alpha1.DevPod {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		dp := p.get(name)
		if ok(&dp) {
			return dp
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s: running %v, annotations %v", name, what, dp.Spec.Running, dp.Annotations)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func marked(at time.Time, id string) func(*devpodv1alpha1.DevPod) bool {
	return func(dp *devpodv1alpha1.DevPod) bool {
		return dp.Spec.Running && dp.Annotations[devpodv1alpha1.ClearAtAnnotation] == at.UTC().Format(time.RFC3339) && dp.Annotations[devpodv1alpha1.ClearForAnnotation] == id
	}
}

func unmarked(dp *devpodv1alpha1.DevPod) bool {
	_, a := dp.Annotations[devpodv1alpha1.ClearAtAnnotation]
	_, b := dp.Annotations[devpodv1alpha1.ClearForAnnotation]
	return dp.Spec.Running && !a && !b
}

// Whoever is in a starting reservation's way is told, and hibernated no
// sooner than the notice after.
func TestClearingNoticeThenHibernate(t *testing.T) {
	p, clock := clearRig(t, "cn")
	ctx := context.Background()
	p.createGPU("cn-bob", "g2", 2, p.n1, http.StatusCreated)
	start := clock.Add(20 * time.Minute) // 10:30
	code, r := reserveAs(t, p.s, p.as("cn-alice"), p.n1, start, start.Add(time.Hour))
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, r)
	}
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cn-bob-g2", "marked for 10:40", marked(clock.Add(30*time.Minute), r.ID)) // the notice runs past the start
	*clock = clock.Add(25 * time.Minute)
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cn-bob-g2", "still running at 10:35", marked(clock.Add(5*time.Minute), r.ID))
	*clock = clock.Add(5 * time.Minute)
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cn-bob-g2", "hibernated at 10:40", func(dp *devpodv1alpha1.DevPod) bool {
		_, a := dp.Annotations[devpodv1alpha1.ClearAtAnnotation]
		return !dp.Spec.Running && dp.Annotations[devpodv1alpha1.HibernatedByAnnotation] == "reservation/"+r.ID && !a
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		var nr devpodv1alpha1.NodeReservation
		if err := k8sClient.Get(ctx, client_key_cluster(r.ID), &nr); err == nil && slices.Contains(nr.Status.Cleared, "cn-bob-g2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reservation does not say whom it cleared")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A reservation of the current slot still gives the notice.
func TestClearingCurrentSlot(t *testing.T) {
	p, clock := clearRig(t, "cc")
	p.createGPU("cc-bob", "g2", 2, p.n1, http.StatusCreated)
	code, r := reserveAs(t, p.s, p.as("cc-alice"), p.n1, p.slot, p.slot.Add(time.Hour)) // 10:00, in force
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, r)
	}
	p.s.EnforceReservations(context.Background())
	devPodWhen(t, p, "cc-bob-g2", "marked for 10:40, not hibernated", marked(clock.Add(30*time.Minute), r.ID))
}

// A reservation given up takes its marks with it.
func TestClearingUnmarkOnCancel(t *testing.T) {
	p, clock := clearRig(t, "cu")
	ctx := context.Background()
	p.createGPU("cu-bob", "g2", 2, p.n1, http.StatusCreated)
	alice := p.as("cu-alice")
	code, r := reserveAs(t, p.s, alice, p.n1, clock.Add(20*time.Minute), clock.Add(80*time.Minute))
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, r)
	}
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cu-bob-g2", "marked", marked(clock.Add(30*time.Minute), r.ID))
	if rec := doJSON(t, p.s.HandleDeleteReservationForTest(), "DELETE", "/api/reservations/"+r.ID, map[string]string{"id": r.ID}, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cu-bob-g2", "unmarked", unmarked)
}

// Only as many go as the reservation needs, the newest first.
func TestClearingOnlyWhatIsShort(t *testing.T) {
	p, clock := clearRig(t, "cs")
	ctx := context.Background()
	p.createGPU("cs-bob", "g1", 4, p.n1, http.StatusCreated)
	p.createGPU("cs-bob", "g2", 4, p.n1, http.StatusCreated)
	alice := p.as("cs-alice")
	code, r := reserveWith(t, p.s, alice, p.n1, clock.Add(20*time.Minute), clock.Add(80*time.Minute), map[string]any{"gpu": 4})
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, r)
	}
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cs-bob-g2", "the newer marked", marked(clock.Add(30*time.Minute), r.ID))
	devPodWhen(t, p, "cs-bob-g1", "the older left", unmarked)
	if rec := doJSON(t, p.s.HandlePatchReservationForTest(), "PATCH", "/api/reservations/"+r.ID, map[string]string{"id": r.ID}, alice, `{"gpu":0,"cpu":8}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cs-bob-g2", "unmarked when no card is needed", unmarked)
}

// Another reservation starting meanwhile neither puts the clearing off
// nor takes it over: the notice given stands.
func TestClearingKeepsNoticeAcrossStarts(t *testing.T) {
	p, clock := clearRig(t, "ck")
	ctx := context.Background()
	p.createGPU("ck-bob", "g4", 4, p.n1, http.StatusCreated)
	code, alice := reserveWith(t, p.s, p.as("ck-alice"), p.n1, clock.Add(20*time.Minute), clock.Add(80*time.Minute), map[string]any{"gpu": 6})
	if code != http.StatusCreated {
		t.Fatalf("alice: %d %+v", code, alice)
	}
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "ck-bob-g4", "marked for 10:40", marked(clock.Add(30*time.Minute), alice.ID))
	// 10:31: carol reserves a few cores from 10:30 — in force now, started
	// together with alice's (and named after it)
	*clock = clock.Add(21 * time.Minute)
	if code, v := reserveWith(t, p.s, p.as("ck-carol"), p.n1, p.slot.Add(30*time.Minute), p.slot.Add(90*time.Minute), map[string]any{"gpu": 0, "cpu": 8}); code != http.StatusCreated {
		t.Fatalf("carol: %d %+v", code, v)
	}
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "ck-bob-g4", "still marked for 10:40", marked(p.slot.Add(40*time.Minute), alice.ID))
	*clock = p.slot.Add(40 * time.Minute)
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "ck-bob-g4", "hibernated at 10:40, for alice's", func(dp *devpodv1alpha1.DevPod) bool {
		return !dp.Spec.Running && dp.Annotations[devpodv1alpha1.HibernatedByAnnotation] == "reservation/"+alice.ID
	})
}

// A DevPod its owner stops after being told loses the mark.
func TestClearingUnmarkStopped(t *testing.T) {
	p, clock := clearRig(t, "cx")
	ctx := context.Background()
	p.createGPU("cx-bob", "g2", 2, p.n1, http.StatusCreated)
	code, r := reserveAs(t, p.s, p.as("cx-alice"), p.n1, clock.Add(20*time.Minute), clock.Add(80*time.Minute))
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, r)
	}
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cx-bob-g2", "marked", marked(clock.Add(30*time.Minute), r.ID))
	p.patch("cx-bob", "cx-bob-g2", `{"running":false}`, http.StatusOK)
	devPodWhen(t, p, "cx-bob-g2", "stopped", func(dp *devpodv1alpha1.DevPod) bool { return !dp.Spec.Running })
	p.s.EnforceReservations(ctx)
	devPodWhen(t, p, "cx-bob-g2", "unmarked", func(dp *devpodv1alpha1.DevPod) bool {
		_, a := dp.Annotations[devpodv1alpha1.ClearAtAnnotation]
		_, b := dp.Annotations[devpodv1alpha1.ClearForAnnotation]
		return !dp.Spec.Running && !a && !b
	})
}

// Reservations made before shares (permits, then) clear nobody.
func TestClearingSkipsPreV2(t *testing.T) {
	p, _ := clearRig(t, "cv")
	ctx := context.Background()
	p.createGPU("cv-bob", "g2", 2, p.n1, http.StatusCreated)
	old := &devpodv1alpha1.NodeReservation{ObjectMeta: metav1.ObjectMeta{Name: "cv-old"}, Spec: devpodv1alpha1.NodeReservationSpec{
		Node: p.n1, User: "cv-alice", Start: metav1.NewTime(p.slot), End: metav1.NewTime(p.slot.Add(time.Hour))}}
	if err := k8sClient.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	p.s.EnforceReservations(ctx)
	time.Sleep(500 * time.Millisecond)
	devPodWhen(t, p, "cv-bob-g2", "left alone", unmarked)
}
