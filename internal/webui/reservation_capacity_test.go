// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

// at: 2026-11-27 at h:m, +08:00.
func at(h, m int) time.Time { return time.Date(2026, 11, 27, h-8, m, 0, 0, time.UTC) }

// rsvOf: a reservation of user for [start, end); gpu < 0 is the whole
// machine, else gpu cards and cpu cores.
func rsvOf(name, user string, start, end time.Time, gpu int32, cpu string) *devpodv1alpha1.NodeReservation {
	nr := &devpodv1alpha1.NodeReservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(at(9, 0))},
		Spec:       devpodv1alpha1.NodeReservationSpec{Node: "n1", User: user, Start: metav1.NewTime(start), End: metav1.NewTime(end)},
	}
	if gpu >= 0 {
		g, c := gpu, resource.MustParse(cpu)
		nr.Spec.GPU, nr.Spec.CPU = &g, &c
	}
	return nr
}

func runningOn(name, owner string, ask amount, since time.Time) placed {
	return placed{DP: &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: devpodv1alpha1.DevPodSpec{Owner: owner, Running: true}}, Ask: ask, Since: since}
}

var cap8 = amount{GPU: 8, CPU: 192000}

func TestBookedPeak(t *testing.T) {
	r1 := rsvOf("r1", "alice", at(10, 0), at(11, 0), -1, "")
	r2 := rsvOf("r2", "bob", at(11, 0), at(12, 0), 4, "96")
	r3 := rsvOf("r3", "carol", at(10, 0), at(12, 0), 1, "1")
	r3.Spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: "carol", At: metav1.NewTime(at(10, 15))}
	rs := []*devpodv1alpha1.NodeReservation{r1, r2, r3}
	for _, c := range []struct {
		name string
		got  amount
		want amount
	}{
		{"booked 10:10", bookedAt(rs, cap8, at(10, 10)), amount{9, 193000}},
		{"booked 10:30", bookedAt(rs, cap8, at(10, 30)), amount{8, 192000}},
		{"peak 10:30-11:30", peakIn(rs, cap8, at(10, 30), at(11, 30)), amount{8, 192000}},
		{"peak 11-12", peakIn(rs, cap8, at(11, 0), at(12, 0)), amount{4, 96000}},
	} {
		if c.got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, c.got, c.want)
		}
	}
}

func TestCheckRoom(t *testing.T) {
	step, horizon := 30*time.Minute, 14*24*time.Hour
	whole := []*devpodv1alpha1.NodeReservation{rsvOf("r1", "alice", at(10, 0), at(11, 0), -1, "")}
	if r := checkRoom(whole, cap8, amount{4, 96000}, false, at(10, 0), at(11, 0), step, horizon); r == nil || r.Free != (amount{}) || !r.Earliest.Equal(at(11, 0)) {
		t.Errorf("against a whole one: %+v", r)
	}
	if r := checkRoom(whole, cap8, amount{4, 96000}, false, at(11, 0), at(12, 0), step, horizon); r != nil {
		t.Errorf("after a whole one: %+v", r)
	}
	halves := []*devpodv1alpha1.NodeReservation{
		rsvOf("r1", "alice", at(10, 0), at(11, 0), 4, "96"),
		rsvOf("r2", "bob", at(10, 0), at(11, 0), 4, "96"),
	}
	if r := checkRoom(halves, cap8, cap8, true, at(10, 30), at(11, 30), step, horizon); r == nil || !r.Earliest.Equal(at(11, 0)) {
		t.Errorf("whole over two halves: %+v", r)
	}
	if r := checkRoom(halves[:1], cap8, amount{4, 96000}, false, at(10, 0), at(11, 0), step, horizon); r != nil {
		t.Errorf("the other half: %+v", r)
	}
	if r := checkRoom(halves[:1], cap8, amount{5, 96000}, false, at(10, 0), at(11, 0), step, horizon); r == nil || r.Free != (amount{4, 96000}) {
		t.Errorf("more than the other half: %+v", r)
	}
	// a machine whose capacity is unknown: a whole one is still exclusive
	if r := checkRoom(whole, amount{}, amount{}, true, at(10, 0), at(11, 0), step, horizon); r == nil {
		t.Error("two whole reservations on a 0-capacity machine overlap")
	}
}

func TestAdmitted(t *testing.T) {
	a := rsvOf("a", "alice", at(10, 0), at(11, 0), -1, "")
	b := rsvOf("b", "bob", at(10, 30), at(11, 30), -1, "")
	b.CreationTimestamp = metav1.NewTime(at(9, 1))
	rs := []*devpodv1alpha1.NodeReservation{b, a}
	if !admitted(rs, cap8, a, at(9, 0)) || admitted(rs, cap8, b, at(9, 0)) {
		t.Errorf("the earlier keeps it: a %v b %v", admitted(rs, cap8, a, at(9, 0)), admitted(rs, cap8, b, at(9, 0)))
	}
	b.CreationTimestamp = a.CreationTimestamp
	if !admitted(rs, cap8, a, at(9, 0)) || admitted(rs, cap8, b, at(9, 0)) {
		t.Errorf("made together, the first name keeps it: a %v b %v", admitted(rs, cap8, a, at(9, 0)), admitted(rs, cap8, b, at(9, 0)))
	}
}

func TestOverbooked(t *testing.T) {
	wholes := []*devpodv1alpha1.NodeReservation{rsvOf("a", "alice", at(10, 0), at(11, 0), -1, ""), rsvOf("b", "bob", at(10, 30), at(11, 30), -1, "")}
	if o := overbooked(wholes, cap8, at(9, 0)); !o["a"] || !o["b"] {
		t.Errorf("two whole ones overlapping: %v", o)
	}
	halves := []*devpodv1alpha1.NodeReservation{rsvOf("a", "alice", at(10, 0), at(11, 0), 4, "96"), rsvOf("b", "bob", at(10, 0), at(11, 0), 4, "96")}
	if o := overbooked(halves, cap8, at(9, 0)); o["a"] || o["b"] {
		t.Errorf("two halves: %v", o)
	}
	// what is past does not count: a ended early (cancelled at 10:15), b from 10:30
	wholes[0].Spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: "alice", At: metav1.NewTime(at(10, 15))}
	wholes[1].Spec.Start = metav1.NewTime(at(10, 0))
	if o := overbooked(wholes, cap8, at(10, 20)); o["a"] || o["b"] {
		t.Errorf("overlapping only before now: %v", o)
	}
	if !admitted(wholes, cap8, wholes[1], at(10, 20)) {
		t.Error("b, overlapping a only before now, gives way")
	}
}

func TestLoadRefusal(t *testing.T) {
	now := at(10, 30)
	rs := []*devpodv1alpha1.NodeReservation{rsvOf("r1", "alice", at(10, 0), at(11, 0), 4, "96")}
	on := []placed{runningOn("a1", "alice", amount{4, 64000}, at(10, 0))}
	l := loadAt(cap8, rs, on, now, "")
	if e := l.refusal("n1", "alice", true, amount{1, 0}); e == nil || e.Code != "RESERVATION_TOO_SMALL" {
		t.Errorf("one more card than she holds: %+v", e)
	}
	if e := loadAt(cap8, rs, on, now, "a1").refusal("n1", "alice", true, amount{4, 64000}); e != nil {
		t.Errorf("the same DevPod again (redeploy): %+v", e)
	}
	if e := l.refusal("n1", "bob", false, amount{4, 0}); e != nil {
		t.Errorf("bob in the unreserved half: %+v", e)
	}
	if e := l.refusal("n1", "bob", false, amount{5, 0}); e == nil || e.Code != "NO_ROOM" {
		t.Errorf("bob past the unreserved half: %+v", e)
	}
}

func TestToClear(t *testing.T) {
	now := at(10, 30)
	held := func(gpu int32, cpu string) []*devpodv1alpha1.NodeReservation {
		return []*devpodv1alpha1.NodeReservation{rsvOf("r1", "alice", at(10, 0), at(11, 0), gpu, cpu)}
	}
	on := []placed{
		runningOn("a1", "alice", amount{4, 0}, at(8, 0)),
		runningOn("bob-1", "bob", amount{2, 0}, at(8, 30)),
		runningOn("carol-1", "carol", amount{4, 0}, at(9, 30)),
	}
	got := toClear(cap8, held(4, "96"), on, now)
	if len(got) != 1 || got[0].DP.Name != "carol-1" {
		t.Errorf("4 held: %v", dpNames(got))
	}
	on[0].Ask = amount{2, 0}
	if got := toClear(cap8, held(2, "48"), on, now); len(got) != 0 {
		t.Errorf("nothing short: %v", dpNames(got))
	}
	twins := []placed{runningOn("x-1", "xu", amount{4, 0}, at(9, 0)), runningOn("x-2", "xu", amount{4, 0}, at(9, 0))}
	if got := toClear(cap8, held(-1, ""), twins, now); len(got) != 2 || got[0].DP.Name != "x-2" {
		t.Errorf("whole held, two in the way: %v", dpNames(got))
	}
	if got := toClear(cap8, held(4, "96"), twins, now); len(got) != 1 || got[0].DP.Name != "x-2" {
		t.Errorf("started together, the later name goes: %v", dpNames(got))
	}
}

func dpNames(ps []placed) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.DP.Name)
	}
	return out
}

func TestAskOfPod(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			Limits:   corev1.ResourceList{gpures.NVIDIA: resource.MustParse("1")}}}},
		Containers: []corev1.Container{{Name: "dev", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
			Limits:   corev1.ResourceList{gpures.NVIDIA: resource.MustParse("2")}}}},
	}
	if got := askOfPod(spec); got != (amount{2, 1500}) {
		t.Errorf("askOfPod: %+v", got)
	}
	// a limit without a request: the API server makes the request the limit
	limitOnly := &corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), gpures.NVIDIA: resource.MustParse("1")}}}}}
	if got := askOfPod(limitOnly); got != (amount{1, 4000}) {
		t.Errorf("askOfPod, limits only: %+v", got)
	}
}

func TestAmountString(t *testing.T) {
	for a, want := range map[amount]string{{4, 96000}: "4 GPUs + 96 cores", {1, 0}: "1 GPU", {0, 1500}: "1.5 cores", {}: "0 cores"} {
		if got := a.String(); got != want {
			t.Errorf("%+v: %q, want %q", a, got, want)
		}
	}
}
