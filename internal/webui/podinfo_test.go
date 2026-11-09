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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/webui"
)

func twoContainerDevPod(name, owner string, target string) *devpodv1alpha1.DevPod {
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{Owner: owner, Running: true, Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "side", Image: "busybox"}, {Name: "dev", Image: "busybox"}},
		}}},
	}
	if target != "" {
		dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi"), MountPath: "/home", TargetContainer: target}
	}
	return dp
}

func runningPod(name string, started map[string]time.Time) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods", Labels: map[string]string{"devpod.io/devpod": name}},
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "side", Image: "busybox"}, {Name: "dev", Image: "busybox"}}},
	}
	p.Status.PodIP = "10.1.2.3"
	for _, c := range []string{"side", "dev"} {
		st := corev1.ContainerStatus{Name: c}
		if at, ok := started[c]; ok {
			st.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at)}
		} else {
			st.State.Waiting = &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}
		}
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, st)
	}
	return p
}

func TestPodInfoFor_TargetContainerStartedAt(t *testing.T) {
	t0 := time.Unix(1794210460, 0).UTC()
	t1 := t0.Add(time.Hour)
	pod := runningPod("x", map[string]time.Time{"side": t0, "dev": t1})

	info := webui.PodInfoFor(twoContainerDevPod("x", "a", "dev"), pod)
	if info == nil || info.Node != "node-a" || info.IP != "10.1.2.3" || info.StartedAt == nil || !info.StartedAt.Time.Equal(t1) {
		t.Fatalf("persistence target: %+v", info)
	}
	if info := webui.PodInfoFor(twoContainerDevPod("x", "a", ""), pod); info.StartedAt == nil || !info.StartedAt.Time.Equal(t0) {
		t.Fatalf("default target is containers[0]: %+v", info)
	}
	waiting := runningPod("x", map[string]time.Time{"side": t0})
	if info := webui.PodInfoFor(twoContainerDevPod("x", "a", "dev"), waiting); info.StartedAt != nil {
		t.Fatalf("target not running: %+v", info)
	}
	if webui.PodInfoFor(twoContainerDevPod("x", "a", ""), nil) != nil {
		t.Fatal("nil pod must give nil info")
	}
}

func createPodWithStatus(t *testing.T, p *corev1.Pod) {
	t.Helper()
	status := p.Status
	if err := k8sClient.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Status = status
	if err := k8sClient.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestListDevPods_IncludesPods(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	dp := twoContainerDevPod("gl-dave-pi", "gl-dave", "")
	if err := k8sClient.Create(ctx, dp); err != nil {
		t.Fatal(err)
	}
	started := time.Unix(1794210460, 0).UTC()
	createPodWithStatus(t, runningPod("gl-dave-pi", map[string]time.Time{"side": started}))
	waitDevPod(t, "gl-dave-pi")
	waitFor(t, func() bool {
		var p corev1.Pod
		return k8sClient.Get(ctx, clientKeyNS("gl-dave-pi"), &p) == nil && p.Status.PodIP != ""
	})

	rec := doJSON(t, s.HandleListDevPodsForTest(), "GET", "/api/devpods", nil, forge(sm, "gl-dave", false), "")
	var body struct {
		Pods map[string]webui.PodInfo `json:"pods"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	p, ok := body.Pods["gl-dave-pi"]
	if !ok || p.Node != "node-a" || p.StartedAt == nil || !p.StartedAt.Time.Equal(started) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestWatchDevPods_PodUpdatePushesOwningDevPod(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	ctx := context.Background()
	if err := k8sClient.Create(ctx, twoContainerDevPod("gl-erin-w", "gl-erin", "")); err != nil {
		t.Fatal(err)
	}
	pod := runningPod("gl-erin-w", nil)
	createPodWithStatus(t, pod)
	waitDevPod(t, "gl-erin-w")

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWatchDevPodsForTest()))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/api/devpods?watch=true", nil)
	req.AddCookie(forge(sm, "gl-erin", false))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				lines <- strings.TrimPrefix(l, "data: ")
			}
		}
	}()
	time.Sleep(500 * time.Millisecond) // let the handler register

	// The container (re)starts: the Pod update must push the DevPod.
	restarted := time.Unix(1794299999, 0).UTC()
	var cur corev1.Pod
	if err := k8sClient.Get(ctx, clientKeyNS("gl-erin-w"), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(restarted)}}
	if err := k8sClient.Status().Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case l := <-lines:
			var ev struct {
				Type   string                 `json:"type"`
				DevPod *devpodv1alpha1.DevPod `json:"devpod"`
				Pod    *webui.PodInfo         `json:"pod"`
			}
			_ = json.Unmarshal([]byte(l), &ev)
			if ev.Type == "MODIFIED" && ev.DevPod != nil && ev.DevPod.Name == "gl-erin-w" && ev.Pod != nil &&
				ev.Pod.StartedAt != nil && ev.Pod.StartedAt.Time.Equal(restarted) {
				return
			}
		case <-deadline:
			t.Fatal("no MODIFIED event carrying the new startedAt")
		}
	}
}
