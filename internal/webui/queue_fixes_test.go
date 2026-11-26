// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
	"github.com/mrhaoxx/devpod/internal/webui"
)

// A job made from a config is held to what a DevPod made from it would
// be: it may not reach Secrets (another user's keys, say).
func TestJobFromConfigGuarded(t *testing.T) {
	p := jobRig(t, "jg", "jg-alice")
	ctx := context.Background()
	content := "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: jg-alice-cj\nspec:\n  pod:\n    spec:\n      containers:\n" +
		"      - name: dev\n        image: ubuntu:24.04\n        volumeMounts: [{name: k, mountPath: /mnt}]\n      volumes:\n      - name: k\n        secret: {secretName: jg-bob-hostkey}\n"
	cfg := &devpodv1alpha1.DevPodConfig{ObjectMeta: metav1.ObjectMeta{Name: "jg-alice-steal", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodConfigSpec{Owner: "jg-alice", Title: "steal", Content: content}}
	if err := k8sClient.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cfg) })
	code, v := submitJob(t, p.s, p.as("jg-alice"), map[string]any{"from": map[string]any{"config": "jg-alice-steal"}, "nodes": 1, "gpus": 1, "command": "cat /mnt/x"})
	if code != http.StatusBadRequest || v.Code != "SECRET_REFERENCE" {
		t.Fatalf("a config mounting a Secret: %d %+v", code, v)
	}
}

// What makes a DevPod a job's rank — spec.job, the job labels — is the
// queue's: a user may not write it, made new or by the YAML editor.
func TestJobFieldsRefusedFromUsers(t *testing.T) {
	p := jobRig(t, "jf", "jf-alice")
	create := func(meta, job string) (int, string) {
		text := "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: jf-alice-fake\n" + meta + "spec:\n  owner: jf-alice\n  running: false\n" + job +
			"  pod:\n    spec:\n      containers: [{name: dev, image: x}]\n"
		body, _ := json.Marshal(map[string]string{"yaml": text})
		rec := doJSON(t, p.s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, p.as("jf-alice"), string(body))
		var e struct{ Code string }
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return rec.Code, e.Code
	}
	if code, c := create("", "  job: {name: abcde, rank: 0}\n"); code != http.StatusForbidden || c != "JOB_FIELDS_FORBIDDEN" {
		t.Errorf("spec.job: %d %s", code, c)
	}
	if code, c := create("  labels: {devpod.io/job: abcde}\n", ""); code != http.StatusForbidden || c != "JOB_FIELDS_FORBIDDEN" {
		t.Errorf("the job label: %d %s", code, c)
	}
	spec := p.get("jf-alice-dev1").Spec
	spec.Job = &devpodv1alpha1.DevPodJob{Name: "abcde"}
	if c := p.putYAML("jf-alice", "jf-alice-dev1", spec, http.StatusForbidden); c != "JOB_FIELDS_FORBIDDEN" {
		t.Errorf("spec.job by the YAML editor: %s", c)
	}
}

// A DevPod naming someone's job that the queue did not make is none of
// its ranks: it neither ends nor holds up the job.
func TestForgedRankIgnored(t *testing.T) {
	p, _ := queueRig(t, "qz", "qz-bob")
	ctx := context.Background()
	id := submit(t, p, "qz-bob", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	setRank(t, ranksBecome(t, id, 1)[0].Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	// alice's, made behind the API (as by kubectl), stopped: were it taken
	// for rank 0, bob's job would end "a rank was stopped"
	fake := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "qz-alice-x", Namespace: "devpods", Labels: map[string]string{"devpod.io/job": id}},
		Spec: devpodv1alpha1.DevPodSpec{Owner: "qz-alice", Job: &devpodv1alpha1.DevPodJob{Name: id, Rank: 0},
			Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}}}
	if err := k8sClient.Create(ctx, fake); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, fake) })
	waitDevPod(t, fake.Name)
	for range 6 {
		p.s.QueueRound(ctx)
	}
	if j := jobNow(t, id); j.Status.Phase != devpodv1alpha1.JobRunning {
		t.Fatalf("bob's job: %s %s", j.Status.Phase, j.Status.Reason)
	}
}

// A job queued again starts anew only once its former ranks are gone:
// one still going (being deleted) is never taken for the new start's.
func TestRequeuedJobWaitsForFormerRanks(t *testing.T) {
	p, clock := queueRig(t, "qw", "qw-alice")
	ctx := context.Background()
	id := submit(t, p, "qw-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	rank := ranksBecome(t, id, 1)[0].Name
	// deleted, it lingers (as a rank whose Pod is in its grace)
	hold := func(add bool) {
		t.Helper()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var dp devpodv1alpha1.DevPod
			if err := k8sManager.GetAPIReader().Get(ctx, client_key(rank), &dp); err != nil {
				return err
			}
			if add {
				controllerutil.AddFinalizer(&dp, "test.devpod.io/hold")
			} else {
				controllerutil.RemoveFinalizer(&dp, "test.devpod.io/hold")
			}
			return k8sClient.Update(ctx, &dp)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	hold(true)
	t.Cleanup(func() {
		var dp devpodv1alpha1.DevPod
		if k8sManager.GetAPIReader().Get(ctx, client_key(rank), &dp) == nil && controllerutil.RemoveFinalizer(&dp, "test.devpod.io/hold") {
			_ = k8sClient.Update(ctx, &dp)
		}
	})
	*clock = clock.Add(6 * time.Minute) // its ranks never ran: queued again
	waitFor(t, func() bool {
		p.s.QueueRound(ctx)
		j := jobNow(t, id)
		return j.Status.Phase == devpodv1alpha1.JobQueued && j.Status.Reason == "gang-timeout"
	})
	for range 4 {
		p.s.QueueRound(ctx)
	}
	if j := jobNow(t, id); j.Status.Phase != devpodv1alpha1.JobQueued || j.Status.Attempts != 1 {
		t.Fatalf("started over its former rank: %s attempts %d", j.Status.Phase, j.Status.Attempts)
	}
	hold(false)
	if j := roundUntil(t, p, id, devpodv1alpha1.JobStarting); j.Status.Attempts != 2 {
		t.Fatalf("attempts %d", j.Status.Attempts)
	}
	if r := ranksBecome(t, id, 1)[0]; r.Annotations["devpod.io/attempt"] != "2" {
		t.Fatalf("the new rank's attempt %q", r.Annotations["devpod.io/attempt"])
	}
}

// A status worked out from a job read before it was cancelled is not
// written over the cancel.
func TestStaleWriteDoesNotUndoCancel(t *testing.T) {
	p, _ := queueRig(t, "qx", "qx-alice")
	ctx := context.Background()
	id := submit(t, p, "qx-alice", 1, 1, nil)
	stale := jobNow(t, id) // as a round read it
	if rec := doJSON(t, p.s.HandleCancelJobForTest(), "DELETE", "/api/jobs/"+id, map[string]string{"id": id}, p.as("qx-alice"), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	stale.Status.Phase = devpodv1alpha1.JobStarting
	if p.s.WriteJobStatusForTest(ctx, &stale) {
		t.Fatal("a stale write went through")
	}
	if j := jobNow(t, id); j.Status.Phase != devpodv1alpha1.JobCancelled {
		t.Fatalf("the cancel was undone: %s", j.Status.Phase)
	}
}

// The quota counts the owner's jobs the same round starts.
func TestQueueQuotaCountsSameRound(t *testing.T) {
	p, _ := queueRig(t, "qq", "qq-alice")
	gpuMachine(t, p.n2)
	p.s.DefaultQuota.Compute[gpures.NVIDIA] = resource.MustParse("8")
	a := submit(t, p, "qq-alice", 1, 8, nil)
	b := submit(t, p, "qq-alice", 1, 8, nil)
	p.s.QueueRound(context.Background())
	ja, jb := jobNow(t, a), jobNow(t, b)
	if ja.Status.Phase != devpodv1alpha1.JobStarting || jb.Status.Phase != devpodv1alpha1.JobQueued || jb.Status.Reason != "quota" {
		t.Fatalf("a %s, b %s %s", ja.Status.Phase, jb.Status.Phase, jb.Status.Reason)
	}
}

// A job's ranks are not DevPods of its owner's to count: a two-machine
// job runs for one at their limit of DevPods.
func TestJobRanksNotCountedAsDevPods(t *testing.T) {
	p, _ := queueRig(t, "qn", "qn-alice")
	gpuMachine(t, p.n2)
	p.s.DefaultQuota.MaxDevPods = ptr.To(int32(1)) // dev1 is theirs
	id := submit(t, p, "qn-alice", 2, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranksBecome(t, id, 2)
}

// A job whose ranks time after time do not all start fails, saying why.
func TestGangTimeoutsEndTheJob(t *testing.T) {
	p, clock := queueRig(t, "qc", "qc-alice")
	ctx := context.Background()
	id := submit(t, p, "qc-alice", 1, 1, nil)
	for n := int32(1); n < 3; n++ {
		roundUntil(t, p, id, devpodv1alpha1.JobStarting)
		ranksBecome(t, id, 1)
		*clock = clock.Add(6 * time.Minute)
		waitFor(t, func() bool {
			p.s.QueueRound(ctx)
			j := jobNow(t, id)
			return j.Status.Phase == devpodv1alpha1.JobQueued && j.Status.GangTimeouts == n
		})
		ranksBecome(t, id, 0)
	}
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranksBecome(t, id, 1)
	*clock = clock.Add(6 * time.Minute)
	j := roundUntil(t, p, id, devpodv1alpha1.JobFailed)
	if j.Status.Reason != "gang-timeout" || j.Status.Detail["why"] == "" || j.Status.GangTimeouts != 3 {
		t.Fatalf("%+v", j.Status)
	}
}

// A cordoned machine takes no rank.
func TestQueueSkipsCordonedMachine(t *testing.T) {
	p, _ := queueRig(t, "qk", "qk-alice")
	ctx := context.Background()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var n corev1.Node
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.n1}, &n); err != nil {
			return err
		}
		n.Spec.Unschedulable = true
		return k8sClient.Update(ctx, &n)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var n corev1.Node
		return k8sClient.Get(ctx, types.NamespacedName{Name: p.n1}, &n) == nil && n.Spec.Unschedulable
	})
	id := submit(t, p, "qk-alice", 1, 1, nil)
	for range 3 {
		p.s.QueueRound(ctx)
	}
	if j := jobNow(t, id); j.Status.Phase != devpodv1alpha1.JobQueued {
		t.Fatalf("started on a cordoned machine: %s", j.Status.Phase)
	}
}

// blockingLogs: a rank's output that comes only once release is closed.
type blockingLogs struct{ release chan struct{} }

func (b blockingLogs) Read(p []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

// An ended job's output is kept off the round: a log slow to read holds
// up no other job, and the ranks go once it is kept.
func TestEndJobDoesNotWaitForLogs(t *testing.T) {
	p, _ := queueRig(t, "qb", "qb-alice")
	objs := &memObjects{}
	p.s.Audit = &webui.AuditDeps{Store: &fakeAuditStore{}, Objects: objs}
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	p.s.PodLogs = func(context.Context, string, string, string, string, bool) (io.ReadCloser, error) {
		return io.NopCloser(blockingLogs{release}), nil
	}
	id := submit(t, p, "qb-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	r := ranksBecome(t, id, 1)[0]
	setRank(t, r.Name, devpodv1alpha1.DevPodRunning, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobRunning)
	zero := int32(0)
	setRank(t, r.Name, devpodv1alpha1.DevPodSucceeded, &zero)
	done := make(chan devpodv1alpha1.Job, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for ctx.Err() == nil {
			p.s.QueueRound(ctx)
			var j devpodv1alpha1.Job
			if k8sManager.GetAPIReader().Get(ctx, types.NamespacedName{Name: id}, &j) == nil && j.Status.Phase == devpodv1alpha1.JobSucceeded {
				done <- j
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	select {
	case j := <-done:
		if j.Status.Ranks[0].Log != "jobs/-/"+id+"/rank-0.log" {
			t.Fatalf("log key %q", j.Status.Ranks[0].Log)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the round waited on the rank's output")
	}
	if len(rankDevPods(t, id)) != 1 {
		t.Fatal("the rank went before its output was kept")
	}
	close(release)
	ranksBecome(t, id, 0)
	waitFor(t, func() bool {
		objs.mu.Lock()
		defer objs.mu.Unlock()
		_, ok := objs.m["jobs/-/"+id+"/rank-0.log"]
		return ok
	})
}

// Live output: a line ends at \n, \r\n or a lone \r (a progress bar), and
// a very long one goes in pieces — none of it ends the stream.
func TestJobLogsLineBreaks(t *testing.T) {
	p, _ := queueRig(t, "qr", "qr-alice")
	long := strings.Repeat("x", 2<<20)
	p.s.PodLogs = func(context.Context, string, string, string, string, bool) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("a\rb\r\nc\n" + long + "\nend")), nil
	}
	id := submit(t, p, "qr-alice", 1, 1, nil)
	roundUntil(t, p, id, devpodv1alpha1.JobStarting)
	ranksBecome(t, id, 1)
	rec := doJSON(t, p.s.HandleJobLogsForTest(), "GET", "/api/jobs/"+id+"/logs?rank=0&follow=1", map[string]string{"id": id}, p.as("qr-alice"), "")
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: a\n\ndata: b\n\ndata: c\n\n") || !strings.HasSuffix(body, "data: end\n\n") {
		t.Fatalf("lines: %q … %q", body[:min(len(body), 60)], body[max(0, len(body)-30):])
	}
	xs := 0
	for _, ev := range strings.Split(body, "\n\n") {
		d := strings.TrimPrefix(ev, "data: ")
		if len(d) > 64<<10 {
			t.Fatalf("an event of %d bytes", len(d))
		}
		if strings.Trim(d, "x") == "" {
			xs += len(d)
		}
	}
	if xs != len(long) {
		t.Fatalf("%d of %d bytes of the long line", xs, len(long))
	}
}

// A command from inside a DevPod (dp) asks to be no admin, and is not.
func TestGatewayDropAdmin(t *testing.T) {
	setupSuite(t)
	s, _ := newServer(t)
	s.Admins = map[string]bool{"gd-root": true}
	s.CLIKey = []byte("gateway-shared-key")
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	admin := func(drop bool) bool {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer gwk_gateway-shared-key")
		req.Header.Set("X-DevPod-User", "gd-root")
		if drop {
			req.Header.Set("X-DevPod-Drop-Admin", "1")
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var me struct{ Admin bool }
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&me) != nil {
			t.Fatalf("me: %d", resp.StatusCode)
		}
		return me.Admin
	}
	if !admin(false) {
		t.Fatal("the admin is no admin")
	}
	if admin(true) {
		t.Fatal("dropped, still an admin")
	}
}
