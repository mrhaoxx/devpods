// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrhaoxx/devpod/internal/webui"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

	envOnce sync.Once
	envErr  error
	envStop func()
)

// TestMain stops the envtest apiserver, if a test started it, once every
// test has run.
func TestMain(m *testing.M) {
	code := m.Run()
	if envStop != nil {
		envStop()
	}
	os.Exit(code)
}

// setupSuite gives a test the package's one envtest apiserver + manager
// (started by the first test that asks), empty of what tests make: what
// the test before left is deleted first (resetCluster). The webui
// registers no reconcilers — the manager only provides the cached client
// and informers the handlers use in production.
func setupSuite(t *testing.T) {
	t.Helper()
	envOnce.Do(func() { envErr = startEnv() })
	if envErr != nil {
		t.Fatalf("envtest: %v", envErr)
	}
	resetCluster(t)
}

// startEnv boots the apiserver and the manager, with the devpods
// namespace.
func startEnv() error {
	scheme = runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(devpodv1alpha1.AddToScheme(scheme))

	envTestEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := envTestEnv.Start()
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		_ = envTestEnv.Stop()
		return fmt.Errorf("manager: %w", err)
	}
	if err := webui.RegisterIndexes(context.Background(), k8sManager.GetFieldIndexer()); err != nil {
		_ = envTestEnv.Stop()
		return fmt.Errorf("indexes: %w", err)
	}
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	go func() { _ = k8sManager.Start(mgrCtx) }()
	envStop = func() {
		mgrCancel()
		_ = envTestEnv.Stop()
	}
	if !k8sManager.GetCache().WaitForCacheSync(mgrCtx) {
		return fmt.Errorf("cache sync")
	}
	k8sClient = k8sManager.GetClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns := &corev1.Namespace{}
	ns.Name = "devpods"
	_ = k8sClient.Create(ctx, ns)
	return nil
}

// resetKinds: every kind a test (or a handler it calls) makes — all of
// devpod.io's, and the core ones below.
func resetKinds() []client.ObjectList {
	var out []client.ObjectList
	var names []string
	for gvk := range scheme.AllKnownTypes() {
		if gvk.Group == devpodv1alpha1.GroupVersion.Group && strings.HasSuffix(gvk.Kind, "List") {
			names = append(names, gvk.Kind)
		}
	}
	slices.Sort(names)
	for _, k := range names {
		obj, err := scheme.New(devpodv1alpha1.GroupVersion.WithKind(k))
		if err == nil {
			out = append(out, obj.(client.ObjectList))
		}
	}
	return append(out, &corev1.PodList{}, &corev1.NodeList{}, &corev1.PersistentVolumeClaimList{}, &corev1.PersistentVolumeList{},
		&corev1.SecretList{}, &corev1.ConfigMapList{}, &corev1.EventList{}, &corev1.ServiceList{},
		&discoveryv1.EndpointSliceList{}, &coordinationv1.LeaseList{})
}

// kept: what resetCluster leaves — the apiserver's own.
func kept(o client.Object) bool {
	ns := o.GetNamespace()
	return strings.HasPrefix(ns, "kube-") || (ns == "default" && o.GetName() == "kubernetes")
}

// resetCluster deletes what the tests before left (with no controller
// here, finalizers are taken off and Pods go at once), and waits until the
// cache no longer holds it.
func resetCluster(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	reader := k8sManager.GetAPIReader()
	unfinalize, _ := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": nil}})
	type left struct {
		list client.ObjectList
		uids map[types.UID]bool
	}
	var waits []left
	for _, proto := range resetKinds() {
		l := proto.DeepCopyObject().(client.ObjectList)
		if err := reader.List(ctx, l); err != nil {
			continue // not served here
		}
		items, err := meta.ExtractList(l)
		if err != nil {
			t.Fatalf("reset: %T: %v", l, err)
		}
		uids := map[types.UID]bool{}
		for _, it := range items {
			o := it.(client.Object)
			if kept(o) {
				continue
			}
			if len(o.GetFinalizers()) > 0 {
				_ = k8sClient.Patch(ctx, o, client.RawPatch(types.MergePatchType, unfinalize))
			}
			_ = k8sClient.Delete(ctx, o, client.GracePeriodSeconds(0))
			uids[o.GetUID()] = true
		}
		if len(uids) > 0 {
			waits = append(waits, left{proto, uids})
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for _, w := range waits {
		for {
			l := w.list.DeepCopyObject().(client.ObjectList)
			still := ""
			if err := k8sClient.List(ctx, l); err == nil {
				items, _ := meta.ExtractList(l)
				for _, it := range items {
					if o := it.(client.Object); w.uids[o.GetUID()] {
						still = o.GetNamespace() + "/" + o.GetName()
						break
					}
				}
			} else {
				still = err.Error()
			}
			if still == "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("reset: %T: %s is still there", w.list, still)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
