// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

type jobV struct {
	ID, Owner, Phase, Reason, Command string
	Nodes, GPUs, Priority             int32
	CPUs                              float64
	Mine, CanManage, Requeue, Held    bool
	Position                          int32
	Code, Message                     string
}

// jobRig: placeRig's machines (n1 reservable, 8 cards, 192 cores), the
// queue on, and user's DevPod <user>-dev1 to make jobs from.
func jobRig(t *testing.T, prefix, user string) *placeRig {
	t.Helper()
	p := newPlaceRig(t, prefix)
	queueOn(p.s)
	// room in the quota for a job's ranks
	p.s.DefaultQuota = devpodv1alpha1.UserQuota{MaxDevPods: ptr.To(int32(100)), Compute: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1000"), corev1.ResourceMemory: resource.MustParse("4Ti")}}
	t.Cleanup(func() {
		var l devpodv1alpha1.JobList
		_ = k8sClient.List(context.Background(), &l)
		for i := range l.Items {
			_ = k8sClient.Delete(context.Background(), &l.Items[i])
		}
	})
	if user != "" {
		p.create(user, http.StatusCreated)
	}
	return p
}

func submitJob(t *testing.T, s *webui.Server, c *http.Cookie, body map[string]any) (int, jobV) {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := doJSON(t, s.HandleSubmitJobForTest(), "POST", "/api/jobs", nil, c, string(b))
	var v jobV
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, v
}

func listJobs(t *testing.T, s *webui.Server, c *http.Cookie, query string) []jobV {
	t.Helper()
	rec := doJSON(t, s.HandleListJobsForTest(), "GET", "/api/jobs"+query, nil, c, "")
	var out struct{ Items []jobV }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	return out.Items
}

// A job is made from the owner's DevPod and waits in the queue; everyone
// sees it, only its owner what it runs.
func TestJobSubmit(t *testing.T) {
	p := jobRig(t, "jb", "jb-alice")
	code, v := submitJob(t, p.s, p.as("jb-alice"), map[string]any{"from": map[string]any{"devpod": "jb-alice-dev1"}, "nodes": 1, "gpus": 4, "command": "python t.py"})
	if code != http.StatusCreated || len(v.ID) != 5 || v.Phase != "Queued" || v.CPUs != 96 || v.Command != "python t.py" {
		t.Fatalf("submit: %d %+v", code, v)
	}
	for _, j := range listJobs(t, p.s, p.as("jb-bob"), "") {
		if j.ID == v.ID && (j.Command != "" || j.Mine || j.Owner != "jb-alice") {
			t.Fatalf("bob sees %+v", j)
		}
	}
	found := false
	for _, j := range listJobs(t, p.s, p.as("jb-alice"), "?mine=1") {
		found = found || (j.ID == v.ID && j.Command == "python t.py" && j.Mine)
	}
	if !found {
		t.Fatal("alice does not see her job")
	}
}

// What cannot be had is refused at once, with why.
func TestJobSubmitRefusals(t *testing.T) {
	p := jobRig(t, "jr", "jr-alice")
	alice := p.as("jr-alice")
	base := func(extra map[string]any) map[string]any {
		b := map[string]any{"from": map[string]any{"devpod": "jr-alice-dev1"}, "nodes": 1, "gpus": 4, "command": "true"}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	for name, c := range map[string]struct {
		body     map[string]any
		want     int
		wantCode string
	}{
		"priority over the cap": {base(map[string]any{"priority": 5}), http.StatusForbidden, "PRIORITY_TOO_HIGH"},
		"too long":              {base(map[string]any{"timeLimitSeconds": 100 * 3600}), http.StatusBadRequest, ""},
		"two machines, one":     {base(map[string]any{"nodes": 2}), http.StatusConflict, "NO_MACHINE"},
		"more cards than any":   {base(map[string]any{"gpus": 9}), http.StatusConflict, "NO_MACHINE"},
		"no command":            {base(map[string]any{"command": ""}), http.StatusBadRequest, ""},
		"no source":             {map[string]any{"nodes": 1, "gpus": 1, "command": "true"}, http.StatusBadRequest, ""},
	} {
		code, v := submitJob(t, p.s, alice, c.body)
		if code != c.want || (c.wantCode != "" && v.Code != c.wantCode) {
			t.Errorf("%s: %d %+v", name, code, v)
		}
	}
	// a privilege the rules give on no machine
	setRules(t, onNode(p.n1, devpodv1alpha1.AccessEffect{Allow: true}))
	if code, v := submitJob(t, p.s, alice, base(map[string]any{"privileges": map[string]any{"hostNetwork": true}})); code != http.StatusForbidden || v.Code != "PRIVILEGE_DENIED" {
		t.Errorf("host network: %d %+v", code, v)
	}
	// a source with its own disk
	ctx := context.Background()
	pv := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "jr-alice-pv", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: "jr-alice",
		Persistence: &devpodv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi"), MountPath: "/root"},
		Pod:         &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}}}
	if err := k8sClient.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pv) })
	waitDevPod(t, "jr-alice-pv")
	if code, v := submitJob(t, p.s, alice, base(map[string]any{"from": map[string]any{"devpod": "jr-alice-pv"}})); code != http.StatusBadRequest || !strings.Contains(v.Message, "disk") {
		t.Errorf("persistence: %d %+v", code, v)
	}
	// a name too long for its ranks
	long := "jr-" + strings.Repeat("x", 17)
	ld := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: long + "-d", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: long,
		Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}}}
	if err := k8sClient.Create(ctx, ld); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ld) })
	waitDevPod(t, long+"-d")
	setRules(t)
	if code, v := submitJob(t, p.s, p.as(long), base(map[string]any{"from": map[string]any{"devpod": long + "-d"}})); code != http.StatusBadRequest || !strings.Contains(v.Message, "too long") {
		t.Errorf("long name: %d %+v", code, v)
	}
}

// A job waiting is cancelled at once; only its owner or an admin may.
func TestJobCancel(t *testing.T) {
	p := jobRig(t, "jc", "jc-alice")
	p.s.Admins = map[string]bool{"jc-root": true}
	mk := func() string {
		code, v := submitJob(t, p.s, p.as("jc-alice"), map[string]any{"from": map[string]any{"devpod": "jc-alice-dev1"}, "nodes": 1, "gpus": 1, "command": "true"})
		if code != http.StatusCreated {
			t.Fatalf("submit: %d %+v", code, v)
		}
		return v.ID
	}
	cancel := func(c *http.Cookie, id string) int {
		return doJSON(t, p.s.HandleCancelJobForTest(), "DELETE", "/api/jobs/"+id, map[string]string{"id": id}, c, "").Code
	}
	a, b := mk(), mk()
	if code := cancel(p.as("jc-bob"), a); code != http.StatusForbidden {
		t.Fatalf("bob cancels alice's: %d", code)
	}
	if code := cancel(p.as("jc-alice"), a); code != http.StatusNoContent {
		t.Fatalf("alice cancels: %d", code)
	}
	if code := cancel(forge(p.sm, "jc-root", true), b); code != http.StatusNoContent {
		t.Fatalf("an admin cancels: %d", code)
	}
	for _, j := range listJobs(t, p.s, p.as("jc-alice"), "?mine=1&ended=1") {
		if (j.ID == a || j.ID == b) && j.Phase != "Cancelled" {
			t.Fatalf("%s: %s", j.ID, j.Phase)
		}
	}
}

// The priority caps are the admins'.
func TestPriorityRulesAdmin(t *testing.T) {
	p := jobRig(t, "pa", "")
	p.s.Admins = map[string]bool{"pa-root": true}
	root := forge(p.sm, "pa-root", true)
	put := func(c *http.Cookie, body string) int {
		return doJSON(t, p.s.HandlePutPriorityRulesForTest(), "PUT", "/api/admin/job-priority-rules", nil, c, body).Code
	}
	if code := put(p.as("pa-bob"), `{"items":[]}`); code != http.StatusForbidden {
		t.Fatalf("bob: %d", code)
	}
	if code := put(root, `{"items":[{"condition":"user = = ","maxPriority":5}]}`); code != http.StatusBadRequest {
		t.Fatalf("broken: %d", code)
	}
	if code := put(root, `{"items":[{"condition":"group = lab","maxPriority":100},{"condition":"user = pa-alice","maxPriority":200}]}`); code != http.StatusOK {
		t.Fatalf("put: %d", code)
	}
	waitFor(t, func() bool {
		rec := doJSON(t, p.s.HandleGetPriorityRulesForTest(), "GET", "/api/admin/job-priority-rules", nil, root, "")
		var out struct {
			Items []struct {
				Condition   string
				MaxPriority int32
			}
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return len(out.Items) == 2
	})
	if code := put(root, `{"items":[{"condition":"group = lab","maxPriority":1,"maxJobs":0}]}`); code != http.StatusBadRequest {
		t.Fatalf("maxJobs 0: %d", code)
	}
	if code := put(root, `{"items":[{"condition":"group = lab","maxPriority":10,"maxJobs":30}]}`); code != http.StatusOK {
		t.Fatalf("replace: %d", code)
	}
	waitFor(t, func() bool {
		var l devpodv1alpha1.JobPriorityRuleList
		return k8sClient.List(context.Background(), &l) == nil && len(l.Items) == 1 && l.Items[0].Spec.MaxPriority == 10 &&
			l.Items[0].Spec.MaxJobs != nil && *l.Items[0].Spec.MaxJobs == 30
	})
	rec := doJSON(t, p.s.HandleGetPriorityRulesForTest(), "GET", "/api/admin/job-priority-rules", nil, root, "")
	if !strings.Contains(rec.Body.String(), `"maxJobs":30`) {
		t.Fatalf("get: %s", rec.Body)
	}
}

// A job runs where the machines share storage: under the owner's shared
// home (or on a shared volume of the source), never in what only the
// DevPod's machine has.
func TestJobWorkdirMustBeShared(t *testing.T) {
	p := jobRig(t, "jw", "jw-alice")
	alice := p.as("jw-alice")
	body := func(wd string) map[string]any {
		return map[string]any{"from": map[string]any{"devpod": "jw-alice-dev1"}, "nodes": 1, "gpus": 1, "command": "true", "workdir": wd}
	}
	if code, v := submitJob(t, p.s, alice, body("/root/x")); code != http.StatusBadRequest || v.Code != "WORKDIR_NOT_SHARED" || !strings.Contains(v.Message, "/home/jw-alice") {
		t.Errorf("/root/x: %d %+v", code, v)
	}
	if code, v := submitJob(t, p.s, alice, body("x")); code != http.StatusBadRequest {
		t.Errorf("relative: %d %+v", code, v)
	}
	if code, v := submitJob(t, p.s, alice, body("/home/jw-alice/proj")); code != http.StatusCreated {
		t.Errorf("in the shared home: %d %+v", code, v)
	}
	if code, v := submitJob(t, p.s, alice, body("/home/jw-bob/proj")); code != http.StatusBadRequest {
		t.Errorf("another's home is not one's own: %d %+v", code, v)
	}
}

// Where the controller has not said it runs job ranks (an older one), the
// cluster takes no jobs.
func TestQueueOffWithoutTheControllersWord(t *testing.T) {
	setupSuite(t)
	controllerSays("", "/home")
	t.Cleanup(func() { controllerSays(devpodv1alpha1.FeatureJobs, "/home") })
	s, sm := newServer(t)
	s.Queue = &webui.QueueRules{DefaultTimeLimit: 24 * time.Hour, MaxTimeLimit: 72 * time.Hour, GangTimeout: 5 * time.Minute, Tick: 5 * time.Second, KeepEnded: time.Hour}
	code, v := submitJob(t, s, forge(sm, "jq-alice", false), map[string]any{"from": map[string]any{"devpod": "jq-alice-dev1"}, "nodes": 1, "gpus": 1, "command": "true"})
	if code != http.StatusNotFound || v.Code != "QUEUE_OFF" {
		t.Fatalf("submit: %d %+v", code, v)
	}
}

// A source whose own placement rules out every machine of the queue is
// refused at once, not left waiting.
func TestJobSubmitPlacementNowhere(t *testing.T) {
	p := jobRig(t, "jp", "jp-alice")
	ctx := context.Background()
	arm := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "jp-alice-arm", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: "jp-alice",
		Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/arch": "arm64"}, Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}}}
	if err := k8sClient.Create(ctx, arm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, arm) })
	waitDevPod(t, arm.Name)
	code, v := submitJob(t, p.s, p.as("jp-alice"), map[string]any{"from": map[string]any{"devpod": arm.Name}, "nodes": 1, "gpus": 1, "command": "true"})
	if code != http.StatusConflict || v.Code != "NO_MACHINE" {
		t.Fatalf("arm64 only, no such machine: %d %+v", code, v)
	}
}

// One person has at most so many jobs waiting or running; an admin is not
// held to it, and an ended or cancelled job frees its place.
func TestJobCountCap(t *testing.T) {
	p := jobRig(t, "jn", "jn-alice")
	p.s.Queue.MaxJobsPerUser = 2
	alice := p.as("jn-alice")
	body := map[string]any{"from": map[string]any{"devpod": "jn-alice-dev1"}, "nodes": 1, "gpus": 1, "command": "true"}
	var ids []string
	for range 2 {
		code, v := submitJob(t, p.s, alice, body)
		if code != http.StatusCreated {
			t.Fatalf("submit: %d %+v", code, v)
		}
		ids = append(ids, v.ID)
	}
	if code, v := submitJob(t, p.s, alice, body); code != http.StatusTooManyRequests || v.Code != "JOB_LIMIT" {
		t.Fatalf("a third: %d %+v", code, v)
	}
	dry := map[string]any{"dryRun": true}
	for k, v := range body {
		dry[k] = v
	}
	if code, v := submitJob(t, p.s, alice, dry); code != http.StatusOK {
		t.Fatalf("a dry run is not held to it: %d %+v", code, v)
	}
	if rec := doJSON(t, p.s.HandleCancelJobForTest(), "DELETE", "/api/jobs/"+ids[0], map[string]string{"id": ids[0]}, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rec.Code)
	}
	waitFor(t, func() bool { code, _ := submitJob(t, p.s, alice, body); return code == http.StatusCreated })
}

// A job's environment is bounded: at most 512 variables, 64 KiB.
func TestJobEnvBounded(t *testing.T) {
	p := jobRig(t, "je", "je-alice")
	env := map[string]any{}
	for i := range 600 {
		env[fmt.Sprintf("V%d", i)] = "1"
	}
	code, v := submitJob(t, p.s, p.as("je-alice"), map[string]any{"from": map[string]any{"devpod": "je-alice-dev1"}, "nodes": 1, "gpus": 1, "command": "true", "env": env})
	if code != http.StatusBadRequest || !strings.Contains(v.Message, "environment") {
		t.Fatalf("600 variables: %d %+v", code, v)
	}
	code, v = submitJob(t, p.s, p.as("je-alice"), map[string]any{"from": map[string]any{"devpod": "je-alice-dev1"}, "nodes": 1, "gpus": 1, "command": "true",
		"env": map[string]any{"BIG": strings.Repeat("x", 70<<10)}})
	if code != http.StatusBadRequest {
		t.Fatalf("70 KiB: %d %+v", code, v)
	}
}

// How many jobs one may have is the job rules' as priority is: the most of
// those whose condition holds for them (none: the console's default), in
// the cluster submitted to — jobs elsewhere are not counted there.
func TestJobCountCapRules(t *testing.T) {
	p := jobRig(t, "jr", "jr-alice")
	p.s.Queue.MaxJobsPerUser = 1
	alice := p.as("jr-alice")
	body := map[string]any{"from": map[string]any{"devpod": "jr-alice-dev1"}, "nodes": 1, "gpus": 1, "command": "true"}
	ctx := context.Background()
	code, a := submitJob(t, p.s, alice, body)
	if code != http.StatusCreated {
		t.Fatalf("submit: %d %+v", code, a)
	}
	var job devpodv1alpha1.Job
	if err := k8sClient.Get(ctx, client_key(a.ID), &job); err != nil {
		t.Fatal(err)
	}
	elsewhere := &devpodv1alpha1.Job{ObjectMeta: metav1.ObjectMeta{Name: a.ID + "x", Namespace: job.Namespace}, Spec: *job.Spec.DeepCopy()}
	elsewhere.Spec.Cluster = "elsewhere"
	if err := k8sClient.Create(ctx, elsewhere); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, elsewhere) })
	if rec := doJSON(t, p.s.HandleCancelJobForTest(), "DELETE", "/api/jobs/"+a.ID, map[string]string{"id": a.ID}, alice, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rec.Code)
	}
	waitFor(t, func() bool { code, _ := submitJob(t, p.s, alice, body); return code == http.StatusCreated })
	if code, v := submitJob(t, p.s, alice, body); code != http.StatusTooManyRequests || v.Code != "JOB_LIMIT" {
		t.Fatalf("over the default: %d %+v", code, v)
	}
	three := int32(3)
	rule := &devpodv1alpha1.JobPriorityRule{ObjectMeta: metav1.ObjectMeta{Name: "jr-cap"}, Spec: devpodv1alpha1.JobPriorityRuleSpec{Condition: "user = jr-alice", MaxJobs: &three}}
	if err := k8sClient.Create(ctx, rule); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rule) })
	waitFor(t, func() bool { code, _ := submitJob(t, p.s, alice, body); return code == http.StatusCreated })
	if code, v := submitJob(t, p.s, alice, body); code != http.StatusCreated {
		t.Fatalf("the third: %d %+v", code, v)
	}
	if code, v := submitJob(t, p.s, alice, body); code != http.StatusTooManyRequests || !strings.Contains(v.Message, "3") {
		t.Fatalf("a fourth: %d %+v", code, v)
	}
}

// The queue's list holds the DevPods waiting in line: everyone sees whose
// and how big, only the owner (and admins) which.
func TestListJobsIncludesQueuedDevPods(t *testing.T) {
	p := jobRig(t, "lq", "lq-alice") // lq-alice-dev1, made running: in line
	list := func(user string) []map[string]any {
		t.Helper()
		rec := doJSON(t, p.s.HandleListJobsForTest(), "GET", "/api/jobs", nil, p.as(user), "")
		var out struct{ Devpods []map[string]any }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		return out.Devpods
	}
	var mine []map[string]any
	waitFor(t, func() bool { mine = list("lq-alice"); return len(mine) == 1 })
	if mine[0]["name"] != "lq-alice-dev1" || mine[0]["owner"] != "lq-alice" || mine[0]["mine"] != true {
		t.Fatalf("alice's: %+v", mine[0])
	}
	theirs := list("lq-bob")
	if len(theirs) != 1 || theirs[0]["name"] != nil || theirs[0]["owner"] != "lq-alice" || theirs[0]["mine"] != false {
		t.Fatalf("bob's: %+v", theirs)
	}
}
