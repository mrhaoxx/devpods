// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/controllers"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// The reservation timeline against a real API server. The DevPod
// reconciler runs in the manager with the fake clock; the Reservation
// reconciler is driven by hand (Reconcile at chosen moments), reading
// straight from the API server so every step sees the previous one.
func TestReservations(t *testing.T) {
	clk := &fakeClock{}
	t0 := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
	clk.Set(t0)
	setupSuiteWith(t, func(r *controllers.DevPodReconciler) {
		r.Reservations = true
		r.Now = clk.Now
	})
	env := newTestEnv(t)
	ctx := context.Background()
	c, err := client.New(envTestEnv.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const retention = 24 * time.Hour
	rr := &controllers.ReservationReconciler{Client: c, Namespace: "devpods", Now: clk.Now,
		ClearNotice: 30 * time.Minute, EndingNotices: []time.Duration{30 * time.Minute, 5 * time.Minute}, Retention: retention}
	_ = env

	at := func(d time.Duration) metav1.Time { return metav1.NewTime(t0.Add(d)) }
	reserve := func(name, node, user string, from, to time.Duration, extends string) {
		t.Helper()
		r := &devpodv1alpha1.Reservation{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: devpodv1alpha1.ReservationSpec{Node: node, User: user, Start: at(from), End: at(to), Extends: extends}}
		if err := c.Create(ctx, r); err != nil {
			t.Fatalf("create reservation %s: %v", name, err)
		}
	}
	step := func(name string, now time.Duration) ctrl.Result {
		t.Helper()
		clk.Set(t0.Add(now))
		res, err := rr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
		if err != nil {
			t.Fatalf("reconcile %s at +%v: %v", name, now, err)
		}
		return res
	}
	getRes := func(name string) *devpodv1alpha1.Reservation {
		t.Helper()
		var r devpodv1alpha1.Reservation
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &r); err != nil {
			t.Fatalf("get reservation %s: %v", name, err)
		}
		return &r
	}
	cancel := func(name, by string, when time.Duration) {
		t.Helper()
		r := getRes(name)
		r.Spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: by, At: at(when)}
		if err := c.Update(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	getDP := func(name string) *devpodv1alpha1.DevPod {
		t.Helper()
		var dp devpodv1alpha1.DevPod
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "devpods"}, &dp); err != nil {
			t.Fatalf("get devpod %s: %v", name, err)
		}
		return &dp
	}
	cond := func(name, typ string) string {
		if cnd := meta.FindStatusCondition(getDP(name).Status.Conditions, typ); cnd != nil && cnd.Status == metav1.ConditionTrue {
			return cnd.Message
		}
		return ""
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s", what)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	getPod := func(name string) *corev1.Pod {
		var pod corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "devpods"}, &pod); err != nil {
			return nil
		}
		return &pod
	}
	// devpod creates a running DevPod (pinned to pin when set) and, with
	// onNode, binds its Pod there as the scheduler would.
	devpod := func(name, owner, pin, onNode string) {
		t.Helper()
		spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}
		if pin != "" {
			spec.NodeSelector = map[string]string{"kubernetes.io/hostname": pin}
		}
		dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
			Spec: devpodv1alpha1.DevPodSpec{Owner: owner, Running: true, Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: spec}}}
		if err := c.Create(ctx, dp); err != nil {
			t.Fatalf("create devpod %s: %v", name, err)
		}
		if onNode == "" {
			return
		}
		eventually("pod of "+name, func() bool { return getPod(name) != nil })
		bind := &corev1.Binding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"}, Target: corev1.ObjectReference{Kind: "Node", Name: onNode}}
		if err := c.SubResource("binding").Create(ctx, getPod(name), bind); err != nil {
			t.Fatalf("bind %s to %s: %v", name, onNode, err)
		}
	}
	const hibernatedBy = devpodv1alpha1.HibernatedByAnnotation

	t.Run("timeline: notice, clearing at the start, ending notice, hibernate at the end, retention", func(t *testing.T) {
		devpod("bob-a", "bob", "", "n1")       // someone else's, on the node
		devpod("alice-a", "alice", "n1", "n1") // the holder's
		reserve("r1", "n1", "alice", time.Hour, 3*time.Hour, "")

		if res := step("r1", 0); res.RequeueAfter != 30*time.Minute {
			t.Fatalf("first wake-up in %v, want the clearing notice in 30m", res.RequeueAfter)
		}
		if r := getRes("r1"); r.Status.Phase != devpodv1alpha1.ReservationUpcoming || r.Status.NotifiedClearAt != nil || cond("bob-a", devpodv1alpha1.ConditionReservationClearing) != "" {
			t.Fatalf("before the notice: %+v", r.Status)
		}

		step("r1", 35*time.Minute)
		if msg := cond("bob-a", devpodv1alpha1.ConditionReservationClearing); msg != "n1 将于 11:00 被 alice 预约，届时自动休眠；休眠后容器内数据丢失" {
			t.Fatalf("clearing notice %q", msg)
		}
		if cond("alice-a", devpodv1alpha1.ConditionReservationClearing) != "" || getRes("r1").Status.NotifiedClearAt == nil || !getDP("bob-a").Spec.Running {
			t.Fatal("the notice only warns, and only other users")
		}
		step("r1", 40*time.Minute) // repeating a step changes nothing

		step("r1", time.Hour)
		if dp := getDP("bob-a"); dp.Spec.Running || dp.Annotations[hibernatedBy] != "reservation/r1" || cond("bob-a", devpodv1alpha1.ConditionReservationClearing) != "" {
			t.Fatalf("bob-a at the start: running=%v annotations=%v", dp.Spec.Running, dp.Annotations)
		}
		if r := getRes("r1"); r.Status.Phase != devpodv1alpha1.ReservationActive || !slices.Equal(r.Status.ClearedDevPods, []string{"bob-a"}) {
			t.Fatalf("active: %+v", r.Status)
		}
		if !getDP("alice-a").Spec.Running || cond("alice-a", devpodv1alpha1.ConditionReservationEnding) != "" {
			t.Fatal("the holder's DevPod keeps running, without a notice yet")
		}

		step("r1", 2*time.Hour+35*time.Minute)
		if msg := cond("alice-a", devpodv1alpha1.ConditionReservationEnding); msg != "预约将于 13:00 结束，届时自动休眠；休眠后容器内数据丢失" {
			t.Fatalf("ending notice %q", msg)
		}

		res := step("r1", 3*time.Hour)
		if dp := getDP("alice-a"); dp.Spec.Running || dp.Annotations[hibernatedBy] != "reservation/r1" || cond("alice-a", devpodv1alpha1.ConditionReservationEnding) != "" {
			t.Fatalf("alice-a at the end: running=%v annotations=%v", dp.Spec.Running, dp.Annotations)
		}
		if r := getRes("r1"); r.Status.Phase != devpodv1alpha1.ReservationEnded || !slices.Equal(r.Status.HibernatedAtEnd, []string{"alice-a"}) || len(r.Finalizers) != 0 {
			t.Fatalf("ended: %+v finalizers %v", r.Status, r.Finalizers)
		}
		if res.RequeueAfter != retention {
			t.Fatalf("kept for %v, want %v", res.RequeueAfter, retention)
		}
		step("r1", 3*time.Hour+retention)
		var gone devpodv1alpha1.Reservation
		if err := c.Get(ctx, types.NamespacedName{Name: "r1"}, &gone); !apierrors.IsNotFound(err) {
			t.Fatalf("after the retention: %v", err)
		}
	})

	t.Run("an admin's cancel is an early release", func(t *testing.T) {
		devpod("alice-b", "alice", "n2", "n2")
		reserve("r2", "n2", "alice", 0, 2*time.Hour, "")
		step("r2", 10*time.Minute)
		cancel("r2", "root", 20*time.Minute)
		step("r2", 20*time.Minute)
		if dp := getDP("alice-b"); dp.Spec.Running || dp.Annotations[hibernatedBy] != "reservation/r2" {
			t.Fatalf("alice-b after the cancel: running=%v", dp.Spec.Running)
		}
		if r := getRes("r2"); r.Status.Phase != devpodv1alpha1.ReservationCancelled || !slices.Equal(r.Status.HibernatedAtEnd, []string{"alice-b"}) {
			t.Fatalf("cancelled: %+v", r.Status)
		}
	})

	t.Run("an extension follows: no hibernation at the seam; cancelling takes the extensions along", func(t *testing.T) {
		devpod("alice-c", "alice", "n3", "n3")
		reserve("r3", "n3", "alice", 0, time.Hour, "")
		reserve("r3x", "n3", "alice", time.Hour, 2*time.Hour, "r3")
		step("r3", 40*time.Minute) // inside the ending-notice window, but their time goes on
		if cond("alice-c", devpodv1alpha1.ConditionReservationEnding) != "" {
			t.Fatal("no ending notice while an extension follows")
		}
		step("r3", time.Hour)
		if r := getRes("r3"); r.Status.Phase != devpodv1alpha1.ReservationEnded || len(r.Status.HibernatedAtEnd) != 0 || !getDP("alice-c").Spec.Running {
			t.Fatalf("seam: %+v running=%v", r.Status, getDP("alice-c").Spec.Running)
		}
		step("r3x", time.Hour)
		if getRes("r3x").Status.Phase != devpodv1alpha1.ReservationActive {
			t.Fatal("the extension takes over")
		}

		reserve("r4", "n4", "alice", 0, time.Hour, "")
		reserve("r4x", "n4", "alice", time.Hour, 2*time.Hour, "r4")
		reserve("r4xx", "n4", "alice", 2*time.Hour, 3*time.Hour, "r4x")
		cancel("r4", "alice", 10*time.Minute)
		step("r4", 10*time.Minute)
		if x := getRes("r4x"); x.Spec.Cancelled == nil || x.Spec.Cancelled.By != "alice" {
			t.Fatalf("r4x not cancelled with r4: %+v", x.Spec)
		}
		step("r4x", 10*time.Minute)
		if getRes("r4xx").Spec.Cancelled == nil || getRes("r4x").Status.Phase != devpodv1alpha1.ReservationCancelled {
			t.Fatal("the chain is cancelled link by link")
		}
	})

	t.Run("a controller that was down catches up", func(t *testing.T) {
		devpod("bob-e", "bob", "", "n5")
		reserve("r5", "n5", "alice", time.Hour, 2*time.Hour, "")
		step("r5", 90*time.Minute) // first reconcile ever: already half way through
		if dp := getDP("bob-e"); dp.Spec.Running || dp.Annotations[hibernatedBy] != "reservation/r5" {
			t.Fatalf("bob-e: running=%v", dp.Spec.Running)
		}
		if getRes("r5").Status.Phase != devpodv1alpha1.ReservationActive {
			t.Fatal("phase")
		}
	})

	t.Run("the loser of a conflict is left alone", func(t *testing.T) {
		devpod("bob-h", "bob", "", "n9")
		reserve("r9", "n9", "bob", 0, 2*time.Hour, "")
		time.Sleep(1100 * time.Millisecond) // creation timestamps have second precision
		reserve("r9late", "n9", "alice", time.Hour, 3*time.Hour, "")
		step("r9late", 90*time.Minute)
		if r := getRes("r9late"); r.Status.Phase != "" || len(r.Finalizers) != 0 || !getDP("bob-h").Spec.Running {
			t.Fatalf("the loser acted: %+v", r.Status)
		}
	})

	t.Run("pinned to a node someone holds: no Pod until it is free", func(t *testing.T) {
		clk.Set(t0.Add(10 * time.Minute))
		reserve("r6", "n6", "alice", 0, time.Hour, "")
		devpod("bob-f", "bob", "n6", "")
		eventually("NodeReserved on bob-f", func() bool { return cond("bob-f", devpodv1alpha1.ConditionNodeReserved) != "" })
		dp := getDP("bob-f")
		if msg := cond("bob-f", devpodv1alpha1.ConditionNodeReserved); msg != "n6 被 alice 预约至 11:00" {
			t.Fatalf("message %q", msg)
		}
		eventually("Pending with the reason", func() bool {
			dp = getDP("bob-f")
			return dp.Status.Phase == devpodv1alpha1.DevPodPending && dp.Status.Message == "n6 被 alice 预约至 11:00"
		})
		if getPod("bob-f") != nil {
			t.Fatal("a Pod was created on a held node")
		}
		// the holder's own DevPod pinned there runs
		devpod("alice-f", "alice", "n6", "")
		eventually("the holder's Pod", func() bool { return getPod("alice-f") != nil })

		cancel("r6", "alice", 10*time.Minute) // the node is free: the watch wakes bob-f
		eventually("bob-f's Pod once the node is free", func() bool { return getPod("bob-f") != nil })
		eventually("NodeReserved cleared", func() bool { return cond("bob-f", devpodv1alpha1.ConditionNodeReserved) == "" })
	})

	t.Run("an unpinned Pod keeps off held nodes, and is never recreated for a reservation", func(t *testing.T) {
		clk.Set(t0.Add(10 * time.Minute))
		reserve("r7", "n7", "alice", 0, time.Hour, "")
		devpod("bob-g", "bob", "", "")
		eventually("bob-g's Pod", func() bool { return getPod("bob-g") != nil })
		pod := getPod("bob-g")
		var notIn []string
		if a := pod.Spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			for _, e := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions {
				if e.Key == "kubernetes.io/hostname" && e.Operator == corev1.NodeSelectorOpNotIn {
					notIn = e.Values
				}
			}
		}
		if !slices.Contains(notIn, "n7") {
			t.Fatalf("no exclusion of n7: %+v", pod.Spec.Affinity)
		}
		// alice's own Pod is not kept off her node
		devpod("alice-g", "alice", "", "")
		eventually("alice-g's Pod", func() bool { return getPod("alice-g") != nil })
		if a := getPod("alice-g").Spec.Affinity; a != nil {
			for _, e := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions {
				if slices.Contains(e.Values, "n7") {
					t.Fatalf("the holder is kept off her own node: %+v", e)
				}
			}
		}

		// reservations change; the running Pod must stay the same Pod
		cancel("r7", "alice", 10*time.Minute)
		reserve("r8", "n8", "alice", 0, time.Hour, "")
		dp := getDP("bob-g")
		dp.Annotations = map[string]string{"test/poke": "1"} // force a reconcile
		if err := c.Update(ctx, dp); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		after := getPod("bob-g")
		if after == nil || after.UID != pod.UID || after.DeletionTimestamp != nil {
			t.Fatalf("the Pod was recreated: %v", after)
		}
	})
}
