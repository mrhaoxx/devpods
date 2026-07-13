// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDevPodStream(t *testing.T) {
	setupSuite(t)
	s, sm := newServer(t)
	alice := forge(sm, "gl-alice", false)

	if rec := doJSON(t, s.HandleCreateDevPodForTest(), "POST", "/api/devpods", nil, alice, createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	waitDevPod(t, "gl-alice-dev1")
	t.Cleanup(func() { cleanupDevPods(t) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("name", "gl-alice-dev1")
		s.HandleDevPodStreamForTest()(w, r)
	}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("GET", srv.URL+"/api/devpods/gl-alice-dev1/stream", nil)
	req.AddCookie(alice)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	lines := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if l := scanner.Text(); strings.HasPrefix(l, "data: ") {
				lines <- l
			}
		}
	}()

	// The DevPod informer replays its cache on connect: we should get a
	// "devpod" message for gl-alice-dev1 without doing anything.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gotDevPod := false
	for !gotDevPod {
		select {
		case l := <-lines:
			if strings.Contains(l, `"kind":"devpod"`) && strings.Contains(l, "gl-alice-dev1") {
				gotDevPod = true
			}
		case <-ctx.Done():
			t.Fatal("no devpod message on connect")
		}
	}

	// Now create a related Event and an unrelated one; only ours streams.
	mine := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "gl-alice-dev1.stream", Namespace: "devpods"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "gl-alice-dev1", Namespace: "devpods"},
		Reason:         "StreamTest", Message: "hello", Type: "Normal", LastTimestamp: metav1.Now(),
	}
	other := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "other.stream", Namespace: "devpods"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "gl-bob-other", Namespace: "devpods"},
		Reason:         "MustNotLeak", Message: "nope", Type: "Normal", LastTimestamp: metav1.Now(),
	}
	if err := k8sClient.Create(context.Background(), mine); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), mine)
		_ = k8sClient.Delete(context.Background(), other)
	})

	for {
		select {
		case l := <-lines:
			if strings.Contains(l, "MustNotLeak") {
				t.Fatalf("leaked unrelated event: %s", l)
			}
			if strings.Contains(l, `"kind":"event"`) && strings.Contains(l, "StreamTest") {
				return // success
			}
		case <-ctx.Done():
			t.Fatal("no event message on stream")
		}
	}
}
