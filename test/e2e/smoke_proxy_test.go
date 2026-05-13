// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

func TestSmoke_SSHProxy_ReachesSidecar(t *testing.T) {
	repo := repoRootForProxy()
	cmd := exec.Command("bash", "hack/e2e-up.sh")
	cmd.Dir = repo
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("e2e-up.sh: %v", err)
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

	keyDir := t.TempDir()
	priv := filepath.Join(keyDir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-q").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pubBytes, err := os.ReadFile(priv + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	pubLine := strings.TrimSpace(string(pubBytes))
	// Upsert User. If a previous run left one with a finalizer (e.g. due to
	// orphan DevPods), update its pubkey list instead of trying to delete
	// and recreate it.
	var existing devpodv1alpha1.User
	if err := c.Get(ctx, client.ObjectKey{Name: "alice"}, &existing); err == nil {
		existing.Spec.Pubkeys = []string{pubLine}
		if err := c.Update(ctx, &existing); err != nil {
			t.Fatalf("update user: %v", err)
		}
	} else {
		user := &devpodv1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{pubLine}},
		}
		if err := c.Create(ctx, user); err != nil {
			t.Fatalf("create user: %v", err)
		}
		t.Cleanup(func() {
			_ = c.Delete(context.Background(), &devpodv1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{Name: "alice"},
			})
		})
	}

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
	// Delete any previous instance; wait for it to actually go away (it may
	// have a finalizer or pending pod) before creating a fresh one.
	_ = c.Delete(ctx, dp)
	waitGone := time.Now().Add(60 * time.Second)
	for time.Now().Before(waitGone) {
		var got devpodv1alpha1.DevPod
		if err := c.Get(ctx, client.ObjectKey{Name: "smoke", Namespace: "devpods"}, &got); err != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := c.Create(ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), &devpodv1alpha1.DevPod{
			ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: "devpods"},
		})
	})

	// Wait for status.endpoint.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if err := c.Get(ctx, client.ObjectKey{Name: "smoke", Namespace: "devpods"}, &got); err == nil && got.Status.Endpoint != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Port-forward.
	pf := exec.Command("kubectl", "-n", "devpod-system", "port-forward", "svc/devpod-gateway", "12222:22")
	pf.Stdout = os.Stderr
	pf.Stderr = os.Stderr
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(2 * time.Second)

	// Try the actual SSH proxy, retrying for a minute (port-forward warm-up,
	// gateway cache sync, etc.).
	deadline = time.Now().Add(2 * time.Minute)
	var out []byte
	for time.Now().Before(deadline) {
		sshCmd := exec.Command("ssh",
			"-i", priv,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=5",
			"-p", "12222",
			"alice+smoke@127.0.0.1",
			"uname", "-a",
		)
		out, err = sshCmd.CombinedOutput()
		if err == nil && strings.Contains(string(out), "Linux") {
			t.Logf("ssh ok: %s", strings.TrimSpace(string(out)))
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("ssh proxy never produced 'Linux' uname output\nlast err: %v\nlast out:\n%s", err, out)
}

// repoRootForProxy returns the repository root from the location of this
// test file (the test runs in test/e2e/, not the repo root).
func repoRootForProxy() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}
