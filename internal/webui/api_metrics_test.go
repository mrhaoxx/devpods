// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

// fakeTelemetryStore adds the phase-2 queries to the audit fake.
type fakeTelemetryStore struct {
	fakeAuditStore
	info       *store.ContainerInfoRow
	lastRange  [2]time.Time
	lastSess   map[string]time.Time
	recentUIDs []string
}

func (f *fakeTelemetryStore) MetricsRange(_ context.Context, uid string, from, to time.Time, step time.Duration) (*store.MetricSeries, error) {
	f.lastRange = [2]time.Time{from, to}
	v := 2.0
	return &store.MetricSeries{Step: 60, TS: []int64{from.Unix()}, Series: map[string][]*float64{"cpu": {&v}}}, nil
}

func (f *fakeTelemetryStore) RecentContainer(_ context.Context, uids []string, from, to time.Time, step time.Duration) (*store.RecentSeries, error) {
	f.recentUIDs = append([]string(nil), uids...)
	v := 1.5
	out := &store.RecentSeries{Step: int64(step / time.Second), TS: []int64{from.Unix()}, Items: map[string]store.RecentItem{}}
	for _, u := range uids {
		out.Items[u] = store.RecentItem{CPU: []*float64{&v}, Mem: []*float64{nil}}
	}
	return out, nil
}

func (f *fakeTelemetryStore) LatestInfo(_ context.Context, uid string, _ time.Time) (*store.ContainerInfoRow, *store.SnapshotRow, error) {
	if f.info == nil {
		return nil, nil, store.ErrNotFound
	}
	return f.info, nil, nil
}

func (f *fakeTelemetryStore) LastSessionByUser(context.Context, time.Time) (map[string]time.Time, error) {
	return f.lastSess, nil
}

func TestMetricsAPI_OwnerOnly(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)
	createFor(t, s, alice, "gl-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	st := &fakeTelemetryStore{}
	s.Audit = &webui.AuditDeps{Store: st}
	path := map[string]string{"name": "gl-alice-dev1"}
	if rec := doJSON(t, s.HandleDevPodMetricsForTest(), "GET", "/api/devpods/gl-alice-dev1/metrics?range=1h", path, forge(sm, "gl-bob", false), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bob: %d", rec.Code)
	}
	if rec := doJSON(t, s.HandleDevPodMetricsForTest(), "GET", "/api/devpods/gl-alice-dev1/metrics?range=1y", path, alice, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad range: %d", rec.Code)
	}
	rec := doJSON(t, s.HandleDevPodMetricsForTest(), "GET", "/api/devpods/gl-alice-dev1/metrics?range=24h", path, forge(sm, "root", true), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"series":{"cpu":[2]}`) {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	if d := st.lastRange[1].Sub(st.lastRange[0]); d != 24*time.Hour {
		t.Fatalf("range %v", d)
	}
	// Grafana-like presets (feedback round 1)
	for rg, d := range map[string]time.Duration{"3h": 3 * time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour} {
		if rec := doJSON(t, s.HandleDevPodMetricsForTest(), "GET", "/api/devpods/gl-alice-dev1/metrics?range="+rg, path, alice, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", rg, rec.Code, rec.Body)
		}
		if got := st.lastRange[1].Sub(st.lastRange[0]); got != d {
			t.Fatalf("%s: range %v", rg, got)
		}
	}
	if rec := doJSON(t, s.HandleDevPodMetricsForTest(), "GET", "/api/devpods/gl-alice-dev1/metrics?range=2h", path, alice, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("2h is not a preset: %d", rec.Code)
	}
}

func TestLiveRecentAPI(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "rc-alice", false)
	createFor(t, s, alice, "rc-alice-dev1")
	createFor(t, s, forge(sm, "rc-bob", false), "rc-bob-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	get := func(c *http.Cookie, target string, want int) map[string]any {
		t.Helper()
		rec := doJSON(t, s.HandleLiveRecentForTest(), "GET", target, nil, c, "")
		if rec.Code != want {
			t.Fatalf("GET %s: %d (want %d) %s", target, rec.Code, want, rec.Body)
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	get(alice, "/api/live/devpods/recent", http.StatusServiceUnavailable) // no telemetry store
	st := &fakeTelemetryStore{}
	s.Audit = &webui.AuditDeps{Store: st}
	out := get(alice, "/api/live/devpods/recent", http.StatusOK)
	items := out["items"].(map[string]any)
	if _, ok := items["rc-alice-dev1"]; !ok || len(items) != 1 || out["step"].(float64) != 15 {
		t.Fatalf("mine: %+v", out)
	}
	if len(st.recentUIDs) != 1 {
		t.Fatalf("asked the store for %v", st.recentUIDs)
	}
	get(alice, "/api/live/devpods/recent?scope=all", http.StatusForbidden)
	out = get(forge(sm, "rc-root", true), "/api/live/devpods/recent?scope=all", http.StatusOK)
	if items := out["items"].(map[string]any); items["rc-alice-dev1"] == nil || items["rc-bob-dev1"] == nil {
		t.Fatalf("all: %+v", out)
	}
	out = get(alice, "/api/live/devpods/recent?name=rc-alice-dev1", http.StatusOK)
	if items := out["items"].(map[string]any); len(items) != 1 {
		t.Fatalf("one: %+v", out)
	}
	get(alice, "/api/live/devpods/recent?name=rc-bob-dev1", http.StatusNotFound)
}

func TestInfoAPI_NoTelemetry(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)
	createFor(t, s, alice, "gl-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	st := &fakeTelemetryStore{}
	s.Audit = &webui.AuditDeps{Store: st}
	path := map[string]string{"name": "gl-alice-dev1"}
	rec := doJSON(t, s.HandleDevPodInfoForTest(), "GET", "/api/devpods/gl-alice-dev1/info", path, alice, "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"NO_TELEMETRY"`) {
		t.Fatalf("no rows: %d %s", rec.Code, rec.Body)
	}
	st.info = &store.ContainerInfoRow{TS: time.Now(), Info: `{"os":"Ubuntu 24.04","cpu_limit":2}`}
	rec = doJSON(t, s.HandleDevPodInfoForTest(), "GET", "/api/devpods/gl-alice-dev1/info", path, alice, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"os":"Ubuntu 24.04"`) {
		t.Fatalf("info: %d %s", rec.Code, rec.Body)
	}
	s.Audit = nil
	if rec := doJSON(t, s.HandleDevPodInfoForTest(), "GET", "/api/devpods/gl-alice-dev1/info", path, alice, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store: %d", rec.Code)
	}
}

func TestNodesAPI_Rollup(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)
	createFor(t, s, alice, "gl-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	dp := waitDevPod(t, "gl-alice-dev1")
	ctx := context.Background()

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "g206-nodes", Labels: map[string]string{"nvidia.com/gpu.product": "A100"}}}
	if err := k8sClient.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), node) })
	node.Status = corev1.NodeStatus{
		Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64"), corev1.ResourceMemory: resource.MustParse("256Gi"),
			"nvidia.com/gpu": resource.MustParse("4"), "rdma/hca_shared_devices_a": resource.MustParse("100")},
		Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}
	if err := k8sClient.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	lim := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"), "nvidia.com/gpu": resource.MustParse("1")}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gl-alice-dev1-pod", Namespace: "devpods", Labels: map[string]string{render.LabelDevPod: "gl-alice-dev1"}},
		Spec: corev1.PodSpec{NodeName: "g206-nodes", Containers: []corev1.Container{{Name: "dev", Image: "ubuntu",
			Resources: corev1.ResourceRequirements{Limits: lim, Requests: lim}}}},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod, client.GracePeriodSeconds(0)) })
	var cur devpodv1alpha1.DevPod
	_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dp), &cur)
	cur.Status.Phase = devpodv1alpha1.DevPodRunning
	if err := k8sClient.Status().Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}

	st := live.NewState(nil)
	li := &webui.LiveInputs{S: s}
	s.Live = &webui.LiveDeps{Hub: live.NewHub(), Snapshot: func(ctx context.Context) (live.Snapshot, bool) {
		st.Apply(string(dp.UID), []wire.Record{
			{K: wire.KContainer, C: &wire.Container{CPU: 1.5, MemWorkingSet: 1 << 30}},
			{K: wire.KGPU, G: &wire.GPU{Index: 0, UUID: "GPU-a", Util: 77}},
		})
		return st.Snapshot(li.Build(ctx)), true
	}}

	if rec := doJSON(t, s.HandleAdminNodesForTest(), "GET", "/api/admin/nodes", nil, alice, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: %d", rec.Code)
	}
	admin := forge(sm, "root", true)
	type nodeView struct {
		Name  string `json:"name"`
		Ready bool   `json:"ready"`
		CPU   struct {
			Allocatable, Allocated float64
			Used                   *float64
		} `json:"cpu"`
		GPU struct {
			Model       string
			Total, Used int
			Cards       []live.CardLive
		} `json:"gpu"`
		RDMA    []map[string]any `json:"rdma"`
		DevPods json.RawMessage  `json:"devpods"`
	}
	var got *nodeView
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && (got == nil || got.CPU.Allocated == 0) {
		rec := doJSON(t, s.HandleAdminNodesForTest(), "GET", "/api/admin/nodes", nil, admin, "")
		var body struct{ Items []nodeView }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		for i := range body.Items {
			if body.Items[i].Name == "g206-nodes" {
				got = &body.Items[i]
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got == nil || !got.Ready || got.CPU.Allocatable != 64 || got.CPU.Allocated != 2 || got.CPU.Used == nil || *got.CPU.Used != 1.5 {
		t.Fatalf("node cpu %+v", got)
	}
	if got.GPU.Model != "A100" || got.GPU.Total != 4 || got.GPU.Used != 1 || len(got.GPU.Cards) != 1 ||
		got.GPU.Cards[0].DevPod != "gl-alice-dev1" || got.GPU.Cards[0].Owner != "gl-alice" || got.GPU.Cards[0].Util != 77 {
		t.Fatalf("node gpu %+v", got.GPU)
	}
	if len(got.RDMA) != 1 || got.RDMA[0]["resource"] != "rdma/hca_shared_devices_a" {
		t.Fatalf("rdma %+v", got.RDMA)
	}
	rec := doJSON(t, s.HandleAdminNodeForTest(), "GET", "/api/admin/nodes/g206-nodes", map[string]string{"name": "g206-nodes"}, admin, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"gl-alice-dev1"`) {
		t.Fatalf("node detail: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, s.HandleAdminNodeForTest(), "GET", "/api/admin/nodes/nope", map[string]string{"name": "nope"}, admin, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node: %d", rec.Code)
	}
}

func TestUsersAPI_UsageWithoutClickHouse(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	u := &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "gl-carol"}}
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), u) })
	s.Live = &webui.LiveDeps{Hub: live.NewHub(), Snapshot: func(context.Context) (live.Snapshot, bool) {
		return live.Snapshot{Users: []live.UserLive{{User: "gl-carol", CPU: 2.5, Mem: 1 << 30, GPU: 1, Running: 1}}}, true
	}}
	admin := forge(sm, "root", true)
	carol := func() map[string]any {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			rec := doJSON(t, s.HandleListUsersForTest(), "GET", "/api/admin/users", nil, admin, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("users: %d %s", rec.Code, rec.Body)
			}
			var body struct{ Items []map[string]any }
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			for _, it := range body.Items {
				if it["name"] == "gl-carol" {
					return it
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("gl-carol never listed")
		return nil
	}
	it := carol()
	lu, _ := it["liveUsage"].(map[string]any)
	if lu == nil || lu["cpu"] != 2.5 || lu["gpu"] != float64(1) || lu["devpodsRunning"] != float64(1) {
		t.Fatalf("live usage %+v", it)
	}
	if _, ok := it["lastSession"]; ok {
		t.Fatalf("lastSession without ClickHouse: %+v", it)
	}
	when := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	s.Audit = &webui.AuditDeps{Store: &fakeTelemetryStore{lastSess: map[string]time.Time{"gl-carol": when}}}
	if it := carol(); it["lastSession"] != "2026-11-01T08:00:00Z" {
		t.Fatalf("lastSession %+v", it)
	}
}

func TestProcessesAPI_Redacted(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)
	createFor(t, s, alice, "gl-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	path := map[string]string{"name": "gl-alice-dev1"}
	if rec := doJSON(t, s.HandleDevPodProcessesForTest(), "GET", "/", path, alice, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("telemetry off: %d", rec.Code)
	}
	s.Live = &webui.LiveDeps{Hub: live.NewHub(), Source: fakeSource{frame: live.DevPodFrame{Processes: []live.ProcLive{
		{Pid: 5, Exe: "/usr/bin/python3", Cmdline: "python3 secret.py", User: "carol"}}}}}
	rec := doJSON(t, s.HandleDevPodProcessesForTest(), "GET", "/", path, alice, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cmdline":"python3"`) || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("owner: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, s.HandleDevPodProcessesForTest(), "GET", "/", path, forge(sm, "gl-bob", false), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger: %d", rec.Code)
	}
}

func TestSumRecentByNode(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	rs := &store.RecentSeries{Step: 15, TS: []int64{0, 15, 30}, Items: map[string]store.RecentItem{
		"u1": {CPU: []*float64{f(1), nil, f(3)}, Mem: []*float64{f(10), nil, f(30)}},
		"u2": {CPU: []*float64{f(2), nil, nil}, Mem: []*float64{f(20), nil, nil}},
		"u3": {CPU: []*float64{f(9), f(9), f(9)}, Mem: []*float64{nil, nil, nil}},
		"gone": {CPU: []*float64{f(5), f(5), f(5)}, Mem: []*float64{nil, nil, nil}},
	}}
	got := webui.SumRecentByNodeForTest(rs, map[string]string{"u1": "n1", "u2": "n1", "u3": "n2"})
	b, _ := json.Marshal(got)
	// a bucket nobody reported stays a gap; an unknown DevPod is left out
	want := `{"n1":{"cpu":[3,null,3],"mem":[30,null,30]},"n2":{"cpu":[9,9,9],"mem":[null,null,null]}}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}
