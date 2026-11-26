// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

func cpuSpec(gpus int) *corev1.PodSpec {
	l := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	if gpus > 0 {
		l[gpures.NVIDIA] = *resource.NewQuantity(int64(gpus), resource.DecimalSI)
	}
	return &corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x", Resources: corev1.ResourceRequirements{Limits: l, Requests: l}}}}
}

// Jobs run only on reservable machines; a DevPod on any its owner may
// use and its own spec fits.
func TestViewJobsOnlyReservable(t *testing.T) {
	p := newPlaceRig(t, "vj")
	ctx := context.Background()
	if p.s.QueueAllowedForTest(ctx, "vj-alice", false, cpuSpec(0), p.n2) {
		t.Fatal("a job on a machine not reservable")
	}
	if !p.s.QueueAllowedForTest(ctx, "vj-alice", false, cpuSpec(1), p.n1) {
		t.Fatal("a job not on the reservable machine")
	}
}

func TestViewDevPodAnyAllowedMachine(t *testing.T) {
	p := newPlaceRig(t, "va")
	ctx := context.Background()
	if !p.s.QueueAllowedForTest(ctx, "va-alice", true, cpuSpec(0), p.n2) {
		t.Fatal("a DevPod kept off a machine not reservable")
	}
	if p.s.QueueAllowedForTest(ctx, "va-alice", true, cpuSpec(1), p.n2) {
		t.Fatal("a DevPod asking a card given a machine with none")
	}
}

// A machine one may use only under a reservation is never the queue's to
// give a DevPod.
func TestViewDevPodNoReservationMachines(t *testing.T) {
	p := newPlaceRig(t, "vr")
	setRules(t, onNode(p.n1, needsReservation), everyoneAllowed)
	if p.s.QueueAllowedForTest(context.Background(), "vr-alice", true, cpuSpec(1), p.n1) {
		t.Fatal("a DevPod given a machine that needs a reservation")
	}
}

// A home on a machine's own disk (a local PV) keeps its DevPod there.
func TestHomeNodeFromLocalPV(t *testing.T) {
	p := newPlaceRig(t, "vh")
	ctx := context.Background()
	dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "vh-alice-h", Namespace: "devpods"}, Spec: devpodv1alpha1.DevPodSpec{Owner: "vh-alice",
		Persistence: &devpodv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi")}, Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: *cpuSpec(0)}}}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "vh-pv"}, Spec: corev1.PersistentVolumeSpec{
		Capacity:               corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		PersistentVolumeSource: corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/data"}},
		NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{p.n1}}}}}}}}}
	if err := k8sClient.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pv) })
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: p.s.HomePVCNameForTest(dp), Namespace: "devpods"}, Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, VolumeName: "vh-pv",
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	if err := k8sClient.Create(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pvc) })
	waitFor(t, func() bool { return p.s.HomeNodeForTest(ctx, dp) == p.n1 })
	plain := dp.DeepCopy()
	plain.Spec.Persistence = nil
	if n := p.s.HomeNodeForTest(ctx, plain); n != "" {
		t.Fatalf("no home volume: %q", n)
	}
}
