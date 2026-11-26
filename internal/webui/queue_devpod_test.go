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
