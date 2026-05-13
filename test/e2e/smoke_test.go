// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// repoRoot resolves the repository root from this source file's location so
// the smoke test invokes hack/e2e-up.sh regardless of go test's cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = <repo>/test/e2e/smoke_test.go -> repo root is two levels up.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func TestSmoke_ApplyDevPodProducesSidecaredPod(t *testing.T) {
	root := repoRoot(t)
	// Bring up the cluster + install chart (idempotent if already up).
	cmd := exec.Command("bash", "hack/e2e-up.sh")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("e2e-up.sh: %v\n%s", err, out)
	}

	scheme := apiruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA placeholder"}},
	}
	_ = c.Create(ctx, user)

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "alice",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "dev",
						Image:   "alpine:3.20",
						Command: []string{"sh", "-c", "sleep infinity"},
					}},
				},
			},
		},
	}
	_ = c.Delete(ctx, dp)
	if err := c.Create(ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), dp) })

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var pod corev1.Pod
		err := c.Get(ctx, client.ObjectKey{Name: "alice-smoke", Namespace: "devpods"}, &pod)
		if err == nil && len(pod.Spec.Containers) == 2 {
			if pod.Spec.Containers[1].Name == "devpod-sshd" {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Pod alice-smoke with sidecar never appeared")
}
