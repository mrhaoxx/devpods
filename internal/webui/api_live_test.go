// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/live"
)

// sseData opens an SSE stream and returns its data payloads.
func sseData(t *testing.T, url string, cookie *http.Cookie) (int, <-chan string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 64)
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return resp.StatusCode, out
	}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				out <- strings.TrimPrefix(l, "data: ")
			}
		}
	}()
	return resp.StatusCode, out
}

func next(t *testing.T, ch <-chan string, match string) string {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case l := <-ch:
			if strings.Contains(l, match) {
				return l
			}
		case <-deadline:
			t.Fatalf("no event containing %q", match)
		}
	}
}

func createFor(t *testing.T, s *webui.Server, cookie *http.Cookie, name string) {
	t.Helper()
	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, cookie, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, name)
}

func publishing(t *testing.T, hub *live.Hub, snap func() live.Snapshot) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go live.Tick(ctx, hub, 100*time.Millisecond, snap)
}

func TestLiveDevPods_ScopeAndVisibility(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice, bob, admin := forge(sm, "gl-alice", false), forge(sm, "gl-bob", false), forge(sm, "root", true)
	createFor(t, s, alice, "gl-alice-dev1")
	createFor(t, s, bob, "gl-bob-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	a, b := waitDevPod(t, "gl-alice-dev1"), waitDevPod(t, "gl-bob-dev1")

	hub := live.NewHub()
	s.Live = &webui.LiveDeps{Hub: hub}
	cpu := 1.5
	publishing(t, hub, func() live.Snapshot {
		return live.Snapshot{TS: time.Now().UTC(), DevPods: map[string]live.DevPodLive{string(a.UID): {CPU: &cpu}, string(b.UID): {CPU: &cpu}}}
	})
	srv := httptest.NewServer(s.HandleLiveDevPodsForTest())
	t.Cleanup(srv.Close)

	names := func(raw string) []string {
		var ev struct {
			Items []struct {
				Name  string   `json:"name"`
				Owner string   `json:"owner"`
				CPU   *float64 `json:"cpu"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range ev.Items {
			if it.CPU == nil || *it.CPU != 1.5 {
				t.Fatalf("live data missing for %s: %s", it.Name, raw)
			}
			out = append(out, it.Owner+"/"+it.Name)
		}
		sort.Strings(out)
		return out
	}
	code, ch := sseData(t, srv.URL+"?scope=mine", bob)
	if code != http.StatusOK {
		t.Fatalf("bob mine: %d", code)
	}
	if got := strings.Join(names(next(t, ch, `"items"`)), ","); got != "gl-bob/gl-bob-dev1" {
		t.Fatalf("bob sees %s", got)
	}
	if code, _ := sseData(t, srv.URL+"?scope=all", bob); code != http.StatusForbidden {
		t.Fatalf("bob scope=all: %d", code)
	}
	code, ch = sseData(t, srv.URL+"?scope=all", admin)
	if code != http.StatusOK {
		t.Fatalf("admin all: %d", code)
	}
	if got := strings.Join(names(next(t, ch, `"items"`)), ","); got != "gl-alice/gl-alice-dev1,gl-bob/gl-bob-dev1" {
		t.Fatalf("admin sees %s", got)
	}
}

func TestOverview_AdminOnly(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	hub := live.NewHub()
	s.Live = &webui.LiveDeps{Hub: hub}
	publishing(t, hub, func() live.Snapshot {
		return live.Snapshot{TS: time.Now().UTC(), KPI: live.KPI{Running: 3}, Nodes: []live.NodeLive{{Name: "g206"}}}
	})
	srv := httptest.NewServer(s.HandleLiveOverviewForTest())
	t.Cleanup(srv.Close)
	if code, _ := sseData(t, srv.URL, forge(sm, "gl-bob", false)); code != http.StatusForbidden {
		t.Fatalf("non-admin: %d", code)
	}
	code, ch := sseData(t, srv.URL, forge(sm, "root", true))
	if code != http.StatusOK {
		t.Fatalf("admin: %d", code)
	}
	raw := next(t, ch, `"kpi"`)
	if !strings.Contains(raw, `"running":3`) || !strings.Contains(raw, `"g206"`) || strings.Contains(raw, `"devpods":{`) {
		t.Fatalf("overview event %s", raw)
	}
}

type fakeSource struct{ frame live.DevPodFrame }

func (f fakeSource) DevPod(context.Context, string, bool) (live.DevPodFrame, error) {
	return f.frame, nil
}

func TestProcesses_RedactOtherUsersCmdline(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)
	createFor(t, s, alice, "gl-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	s.Live = &webui.LiveDeps{Hub: live.NewHub(), Source: fakeSource{frame: live.DevPodFrame{Processes: []live.ProcLive{
		{Pid: 5, Exe: "/usr/bin/python3", Cmdline: "python3 secret.py --token x", User: "carol"},
		{Pid: 6, Exe: "/bin/bash", Cmdline: "bash -l", User: "gl-alice"},
	}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("name", "gl-alice-dev1")
		s.HandleDevPodStreamForTest()(w, r)
	}))
	t.Cleanup(srv.Close)

	cmdlines := func(cookie *http.Cookie) map[int]string {
		code, ch := sseData(t, srv.URL, cookie)
		if code != http.StatusOK {
			t.Fatalf("stream: %d", code)
		}
		var ev struct {
			Processes []live.ProcLive `json:"processes"`
		}
		if err := json.Unmarshal([]byte(next(t, ch, `"kind":"processes"`)), &ev); err != nil {
			t.Fatal(err)
		}
		out := map[int]string{}
		for _, p := range ev.Processes {
			out[p.Pid] = p.Cmdline
		}
		return out
	}
	if got := cmdlines(alice); got[5] != "python3" || got[6] != "bash -l" {
		t.Fatalf("owner view %v: a collaborator's cmdline must be reduced to the exe name", got)
	}
	if got := cmdlines(forge(sm, "root", true)); got[5] != "python3 secret.py --token x" {
		t.Fatalf("admin view %v", got)
	}
}

func TestLiveInputs_Build(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	createFor(t, s, forge(sm, "gl-alice", false), "gl-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "g206-live", Labels: map[string]string{
		"nvidia.com/gpu.product": "A100", "feature.node.kubernetes.io/cpu-cpuid.AVX": "true"}}}
	if err := k8sClient.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), node) })
	node.Status = corev1.NodeStatus{
		Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64"), "nvidia.com/gpu": resource.MustParse("4")},
		Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}
	if err := k8sClient.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	li := &webui.LiveInputs{S: s}
	var in live.Inputs
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		in = li.Build(ctx)
		if len(in.Nodes) > 0 && in.Nodes[0].Ready {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var n *live.NodeRef
	for i := range in.Nodes {
		if in.Nodes[i].Name == "g206-live" {
			n = &in.Nodes[i]
		}
	}
	if n == nil || !n.Ready || n.CPUAllocatable != 64 || n.GPUAllocatable != 4 || n.GPUModel != "A100" || len(n.Labels) != 1 {
		t.Fatalf("node %+v", n)
	}
	if len(in.DevPods) != 1 || in.DevPods[0].Owner != "gl-alice" || in.DevPods[0].CPULimit != 2 || in.DevPods[0].MemLimit != 4<<30 {
		t.Fatalf("devpods %+v", in.DevPods)
	}
	if q := in.Quotas["gl-alice"]; q.CPU != 8 || q.Mem != 16<<30 {
		t.Fatalf("quota %+v", q)
	}
}

// Ingest authenticates every batch by DevPod UID: an index lookup, not a
// list of every DevPod (400 DevPods × 400 batches/s).
func TestDevPodByUID(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	createFor(t, s, forge(sm, "gl-alice", false), "gl-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })
	dp := waitDevPod(t, "gl-alice-dev1")
	got, ok := s.DevPodByUID(context.Background(), string(dp.UID))
	if !ok || got.Name != "gl-alice-dev1" || got.Spec.Owner != "gl-alice" {
		t.Fatalf("%v %+v", ok, got)
	}
	if _, ok := s.DevPodByUID(context.Background(), "no-such-uid"); ok {
		t.Fatal("unknown uid found")
	}
}
