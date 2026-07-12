// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

var (
	envTestEnv *envtest.Environment
	k8sClient  client.Client
	k8sManager manager.Manager
	scheme     *runtime.Scheme
)

// setupSuite boots one envtest apiserver + manager per test. The
// webui registers no reconcilers — the manager only provides the
// cached client and informers the handlers use in production.
func setupSuite(t *testing.T) {
	t.Helper()

	scheme = runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))

	envTestEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := envTestEnv.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}

	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	go func() { _ = k8sManager.Start(mgrCtx) }()
	if !k8sManager.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatal("cache sync")
	}
	k8sClient = k8sManager.GetClient()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns := &corev1.Namespace{}
	ns.Name = "devpods"
	_ = k8sClient.Create(ctx, ns)

	t.Cleanup(func() {
		mgrCancel()
		_ = envTestEnv.Stop()
	})
}
