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

func TestWatchEventsStreams(t *testing.T) {
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
		s.HandleWatchDevPodEventsForTest()(w, r)
	}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("GET", srv.URL+"/api/devpods/gl-alice-dev1/events?watch=true", nil)
	req.AddCookie(alice)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	// One event for alice's pod, one for an unrelated object.
	ctx := context.Background()
	mine := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "gl-alice-dev1.stream", Namespace: "devpods"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "gl-alice-dev1", Namespace: "devpods"},
		Reason:         "StreamTest", Message: "hello from sse", Type: "Normal",
		LastTimestamp: metav1.Now(),
	}
	other := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "other.stream", Namespace: "devpods"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "gl-bob-other", Namespace: "devpods"},
		Reason:         "MustNotLeak", Message: "not yours", Type: "Normal",
		LastTimestamp: metav1.Now(),
	}
	if err := k8sClient.Create(ctx, mine); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mine)
		_ = k8sClient.Delete(ctx, other)
	})

	// Generous: under the full -race suite several envtest apiservers
	// run concurrently and informer delivery can lag.
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if l := scanner.Text(); strings.HasPrefix(l, "data: ") {
				lines <- l
			}
		}
	}()
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, "MustNotLeak") {
				t.Fatalf("leaked unrelated event: %s", l)
			}
			if strings.Contains(l, "StreamTest") {
				return // success
			}
		case <-deadline.Done():
			t.Fatal("timed out waiting for event on stream")
		}
	}
}
