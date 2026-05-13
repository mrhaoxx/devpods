// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

func TestHomePVC_NilPersistenceReturnsNil(t *testing.T) {
	dp := minimalDevPod()
	pvc, err := render.HomePVC(dp, cfg())
	if err != nil {
		t.Fatalf("HomePVC: %v", err)
	}
	if pvc != nil {
		t.Errorf("expected nil when persistence is nil, got %v", pvc)
	}
}

func TestHomePVC_DefaultsToRWO(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:      resource.MustParse("10Gi"),
		MountPath: "/workspace",
	}

	pvc, err := render.HomePVC(dp, cfg())
	if err != nil {
		t.Fatalf("HomePVC: %v", err)
	}
	if pvc == nil {
		t.Fatal("expected non-nil PVC")
	}
	if pvc.Name != "alice-frontend-dev-home" {
		t.Errorf("PVC name = %q, want alice-frontend-dev-home", pvc.Name)
	}
	if got := pvc.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteOnce {
		t.Errorf("access modes = %v, want [ReadWriteOnce]", got)
	}
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if want := resource.MustParse("10Gi"); q.Cmp(want) != 0 {
		t.Errorf("size = %v, want 10Gi", q)
	}
}

func TestHomePVC_EmptyStorageClassMeansClusterDefault(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:      resource.MustParse("1Gi"),
		MountPath: "/workspace",
	}
	pvc, err := render.HomePVC(dp, cfg())
	if err != nil {
		t.Fatalf("HomePVC: %v", err)
	}
	if pvc == nil {
		t.Fatal("expected non-nil PVC")
	}
	if pvc.Spec.StorageClassName != nil {
		t.Errorf("StorageClassName = %v, want nil (cluster default)", *pvc.Spec.StorageClassName)
	}
}

func TestHomePVC_ExplicitStorageClass(t *testing.T) {
	dp := minimalDevPod()
	dp.Spec.Persistence = &devpodv1alpha1.PersistenceSpec{
		Size:             resource.MustParse("1Gi"),
		StorageClassName: "fast-ssd",
		MountPath:        "/workspace",
	}
	pvc, err := render.HomePVC(dp, cfg())
	if err != nil {
		t.Fatalf("HomePVC: %v", err)
	}
	if pvc == nil {
		t.Fatal("expected non-nil PVC")
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-ssd" {
		t.Errorf("StorageClassName = %v, want fast-ssd", pvc.Spec.StorageClassName)
	}
}
