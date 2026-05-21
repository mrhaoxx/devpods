// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/controllers"
)

var (
	envTestEnv *envtest.Environment
	k8sClient  client.Client
	scheme     *runtime.Scheme
)

// testEnv encapsulates the envtest lifecycle exposed to per-test files
// in the package.
type testEnv struct {
	Client client.Client
	Ctx    context.Context
	Cancel context.CancelFunc
	Ns     *corev1.Namespace
}

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

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// setupSuite is called per-test, so the controller name
		// would otherwise collide across tests in the same process.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	if err := (&controllers.DevPodReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		GwConfig:           defaultGwConfig(),
		GatewayNamespace:   "devpod-system",
		GatewayInternalPub: []byte("ssh-ed25519 AAAA test gateway-internal\n"),
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("setup DevPodReconciler: %v", err)
	}
	if err := (&controllers.UserReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("setup UserReconciler: %v", err)
	}
	if err := (&controllers.DevPodSnapshotReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		SnapshotImage: "docker:cli",
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("setup DevPodSnapshotReconciler: %v", err)
	}

	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	go func() {
		_ = mgr.Start(mgrCtx)
	}()
	k8sClient = mgr.GetClient()
	t.Cleanup(func() {
		mgrCancel()
		_ = envTestEnv.Stop()
	})
}

func defaultGwConfig() *devpodv1alpha1.GatewayConfig {
	return &devpodv1alpha1.GatewayConfig{
		Spec: devpodv1alpha1.GatewayConfigSpec{
			DevPodNamespace: "devpods",
			SupervisorImage:    "ghcr.io/example/devpod-sshd:test",
			HostKeyRef:      devpodv1alpha1.SecretRef{Name: "gw-host", Namespace: "devpod-system"},
			InternalKeyRef:  devpodv1alpha1.SecretRef{Name: "gw-internal", Namespace: "devpod-system"},
		},
	}
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	ns := &corev1.Namespace{}
	ns.Name = "devpods"
	_ = k8sClient.Create(ctx, ns)

	return &testEnv{Client: k8sClient, Ctx: ctx, Cancel: cancel, Ns: ns}
}
