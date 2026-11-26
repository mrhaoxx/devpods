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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gatewaycli"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
	"github.com/mrhaoxx/devpod/internal/webui"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// dpRig: a console whose CLI answers /api/me as the user it is asked for,
// and two DevPods (alice's, bob's) with their UIDs.
func dpRig(t *testing.T) (*webui.Server, map[string]string) {
	t.Helper()
	setupSuite(t)
	s, _ := newServer(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"user": r.Header.Get("X-DevPod-User")})
	}))
	t.Cleanup(api.Close)
	s.CLI = &gatewaycli.Server{API: gatewaycli.NewAPI(api.URL, "k")}
	s.IngestKey = []byte("ingest-key")
	ctx := context.Background()
	uids := map[string]string{}
	for _, owner := range []string{"dpa", "dpb"} {
		dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: owner + "-dev", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: owner,
			Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}}}
		if err := k8sClient.Create(ctx, dp); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, dp) })
		uids[owner] = string(dp.UID)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, _, ok := s.DevPodByUID(ctx, uids["dpb"]); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the DevPods never reached the cache")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s, uids
}

// dpCall posts argv as the DevPod uid with token, over a link of cluster
// (""; none), and returns the status and the frames.
func dpCall(t *testing.T, s *webui.Server, uid, token, cluster string, argv ...string) (int, []map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"argv": argv, "cwd": "/home/x"})
	r := httptest.NewRequest(http.MethodPost, "/dp/v1/cli", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-DevPod-UID", uid)
	if cluster != "" {
		r = r.WithContext(clusters.WithLinkCluster(r.Context(), cluster))
	}
	w := httptest.NewRecorder()
	s.HandleDP(w, r)
	var frames []map[string]any
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	for sc.Scan() {
		var f map[string]any
		if json.Unmarshal(sc.Bytes(), &f) == nil {
			frames = append(frames, f)
		}
	}
	return w.Code, frames
}

func said(frames []map[string]any, key string) string {
	var b strings.Builder
	for _, f := range frames {
		if v, ok := f[key].(string); ok {
			b.WriteString(v)
		}
	}
	return b.String()
}

func exitOf(frames []map[string]any) float64 {
	for _, f := range frames {
		if x, ok := f["x"].(float64); ok {
			return x
		}
	}
	return -1
}

// A DevPod's dp runs the CLI as the DevPod's owner.
func TestHandleDPRunsAsOwner(t *testing.T) {
	s, uids := dpRig(t)
	code, frames := dpCall(t, s, uids["dpa"], wire.DPToken(s.IngestKey, uids["dpa"]), "", "whoami")
	if code != http.StatusOK || !strings.Contains(said(frames, "o"), "dpa") || exitOf(frames) != 0 {
		t.Fatalf("whoami: %d %v", code, frames)
	}
}

// Nobody else's, no forged one, none of a DevPod gone or of another link.
func TestHandleDPRefuses(t *testing.T) {
	s, uids := dpRig(t)
	for name, c := range map[string]struct{ uid, token, cluster string }{
		"wrong token":       {uids["dpa"], "nope", ""},
		"another's token":   {uids["dpa"], wire.DPToken(s.IngestKey, uids["dpb"]), ""},
		"ingest token":      {uids["dpa"], wire.Token(s.IngestKey, uids["dpa"]), ""},
		"unknown DevPod":    {"no-such-uid", wire.DPToken(s.IngestKey, "no-such-uid"), ""},
		"another's cluster": {uids["dpa"], wire.DPToken(s.IngestKey, uids["dpa"]), "st2"},
	} {
		if code, frames := dpCall(t, s, c.uid, c.token, c.cluster, "whoami"); code != http.StatusUnauthorized {
			t.Errorf("%s: %d %v", name, code, frames)
		}
	}
}

// From a DevPod only the queue's, the reservations' and the harmless.
func TestHandleDPOriginCommands(t *testing.T) {
	s, uids := dpRig(t)
	_, frames := dpCall(t, s, uids["dpa"], wire.DPToken(s.IngestKey, uids["dpa"]), "", "delete", "x", "--yes")
	if exitOf(frames) != 2 || !strings.Contains(said(frames, "e"), "not available from inside a DevPod") {
		t.Fatalf("delete: %v", frames)
	}
}
