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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// nodeNow reads a machine (through the cache).
func nodeNow(t *testing.T, name string) corev1.Node {
	t.Helper()
	var n corev1.Node
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// stampedNow: the console has seen name reservable, as the cache says.
func stampedNow(t *testing.T, name string) bool {
	t.Helper()
	return nodeNow(t, name).Annotations[devpodv1alpha1.ReservableSeenAnnotation] != ""
}

// unlabel takes the reservable label off name by hand (as kubectl would).
func unlabel(t *testing.T, name string) {
	t.Helper()
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{devpodv1alpha1.ReservableNodeLabel: nil}}})
	if err := k8sClient.Patch(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return nodeNow(t, name).Labels[devpodv1alpha1.ReservableNodeLabel] == "" })
}

// stamp: a round sees name reservable, and says so on it.
func stamp(t *testing.T, s *webui.Server, name string) {
	t.Helper()
	waitFor(t, func() bool {
		s.EnforceReservations(context.Background())
		return stampedNow(t, name)
	})
}

func reservationNow(t *testing.T, id string) (devpodv1alpha1.NodeReservation, bool) {
	t.Helper()
	var nr devpodv1alpha1.NodeReservation
	err := k8sClient.Get(context.Background(), client_key_cluster(id), &nr)
	return nr, err == nil
}

// A reservable machine is stamped as seen so: its label gone later, the
// console knows it was closed.
func TestReservableNodeIsStamped(t *testing.T) {
	p, _ := clearRig(t, "ms")
	stamp(t, p.s, p.n1)
	if stampedNow(t, p.n2) {
		t.Fatal("a machine never reservable was stamped")
	}
}

// Its label taken off by hand, a machine is closed: its reservations
// end, and the record says nobody in the console did it.
func TestClosedByHandCancelsReservations(t *testing.T) {
	p, _ := clearRig(t, "mh")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	stamp(t, p.s, p.n1)
	_, now := reserveAs(t, p.s, p.as("mh-alice"), p.n1, p.slot, p.slot.Add(time.Hour)) // in force
	_, later := reserveAs(t, p.s, p.as("mh-bob"), p.n1, p.slot.Add(2*time.Hour), p.slot.Add(3*time.Hour))
	unlabel(t, p.n1)
	waitFor(t, func() bool {
		p.s.EnforceReservations(context.Background())
		nr, ok := reservationNow(t, now.ID)
		_, still := reservationNow(t, later.ID)
		return ok && nr.Spec.Cancelled != nil && nr.Spec.Cancelled.By == "machine-closed" && !still && !stampedNow(t, p.n1)
	})
	rows := eventsOf(t, f, p.n1, "closed")
	if rows[0].Actor != "system:label-removed" || rows[0].Kind != "machine" {
		t.Fatalf("closed %+v", rows[0])
	}
}

// Closed through the console's switch, the record of the closing says who.
func TestClosedBySwitchRecordsAdmin(t *testing.T) {
	p, _ := clearRig(t, "mw")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	p.s.Admins = map[string]bool{"mw-root": true}
	stamp(t, p.s, p.n1)
	if code := doJSON(t, p.s.Routes().ServeHTTP, "PUT", "/api/admin/nodes/"+p.n1+"/reservable", nil, forge(p.sm, "mw-root", true), `{"reservable":false}`).Code; code != http.StatusOK {
		t.Fatalf("switch: %d", code)
	}
	waitFor(t, func() bool {
		p.s.EnforceReservations(context.Background())
		return !stampedNow(t, p.n1)
	})
	if rows := eventsOf(t, f, p.n1, "reservable-off", "closed"); rows[1].Actor != "mw-root" {
		t.Fatalf("closed %+v", rows[1])
	}
}

// Its jobs stop: back in line, or ended, as each was submitted.
func TestClosedStopsJobs(t *testing.T) {
	p, _ := queueRig(t, "mj", "mj-alice")
	stamp(t, p.s, p.n1)
	again := submit(t, p, "mj-alice", 1, 4, nil)
	ends := submit(t, p, "mj-alice", 1, 4, map[string]any{"requeue": false})
	for _, id := range []string{again, ends} {
		roundUntil(t, p, id, devpodv1alpha1.JobStarting)
		setRank(t, ranksBecome(t, id, 1)[0].Name, devpodv1alpha1.DevPodRunning, nil)
		roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	}
	unlabel(t, p.n1)
	waitFor(t, func() bool {
		p.s.EnforceReservations(context.Background())
		a, b := jobNow(t, again), jobNow(t, ends)
		return a.Status.Phase == devpodv1alpha1.JobQueued && a.Status.Reason == "machine-closed" &&
			b.Status.Phase == devpodv1alpha1.JobCancelled && b.Status.Reason == "machine-closed"
	})
}

// Its DevPods go only where they ran by a reservation: one whose owner may
// use the machine without runs on.
func TestClosedHibernatesOnlyReservationDevPods(t *testing.T) {
	p, _ := clearRig(t, "md")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	stamp(t, p.s, p.n1)
	p.createGPU("md-a", "g1", 1, p.n1, http.StatusCreated)
	p.createGPU("md-b", "g1", 1, p.n1, http.StatusCreated)
	a, b := "md-a-g1", "md-b-g1"
	// a may have the machine only under a reservation; b as it is
	setRules(t, devpodv1alpha1.AccessRule{Machines: []string{p.n1}, Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Users: []string{"md-a"}}, Effect: needsReservation}, everyoneAllowed)
	unlabel(t, p.n1)
	waitFor(t, func() bool {
		p.s.EnforceReservations(context.Background())
		da := p.get(a)
		return !da.Spec.Running && da.Annotations[devpodv1alpha1.HibernatedByAnnotation] == "machine-closed"
	})
	time.Sleep(500 * time.Millisecond)
	if db := p.get(b); !db.Spec.Running {
		t.Fatalf("b was stopped: %v", db.Annotations)
	}
	if rows := eventsOf(t, f, a, "create", "hibernate"); rows[1].Reason != "machine-closed" || rows[1].Actor != "system:label-removed" {
		t.Fatalf("hibernate %+v", rows[1])
	}
}

// A machine never seen reservable (its label gone before the console
// looked, or never there) is not closed: what is on it stays.
func TestUnstampedUnlabelledNodeIsNotClosed(t *testing.T) {
	p, _ := clearRig(t, "mu")
	_, r := reserveAs(t, p.s, p.as("mu-alice"), p.n1, p.slot.Add(2*time.Hour), p.slot.Add(3*time.Hour))
	unlabel(t, p.n1) // before any round saw it
	for range 3 {
		p.s.EnforceReservations(context.Background())
	}
	if nr, ok := reservationNow(t, r.ID); !ok || nr.Spec.Cancelled != nil {
		t.Fatalf("the reservation was touched: %v %+v", ok, nr.Spec.Cancelled)
	}
}
