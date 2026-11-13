// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/controllers"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

func telemetryDevPod(t *testing.T, env *testEnv, owner, name string) *devpodv1alpha1.DevPod {
	t.Helper()
	_ = env.Client.Create(env.Ctx, &devpodv1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: owner}, Spec: devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA " + owner}}})
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{Owner: owner, Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}}}},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}
	return dp
}

// waitSecret polls until cond holds for the DevPod's host-key Secret.
func waitSecret(t *testing.T, env *testEnv, name string, cond func(*corev1.Secret) bool) *corev1.Secret {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var sec corev1.Secret
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: name + "-hostkey", Namespace: "devpods"}, &sec); err == nil && cond(&sec) {
			return &sec
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("secret %s-hostkey never satisfied the condition", name)
	return nil
}

func TestDevPodReconciler_TelemetryKeysReconciled(t *testing.T) {
	settings := &render.TelemetrySettings{IngestURL: "http://ingest/ingest/v1/batch", Key: []byte("test-key"), Defaults: config.DefaultConfig()}
	setupSuiteWith(t, func(r *controllers.DevPodReconciler) { r.Telemetry = settings })
	env := newTestEnv(t)
	dp := telemetryDevPod(t, env, "tel", "tel-dev")
	sec := waitSecret(t, env, "tel-dev", func(s *corev1.Secret) bool { return len(s.Data["ingest-token"]) > 0 })
	var live devpodv1alpha1.DevPod
	_ = env.Client.Get(env.Ctx, types.NamespacedName{Name: dp.Name, Namespace: "devpods"}, &live)
	if string(sec.Data["ingest-token"]) != wire.Token(settings.Key, string(live.UID)) || len(sec.Data["ssh_host_ed25519_key"]) == 0 {
		t.Fatal("token must be HMAC(key, uid) and the SSH key must stay")
	}
	var pod corev1.Pod
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && env.Client.Get(env.Ctx, types.NamespacedName{Name: "tel-dev", Namespace: "devpods"}, &pod) != nil {
		time.Sleep(250 * time.Millisecond)
	}
	if pod.UID == "" {
		t.Fatal("pod never created")
	}
	// A stale telemetry.json (e.g. from an older controller) is corrected
	// on the next reconcile, without recreating the Pod.
	sec.Data["telemetry.json"] = []byte(`{"topN":1}`)
	if err := env.Client.Update(env.Ctx, sec); err != nil {
		t.Fatal(err)
	}
	live.Annotations = map[string]string{"test/poke": "1"}
	_ = env.Client.Update(env.Ctx, &live)
	waitSecret(t, env, "tel-dev", func(s *corev1.Secret) bool {
		var c config.Config
		return json.Unmarshal(s.Data["telemetry.json"], &c) == nil && c.TopN == 20 && c.DevPodUID == string(live.UID)
	})
	var after corev1.Pod
	if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "tel-dev", Namespace: "devpods"}, &after); err != nil || after.UID != pod.UID {
		t.Fatalf("telemetry config must not recreate the Pod: %v %s→%s", err, pod.UID, after.UID)
	}
}

func TestDevPodReconciler_TelemetryOffLeavesSecret(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)
	telemetryDevPod(t, env, "notel", "notel-dev")
	sec := waitSecret(t, env, "notel-dev", func(s *corev1.Secret) bool { return len(s.Data["ssh_host_ed25519_key"]) > 0 })
	if _, ok := sec.Data["ingest-token"]; ok {
		t.Fatal("telemetry off: no ingest-token key")
	}
	if _, ok := sec.Data["telemetry.json"]; ok {
		t.Fatal("telemetry off: no telemetry.json key")
	}
}
