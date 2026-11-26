// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// patchAs PATCHes user's DevPod name with body; the answer.
func patchAs(t *testing.T, p *placeRig, user, name, body string) (int, webui.PatchResponse, string) {
	t.Helper()
	rec := doJSON(t, p.s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/"+name, map[string]string{"name": name}, p.as(user), body)
	var out webui.PatchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	var e struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return rec.Code, out, e.Code
}

func queuedNow(dp devpodv1alpha1.DevPod) bool {
	_, ok := dp.Annotations[devpodv1alpha1.QueuedAtAnnotation]
	return ok
}

// Made running, unpinned, with the queue on: it is made stopped, in line.
func TestCreateRunningQueues(t *testing.T) {
	p := jobRig(t, "cq", "")
	name := p.create("cq-alice", http.StatusCreated)
	if dp := p.get(name); dp.Spec.Running || !queuedNow(dp) {
		t.Fatalf("running %v, annotations %v", dp.Spec.Running, dp.Annotations)
	}
}

// Stopped while in line, it leaves the line.
func TestStopLeavesLine(t *testing.T) {
	p := jobRig(t, "sl", "")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	name := p.create("sl-alice", http.StatusCreated)
	if code, _, _ := patchAs(t, p, "sl-alice", name, `{"running":false}`); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	devPodWhen(t, p, name, "out of line", func(dp *devpodv1alpha1.DevPod) bool { return !queuedNow(*dp) && !dp.Spec.Running })
	eventsOf(t, f, name, "create", "dequeue")
}

// Started unpinned with the queue on, it waits in line, answered so.
func TestStartUnpinnedQueues(t *testing.T) {
	p := jobRig(t, "su", "")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	name := p.create("su-alice", http.StatusCreated)
	patchAs(t, p, "su-alice", name, `{"running":false}`)
	devPodWhen(t, p, name, "out of line", func(dp *devpodv1alpha1.DevPod) bool { return !queuedNow(*dp) })
	code, resp, _ := patchAs(t, p, "su-alice", name, `{"running":true}`)
	if code != http.StatusOK || !resp.Queued || resp.DevPod == nil || resp.DevPod.Spec.Running || !queuedNow(*resp.DevPod) {
		t.Fatalf("start: %d %+v", code, resp)
	}
	eventsOf(t, f, name, "create", "dequeue", "queue")
}

// Pinned inside its owner's reservation in force, it starts at once.
func TestStartPinnedInOwnReservationStartsNow(t *testing.T) {
	p := newPlaceRig(t, "sp")
	queueOn(p.s)
	if code, v := reserveWith(t, p.s, p.as("sp-alice"), p.n1, p.slot, p.slot.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	p.createGPU("sp-alice", "g4", 4, p.n1, http.StatusCreated)
	if dp := p.get("sp-alice-g4"); !dp.Spec.Running || queuedNow(dp) {
		t.Fatalf("running %v annotations %v", dp.Spec.Running, dp.Annotations)
	}
}

// Pinned anywhere else, it waits in line for its machine, the pin its own.
func TestStartPinnedOtherwiseQueues(t *testing.T) {
	p := newPlaceRig(t, "so2")
	queueOn(p.s)
	p.createGPU("so2-alice", "c", 0, p.n2, http.StatusCreated)
	dp := p.get("so2-alice-c")
	if dp.Spec.Running || !queuedNow(dp) || pinnedOf(&dp) != p.n2 || dp.Annotations[devpodv1alpha1.AutoPinAnnotation] != "" {
		t.Fatalf("running %v pin %q annotations %v", dp.Spec.Running, pinnedOf(&dp), dp.Annotations)
	}
}

// With the queue off, it is pinned at once to where it fits.
func TestStartQueueOffPlacesNow(t *testing.T) {
	p := newPlaceRig(t, "sn")
	name := p.create("sn-alice", http.StatusCreated)
	dp := p.get(name)
	if !dp.Spec.Running || pinnedOf(&dp) == "" || dp.Annotations[devpodv1alpha1.AutoPinAnnotation] == "" {
		t.Fatalf("running %v pin %q annotations %v", dp.Spec.Running, pinnedOf(&dp), dp.Annotations)
	}
}

// With the queue off and no room, it is refused, told when there may be.
func TestStartQueueOffNoRoom(t *testing.T) {
	p, _ := clearRig(t, "sr")
	if code, v := reserveAs(t, p.s, p.as("sr-bob"), p.n1, p.slot, p.slot.Add(time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	if code := p.createGPU("sr-alice", "g8", 8, "", http.StatusConflict); code != "NO_ROOM" {
		t.Fatalf("8 cards, all reserved: %q", code)
	}
}

// Asking what no machine could ever give is refused at once.
func TestStartNeverFitsRefused(t *testing.T) {
	p := jobRig(t, "sv", "")
	if code := p.createGPU("sv-alice", "g16", 16, "", http.StatusConflict); code != "NO_MACHINE" {
		t.Fatalf("16 cards: %q", code)
	}
}

// A VM DevPod starts as it always has: the queue has no say.
func TestVMDevPodStartsAsToday(t *testing.T) {
	p := jobRig(t, "sm", "")
	p.s.Admins = map[string]bool{"sm-root": true}
	ctx := context.Background()
	vm := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "sm-root-vm", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: "sm-root",
		VM: &devpodv1alpha1.VMWorkloadSpec{Raw: runtime.RawExtension{Raw: []byte(`{"spec":{}}`)}}}}
	if err := k8sClient.Create(ctx, vm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, vm) })
	waitDevPod(t, vm.Name)
	rec := doJSON(t, p.s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/"+vm.Name, map[string]string{"name": vm.Name}, forge(p.sm, "sm-root", true), `{"running":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	if dp := p.get(vm.Name); !dp.Spec.Running || queuedNow(dp) {
		t.Fatalf("running %v annotations %v", dp.Spec.Running, dp.Annotations)
	}
}

// An admin's DevPod waits in line too, at any priority.
func TestAdminDevPodQueues(t *testing.T) {
	p := jobRig(t, "sa", "")
	p.s.Admins = map[string]bool{"sa-root": true}
	root := forge(p.sm, "sa-root", true)
	rec := doJSON(t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, root, createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	name := "sa-root-dev1"
	waitDevPod(t, name)
	doJSON(t, p.s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/"+name, map[string]string{"name": name}, root, `{"running":false}`)
	devPodWhen(t, p, name, "out of line", func(dp *devpodv1alpha1.DevPod) bool { return !queuedNow(*dp) })
	rec = doJSON(t, p.s.HandlePatchDevPodForTest(), "PATCH", "/api/devpods/"+name, map[string]string{"name": name}, root, `{"running":true,"priority":1000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	if dp := p.get(name); !queuedNow(dp) || dp.Annotations[devpodv1alpha1.QueuePriorityAnnotation] != "1000" {
		t.Fatalf("annotations %v", dp.Annotations)
	}
}

// A priority over one's cap is refused.
func TestStartPriorityOverCapRefused(t *testing.T) {
	p := jobRig(t, "sc", "")
	name := p.create("sc-alice", http.StatusCreated)
	patchAs(t, p, "sc-alice", name, `{"running":false}`)
	devPodWhen(t, p, name, "out of line", func(dp *devpodv1alpha1.DevPod) bool { return !queuedNow(*dp) })
	if code, _, ec := patchAs(t, p, "sc-alice", name, `{"running":true,"priority":5}`); code != http.StatusForbidden || ec != "PRIORITY_TOO_HIGH" {
		t.Fatalf("priority 5 over cap 0: %d %q", code, ec)
	}
}

// A round starts a DevPod in its turn: pinned to the snuggest machine,
// the pin the queue's, out of line — the system's doing, recorded.
func TestRoundStartsQueuedDevPod(t *testing.T) {
	p := jobRig(t, "rs", "")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	name := p.create("rs-alice", http.StatusCreated)
	p.s.QueueRound(context.Background())
	dp := devPodWhen(t, p, name, "started", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running })
	if pin := pinnedOf(&dp); pin != p.n2 || queuedNow(dp) || len(dp.Annotations[devpodv1alpha1.AutoPinAnnotation]) <= len(pin) ||
		dp.Annotations[devpodv1alpha1.AutoPinAnnotation][:len(pin)+1] != pin+"@" {
		t.Fatalf("pin %q annotations %v", pin, dp.Annotations)
	}
	rows := eventsOf(t, f, name, "create", "queue-start")
	if r := rows[1]; r.Actor != "system:queue" || r.Node != p.n2 || r.Detail != "auto" {
		t.Fatalf("queue-start %+v", r)
	}
}

// DevPods and jobs keep one line: a job submitted first starts first;
// the DevPod after it waits, saying where it stands — written once.
func TestRoundOrdersDevPodsWithJobs(t *testing.T) {
	p := jobRig(t, "ro", "ro-alice")
	id := submit(t, p, "ro-alice", 1, 8, nil)
	p.createGPU("ro-alice", "g8", 8, "", http.StatusCreated)
	ctx := context.Background()
	p.s.QueueRound(ctx)
	if j := jobNow(t, id); j.Status.Phase != devpodv1alpha1.JobStarting {
		t.Fatalf("job %+v", j.Status)
	}
	dp := devPodWhen(t, p, "ro-alice-g8", "waiting, saying where", func(dp *devpodv1alpha1.DevPod) bool {
		return queuedNow(*dp) && !dp.Spec.Running && dp.Annotations[devpodv1alpha1.QueueStateAnnotation] != ""
	})
	var st struct {
		Position int32
		Reason   string
	}
	_ = json.Unmarshal([]byte(dp.Annotations[devpodv1alpha1.QueueStateAnnotation]), &st)
	if st.Position != 1 || st.Reason != "capacity" {
		t.Fatalf("state %+v", st)
	}
	time.Sleep(300 * time.Millisecond)
	before := p.get("ro-alice-g8").ResourceVersion
	p.s.QueueRound(ctx)
	time.Sleep(300 * time.Millisecond)
	if after := p.get("ro-alice-g8").ResourceVersion; after != before {
		t.Fatalf("a round with nothing changed wrote it: %s -> %s", before, after)
	}
}

// Out of line between the plan and its start, a DevPod is not started.
func TestRoundQueuedDevPodChangedMeanwhile(t *testing.T) {
	p := jobRig(t, "rc", "")
	name := p.create("rc-alice", http.StatusCreated)
	ctx := context.Background()
	p.s.SetQueueBeforeStartsForTest(func() {
		var dp devpodv1alpha1.DevPod
		if err := k8sClient.Get(ctx, client_key(name), &dp); err != nil {
			t.Error(err)
			return
		}
		delete(dp.Annotations, devpodv1alpha1.QueuedAtAnnotation)
		if err := k8sClient.Update(ctx, &dp); err != nil {
			t.Error(err)
		}
	})
	p.s.QueueRound(ctx)
	time.Sleep(500 * time.Millisecond)
	if dp := p.get(name); dp.Spec.Running || pinnedOf(&dp) != "" {
		t.Fatalf("started after leaving the line: running %v pin %q", dp.Spec.Running, pinnedOf(&dp))
	}
}

// startedByRound: user's DevPod name, made and started by a round
// (pinned by the queue); it as the cache has it then.
func startedByRound(t *testing.T, p *placeRig, user string) (string, devpodv1alpha1.DevPod) {
	t.Helper()
	name := p.create(user, http.StatusCreated)
	p.s.QueueRound(context.Background())
	return name, devPodWhen(t, p, name, "started by the round", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running && pinnedOf(dp) != "" })
}

// Stopped outside the console, a DevPod loses the queue's pin at the next
// round — the system's doing, recorded.
func TestAutoPinRemovedWhenStopped(t *testing.T) {
	p := jobRig(t, "ap", "")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	name, dp := startedByRound(t, p, "ap-alice")
	dp.Spec.Running = false
	if err := k8sClient.Update(context.Background(), &dp); err != nil {
		t.Fatal(err)
	}
	devPodWhen(t, p, name, "stopped", func(dp *devpodv1alpha1.DevPod) bool { return !dp.Spec.Running })
	p.s.QueueRound(context.Background())
	devPodWhen(t, p, name, "unpinned", func(dp *devpodv1alpha1.DevPod) bool {
		return pinnedOf(dp) == "" && dp.Annotations[devpodv1alpha1.AutoPinAnnotation] == ""
	})
	eventsOf(t, f, name, "create", "queue-start", "auto-unpin")
}

// A pin its user set stays, stopped or not.
func TestUserPinKept(t *testing.T) {
	p := jobRig(t, "up", "")
	p.createGPU("up-alice", "c", 0, p.n2, http.StatusCreated) // pinned by its user, in line
	p.s.QueueRound(context.Background())
	devPodWhen(t, p, "up-alice-c", "started", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running })
	patchAs(t, p, "up-alice", "up-alice-c", `{"running":false}`)
	devPodWhen(t, p, "up-alice-c", "stopped", func(dp *devpodv1alpha1.DevPod) bool { return !dp.Spec.Running })
	p.s.QueueRound(context.Background())
	time.Sleep(300 * time.Millisecond)
	if dp := p.get("up-alice-c"); pinnedOf(&dp) != p.n2 {
		t.Fatalf("the user's pin went: %q", pinnedOf(&dp))
	}
}

// A DevPod running unpinned from before is left as it is.
func TestRunningUnpinnedUntouched(t *testing.T) {
	p := jobRig(t, "ru", "")
	ctx := context.Background()
	old := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "ru-alice-old", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: "ru-alice", Running: true,
		Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}}}
	if err := k8sClient.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, old) })
	before := waitDevPod(t, old.Name).ResourceVersion
	p.s.QueueRound(ctx)
	time.Sleep(300 * time.Millisecond)
	if dp := p.get(old.Name); dp.ResourceVersion != before || !dp.Spec.Running || pinnedOf(&dp) != "" {
		t.Fatalf("touched: rv %s->%s running %v pin %q", before, dp.ResourceVersion, dp.Spec.Running, pinnedOf(&dp))
	}
}

// A Pod that does not schedule where the queue put it for five minutes:
// back in line at its old place, that machine kept from it a while; the
// next round puts it elsewhere.
func TestUnschedulableRequeuesAndAvoids(t *testing.T) {
	p := jobRig(t, "us", "")
	f := &fakeAuditStore{}
	p.s.Audit = &webui.AuditDeps{Store: f}
	name, dp := startedByRound(t, p, "us-alice")
	first := pinnedOf(&dp)
	auto := dp.Annotations[devpodv1alpha1.AutoPinAnnotation]
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "0/2 nodes are available: insufficient memory", LastTransitionTime: metav1.NewTime(time.Now().Add(-6 * time.Minute))}}
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var got corev1.Pod
		return k8sClient.Get(ctx, client_key(name), &got) == nil && len(got.Status.Conditions) == 1
	})
	p.s.QueueRound(ctx)
	back := devPodWhen(t, p, name, "back in line", func(dp *devpodv1alpha1.DevPod) bool { return !dp.Spec.Running && queuedNow(*dp) })
	if at := back.Annotations[devpodv1alpha1.QueuedAtAnnotation]; auto[len(first)+1:] != at {
		t.Fatalf("its place %q, was %q", at, auto[len(first)+1:])
	}
	if avoid := back.Annotations[devpodv1alpha1.QueueAvoidAnnotation]; len(avoid) <= len(first) || avoid[:len(first)+1] != first+" " {
		t.Fatalf("avoid %q", avoid)
	}
	if rows := eventsOf(t, f, name, "create", "queue-start", "requeue-unschedulable"); rows[2].Reason != "0/2 nodes are available: insufficient memory" {
		t.Fatalf("requeue %+v", rows[2])
	}
	p.s.QueueRound(ctx)
	if dp := devPodWhen(t, p, name, "started again", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running }); pinnedOf(&dp) == first {
		t.Fatalf("put on %q again", first)
	}
}

// Made bigger while running where the queue put it, it must fit there.
func TestEditAutoPinnedTooBigRefused(t *testing.T) {
	p := newPlaceRig(t, "eb") // the queue off: pinned at its start
	p.createGPU("eb-bob", "g6", 6, p.n1, http.StatusCreated)
	p.createGPU("eb-alice", "g1", 1, "", http.StatusCreated)
	dp := p.get("eb-alice-g1")
	if pinnedOf(&dp) != p.n1 || dp.Annotations[devpodv1alpha1.AutoPinAnnotation] == "" {
		t.Fatalf("pin %q annotations %v", pinnedOf(&dp), dp.Annotations)
	}
	if code, _, ec := patchAs(t, p, "eb-alice", "eb-alice-g1", `{"gpu":"4"}`); code != http.StatusConflict || ec != "NO_ROOM_HERE" {
		t.Fatalf("4 cards where 2 are free: %d %q", code, ec)
	}
	if code, _, ec := patchAs(t, p, "eb-alice", "eb-alice-g1", `{"gpu":"2"}`); code != http.StatusOK {
		t.Fatalf("2 cards: %d %q", code, ec)
	}
}

// runQueue runs p's queue loop (a tick of an hour: only nudges start
// rounds) until the test ends.
func runQueue(t *testing.T, p *placeRig) {
	t.Helper()
	p.s.Queue.Tick = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.s.RunQueue(ctx, nil); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// A start with room starts at once: the replica that took it asks for a
// round, not waiting for the tick.
func TestNudgeStartsSoon(t *testing.T) {
	p := jobRig(t, "ns", "")
	runQueue(t, p)
	name := p.create("ns-alice", http.StatusCreated)
	deadline := time.Now().Add(3 * time.Second)
	for !p.get(name).Spec.Running {
		if time.Now().After(deadline) {
			t.Fatal("not started within 3 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Put in line by another replica (as its handler would), it starts as
// soon: the leader sees it in its cache.
func TestQueuedByOtherReplicaStartsSoon(t *testing.T) {
	p := jobRig(t, "nr", "")
	ctx := context.Background()
	dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "nr-alice-x", Namespace: "devpods", Annotations: map[string]string{
		devpodv1alpha1.QueuedAtAnnotation: time.Now().UTC().Format(time.RFC3339Nano), devpodv1alpha1.QueuePriorityAnnotation: "0"}},
		Spec: devpodv1alpha1.DevPodSpec{Owner: "nr-alice", Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}}}}}}}
	runQueue(t, p)
	time.Sleep(200 * time.Millisecond)
	if err := k8sClient.Create(ctx, dp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, dp) })
	deadline := time.Now().Add(3 * time.Second)
	for !p.get(dp.Name).Spec.Running {
		if time.Now().After(deadline) {
			t.Fatal("not started within 3 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// What the console writes on a DevPod — its place and priority in line,
// its pins, its notes — is not anyone's to write in a DevPod they make:
// such annotations in a YAML create are dropped.
func TestCreateCannotForgeQueueMarks(t *testing.T) {
	p := jobRig(t, "fq", "")
	body, _ := json.Marshal(map[string]string{"yaml": `
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: fq-alice-forged
  annotations:
    devpod.io/queued-at: "2000-01-01T00:00:00Z"
    devpod.io/queue-priority: "1000000"
    devpod.io/auto-pin: "fq-n1@2000-01-01T00:00:00Z"
    devpod.io/hibernated-by: "machine-closed"
    devpod.io/access-allowed: "fq-n1"
    note: "mine to keep"
spec:
  running: false
  pod:
    spec:
      containers:
      - name: dev
        image: ubuntu:24.04
        resources:
          limits: {cpu: "2", memory: "4Gi"}
`})
	if rec := doJSON(t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, p.as("fq-alice"), string(body)); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	dp := waitDevPod(t, "fq-alice-forged")
	for _, k := range []string{devpodv1alpha1.QueuedAtAnnotation, devpodv1alpha1.QueuePriorityAnnotation, devpodv1alpha1.AutoPinAnnotation, devpodv1alpha1.HibernatedByAnnotation, "devpod.io/access-allowed"} {
		if v, ok := dp.Annotations[k]; ok {
			t.Errorf("%s kept: %q", k, v)
		}
	}
	if dp.Annotations["note"] != "mine to keep" {
		t.Errorf("the user's own annotation went: %v", dp.Annotations)
	}
}

// A DevPod stopped from before carries the allow-list of its last start;
// started by the queue, it goes where the queue put it: the old list goes.
func TestRoundDropsStaleAllowList(t *testing.T) {
	p := jobRig(t, "sa2", "")
	name := p.create("sa2-alice", http.StatusCreated) // in line
	ctx := context.Background()
	var dp devpodv1alpha1.DevPod
	if err := k8sClient.Get(ctx, client_key(name), &dp); err != nil {
		t.Fatal(err)
	}
	webui.SetAllowedForTest(&dp, []string{p.n1}) // only n1, from a start long ago
	if err := k8sClient.Update(ctx, &dp); err != nil {
		t.Fatal(err)
	}
	devPodWhen(t, p, name, "with the old list", func(dp *devpodv1alpha1.DevPod) bool { return len(allowedOf(*dp)) == 1 })
	p.s.QueueRound(ctx)
	got := devPodWhen(t, p, name, "started", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running })
	if allowedOf(got) != nil || got.Spec.Pod.Spec.Affinity != nil {
		t.Fatalf("pinned %q with the old list %v / %+v", pinnedOf(&got), allowedOf(got), got.Spec.Pod.Spec.Affinity)
	}
}

// Whether an edit fits where it runs is judged by the room now — a
// reservation later on that machine is the clearing's business — and an
// edit asking no more never needs room.
func TestEditAutoPinnedBesideLaterReservation(t *testing.T) {
	p, _ := clearRig(t, "el") // the queue off: pinned at its start
	p.createGPU("el-alice", "g1", 1, "", http.StatusCreated)
	if dp := p.get("el-alice-g1"); pinnedOf(&dp) != p.n1 {
		t.Fatalf("pinned %q", pinnedOf(&dp))
	}
	if code, v := reserveAs(t, p.s, p.as("el-bob"), p.n1, p.slot.Add(2*time.Hour), p.slot.Add(3*time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	if code, _, ec := patchAs(t, p, "el-alice", "el-alice-g1", `{"memory":"8Gi"}`); code != http.StatusOK {
		t.Fatalf("more memory: %d %q", code, ec)
	}
	if code, _, ec := patchAs(t, p, "el-alice", "el-alice-g1", `{"gpu":"2"}`); code != http.StatusOK {
		t.Fatalf("a card more, room now: %d %q", code, ec)
	}
}

// An admin's pinned start inside their own reservation starts at once too.
func TestStartAdminPinnedInOwnReservationStartsNow(t *testing.T) {
	p := newPlaceRig(t, "sx")
	queueOn(p.s)
	p.s.Admins = map[string]bool{"sx-root": true}
	root := forge(p.sm, "sx-root", true)
	if code, v := reserveWith(t, p.s, root, p.n1, p.slot, p.slot.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	rec := doJSON(t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, root, fmt.Sprintf(`{"name":"g4","image":"ubuntu:24.04","cpu":"2","memory":"4Gi","gpu":"4","node":%q}`, p.n1))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if dp := waitDevPod(t, "sx-root-g4"); !dp.Spec.Running || queuedNow(dp) {
		t.Fatalf("running %v annotations %v", dp.Spec.Running, dp.Annotations)
	}
}

// One who may have the machine only under a reservation, asking more
// than theirs holds, is refused at once — no wait would mend it.
func TestStartTooBigForOwnReservationRefused(t *testing.T) {
	p := newPlaceRig(t, "st3")
	queueOn(p.s)
	setRules(t, devpodv1alpha1.AccessRule{Machines: []string{p.n1}, Subject: &devpodv1alpha1.AccessSubject{Kind: "some", Users: []string{"st3-alice"}}, Effect: needsReservation})
	if code, v := reserveWith(t, p.s, p.as("st3-alice"), p.n1, p.slot, p.slot.Add(time.Hour), map[string]any{"gpu": 4}); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	if code := p.createGPU("st3-alice", "g8", 8, p.n1, http.StatusConflict); code != "RESERVATION_TOO_SMALL" {
		t.Fatalf("8 cards in a 4-card reservation: %q", code)
	}
}

// Pinned and waiting, a DevPod starts once its owner's reservation there
// is in force and it fits in it.
func TestRoundStartsPinnedWhenItsReservationBegins(t *testing.T) {
	p, clock := clearRig(t, "rb")
	queueOn(p.s)
	p.s.Queue.Now = func() time.Time { return *clock } // one clock for the queue and the reservations
	// the whole machine hers: the line's planning has no room there for anyone
	if code, v := reserveAs(t, p.s, p.as("rb-alice"), p.n1, p.slot.Add(time.Hour), p.slot.Add(2*time.Hour)); code != http.StatusCreated {
		t.Fatalf("reserve: %d %+v", code, v)
	}
	p.createGPU("rb-alice", "g4", 4, p.n1, http.StatusCreated)
	if dp := p.get("rb-alice-g4"); dp.Spec.Running || !queuedNow(dp) {
		t.Fatalf("before its reservation: running %v", dp.Spec.Running)
	}
	*clock = p.slot.Add(time.Hour + time.Minute)
	p.s.QueueRound(context.Background())
	devPodWhen(t, p, "rb-alice-g4", "started in its reservation", func(dp *devpodv1alpha1.DevPod) bool { return dp.Spec.Running && pinnedOf(dp) == p.n1 })
}

// An admin's hibernate-all takes the user's DevPods out of the line too.
func TestUsersBatchHibernateDequeues(t *testing.T) {
	p := jobRig(t, "hb", "")
	p.s.Admins = map[string]bool{"hb-root": true}
	name := p.create("hb-alice", http.StatusCreated) // in line
	rec := doJSON(t, p.s.HandleUsersBatchForTest(), "POST", "/api/admin/users:batch", nil, forge(p.sm, "hb-root", true), `{"op":"hibernateAll","names":["hb-alice"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body)
	}
	devPodWhen(t, p, name, "out of line", func(dp *devpodv1alpha1.DevPod) bool { return !queuedNow(*dp) && !dp.Spec.Running })
}

// With the queue off, a DevPod whose Pod will not schedule where it was
// pinned is pinned again elsewhere, that machine kept from it a while.
func TestUnschedulableQueueOffPlacedAgain(t *testing.T) {
	p := newPlaceRig(t, "uo")
	name := p.create("uo-alice", http.StatusCreated)
	dp := p.get(name)
	first := pinnedOf(&dp)
	if first == "" || !dp.Spec.Running {
		t.Fatalf("pinned %q running %v", first, dp.Spec.Running)
	}
	ctx := context.Background()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "insufficient memory", LastTransitionTime: metav1.NewTime(time.Now().Add(-6 * time.Minute))}}
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var got corev1.Pod
		return k8sClient.Get(ctx, client_key(name), &got) == nil && len(got.Status.Conditions) == 1
	})
	p.s.PinSweepRoundForTest(ctx)
	got := devPodWhen(t, p, name, "pinned again", func(dp *devpodv1alpha1.DevPod) bool { return pinnedOf(dp) != "" && pinnedOf(dp) != first })
	if !got.Spec.Running || got.Annotations[devpodv1alpha1.AutoPinAnnotation] == "" {
		t.Fatalf("running %v annotations %v", got.Spec.Running, got.Annotations)
	}
}
