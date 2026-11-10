// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestNextWake(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	start, end := t0.Add(time.Hour), t0.Add(3*time.Hour)
	clear, ending := 30*time.Minute, []time.Duration{30 * time.Minute, 5 * time.Minute}
	for _, c := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"long before: the clearing notice", t0, start.Add(-clear)},
		{"in the notice window: the start", start.Add(-10 * time.Minute), start},
		{"running: the first ending notice", start.Add(time.Minute), end.Add(-30 * time.Minute)},
		{"between the notices: the last one", end.Add(-20 * time.Minute), end.Add(-5 * time.Minute)},
		{"after the last notice: the end", end.Add(-time.Minute), end},
		{"exactly at a point: the one after it", start, end.Add(-30 * time.Minute)},
		{"over: nothing", end, time.Time{}},
	} {
		if got := nextWake(start, end, c.now, clear, ending); !got.Equal(c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func rsv(name, node, user string, created, start, end time.Time) devpodv1alpha1.Reservation {
	return devpodv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec:       devpodv1alpha1.ReservationSpec{Node: node, User: user, Start: metav1.NewTime(start), End: metav1.NewTime(end)},
	}
}

func TestHeldByOthersAndLosers(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	first := rsv("a", "n1", "alice", t0, t0, t0.Add(2*time.Hour))
	loser := rsv("b", "n1", "bob", t0.Add(time.Second), t0.Add(time.Hour), t0.Add(3*time.Hour)) // overlaps, created later
	other := rsv("c", "n2", "bob", t0, t0, t0.Add(time.Hour))
	cancelled := rsv("d", "n3", "bob", t0, t0, t0.Add(time.Hour))
	cancelled.Spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: "bob", At: metav1.NewTime(t0)}
	later := rsv("e", "n4", "bob", t0, t0.Add(5*time.Hour), t0.Add(6*time.Hour))
	all := []devpodv1alpha1.Reservation{first, loser, other, cancelled, later}

	if !loses(&loser, all) || loses(&first, all) {
		t.Fatal("the reservation created first keeps the slot")
	}
	// for carol, 90 minutes in: n1 is alice's (bob's overlapping one lost); n2 is over
	held := heldByOthers(all, "carol", t0.Add(90*time.Minute))
	if len(held) != 1 || held["n1"] == nil || held["n1"].Name != "a" {
		t.Fatalf("held for carol: %v", held)
	}
	// the holder is never kept off their own node
	if held := heldByOthers(all, "alice", t0.Add(time.Minute)); held["n1"] != nil || held["n2"] == nil {
		t.Fatalf("held for alice: %v", held)
	}
}

// The exclusion is added after the hash is taken, so it can never read as
// drift: the fingerprint of the rendered Pod is the same with and without.
func TestExclusionDoesNotChangeTheHash(t *testing.T) {
	pod := podWith("supervisor:v1", "hpc:1", "devbox:4")
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "gpu", Operator: corev1.NodeSelectorOpExists}}}, {}},
	}}}
	before := podSpecHash(pod)
	stamped := pod.DeepCopy()
	excludeNodes(stamped, []string{"n2", "n1"})
	terms := stamped.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for i, term := range terms { // terms are ORed: every one carries the rule
		last := term.MatchExpressions[len(term.MatchExpressions)-1]
		if last.Key != hostnameLabel || last.Operator != corev1.NodeSelectorOpNotIn || len(last.Values) != 2 || last.Values[0] != "n1" {
			t.Fatalf("term %d: %+v", i, term)
		}
	}
	if len(terms[0].MatchExpressions) != 2 {
		t.Fatalf("the user's own rule is gone: %+v", terms[0])
	}
	if podSpecHash(pod) != before {
		t.Fatal("the rendered Pod was modified")
	}
	bare := podWith("supervisor:v1", "hpc:1", "devbox:4")
	excludeNodes(bare, nil)
	if bare.Spec.Affinity != nil {
		t.Fatal("no reservations must leave the Pod untouched")
	}
	excludeNodes(bare, []string{"n1"})
	if len(bare.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) != 1 {
		t.Fatalf("affinity %+v", bare.Spec.Affinity)
	}
}
