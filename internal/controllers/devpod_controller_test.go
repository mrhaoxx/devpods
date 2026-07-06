// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

func TestDevPodReconciler_CreatesPodWithBootstrap(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec: devpodv1alpha1.UserSpec{
			Pubkeys: []string{"ssh-ed25519 AAAA placeholder alice@laptop"},
		},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-dev", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "alice",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "dev",
						Image:   "ghcr.io/example/devbox:latest",
						Command: []string{"sleep", "infinity"},
					}},
				},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got corev1.Pod
		err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "frontend-dev", Namespace: "devpods"}, &got)
		if err == nil &&
			len(got.Spec.InitContainers) == 1 &&
			got.Spec.InitContainers[0].Name == "devpod-bootstrap" &&
			len(got.Spec.Containers) == 1 &&
			len(got.Spec.Containers[0].Command) == 1 &&
			got.Spec.Containers[0].Command[0] == "/opt/devpod/devpod-supervisor" {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Pod with bootstrap init + supervisor wrap never appeared")
}

func TestDevPodReconciler_CreatesHostKeySecret(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "bob"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA bob"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "backend-dev", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "bob",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var sec corev1.Secret
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "backend-dev-hostkey", Namespace: "devpods"}, &sec); err == nil {
			if len(sec.Data["ssh_host_ed25519_key"]) > 0 {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("host-key Secret never appeared")
}

func TestDevPodReconciler_WritesStatusEndpointAndPhase(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "diana"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA diana"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "stat", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "diana",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	// Wait for the rendered Pod to exist, then patch its status to Running with an IP.
	deadline := time.Now().Add(30 * time.Second)
	var pod corev1.Pod
	for time.Now().Before(deadline) {
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "diana-stat", Namespace: "devpods"}, &pod); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pod.Name == "" {
		t.Fatalf("pod never appeared")
	}

	patch := client.MergeFrom(pod.DeepCopy())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.1.2.3"
	if err := env.Client.Status().Patch(env.Ctx, &pod, patch); err != nil {
		t.Fatalf("patch pod status: %v", err)
	}

	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if err := env.Client.Get(env.Ctx, types.NamespacedName{Name: "stat", Namespace: "devpods"}, &got); err == nil {
			if got.Status.Phase == devpodv1alpha1.DevPodRunning && got.Status.Endpoint == "10.1.2.3:2222" {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("DevPod status.endpoint/phase never reached Running 10.1.2.3:2222")
}

func TestDevPodReconciler_PersistenceCreatesPVC(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA test"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "persisted", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "alice",
			Running: true,
			Persistence: &devpodv1alpha1.PersistenceSpec{
				Size:      resource.MustParse("1Gi"),
				MountPath: "/workspace",
			},
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "debian:stable"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	pvcKey := types.NamespacedName{Name: "persisted-home", Namespace: "devpods"}
	deadline := time.Now().Add(10 * time.Second)
	var pvc corev1.PersistentVolumeClaim
	for time.Now().Before(deadline) {
		if err := env.Client.Get(env.Ctx, pvcKey, &pvc); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pvc.Name == "" {
		t.Fatalf("PVC %s never created", pvcKey)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "1Gi" {
		t.Errorf("PVC size = %s, want 1Gi", got.String())
	}
}

func TestDevPodReconciler_PersistencePopulatesStatusPVCRef(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "frank"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA test"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "statpvc", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "frank",
			Running: true,
			Persistence: &devpodv1alpha1.PersistenceSpec{
				Size:      resource.MustParse("1Gi"),
				MountPath: "/workspace",
			},
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	dpKey := types.NamespacedName{Name: "statpvc", Namespace: "devpods"}
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if err := env.Client.Get(env.Ctx, dpKey, &got); err == nil {
			if got.Status.PersistentVolumeClaimRef != nil &&
				got.Status.PersistentVolumeClaimRef.Name == "frank-statpvc-home" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("status.persistentVolumeClaimRef never populated")
}

func TestDevPodReconciler_HibernateDeletesPod(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "grace"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA test"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "hibtest", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "grace",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	podKey := types.NamespacedName{Name: "grace-hibtest", Namespace: "devpods"}
	deadline := time.Now().Add(10 * time.Second)
	var pod corev1.Pod
	for time.Now().Before(deadline) {
		if err := env.Client.Get(env.Ctx, podKey, &pod); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pod.Name == "" {
		t.Fatalf("Pod %s never appeared before hibernate", podKey)
	}

	// Flip to running=false. Use a fresh Get for current ResourceVersion.
	dpKey := types.NamespacedName{Name: "hibtest", Namespace: "devpods"}
	var fresh devpodv1alpha1.DevPod
	if err := env.Client.Get(env.Ctx, dpKey, &fresh); err != nil {
		t.Fatalf("get dp before hibernate: %v", err)
	}
	fresh.Spec.Running = false
	if err := env.Client.Update(env.Ctx, &fresh); err != nil {
		t.Fatalf("patch running=false: %v", err)
	}

	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var p corev1.Pod
		err := env.Client.Get(env.Ctx, podKey, &p)
		if apierrors.IsNotFound(err) {
			return
		}
		// Pod may also be in Terminating state with a deletionTimestamp.
		if err == nil && p.DeletionTimestamp != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("Pod was not deleted after spec.running=false")
}

func TestDevPodReconciler_HibernateStatusFields(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "henry"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA test"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "hibstat", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "henry",
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	dpKey := types.NamespacedName{Name: "hibstat", Namespace: "devpods"}
	podKey := types.NamespacedName{Name: "henry-hibstat", Namespace: "devpods"}

	// Wait for Pod to appear (controller has acted).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var p corev1.Pod
		if err := env.Client.Get(env.Ctx, podKey, &p); err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Flip to Running=false.
	var fresh devpodv1alpha1.DevPod
	if err := env.Client.Get(env.Ctx, dpKey, &fresh); err != nil {
		t.Fatalf("get dp: %v", err)
	}
	fresh.Spec.Running = false
	if err := env.Client.Update(env.Ctx, &fresh); err != nil {
		t.Fatalf("update Running=false: %v", err)
	}

	// Verify Stopped status fields.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if err := env.Client.Get(env.Ctx, dpKey, &got); err == nil {
			if got.Status.Phase == devpodv1alpha1.DevPodStopped &&
				got.Status.Endpoint == "" &&
				got.Status.WorkloadRef == nil &&
				got.Status.HibernatedAt != nil {
				goto resumeStep
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("status fields wrong after hibernate")

resumeStep:
	// Flip back to Running=true.
	if err := env.Client.Get(env.Ctx, dpKey, &fresh); err != nil {
		t.Fatalf("get dp pre-resume: %v", err)
	}
	fresh.Spec.Running = true
	if err := env.Client.Update(env.Ctx, &fresh); err != nil {
		t.Fatalf("update Running=true: %v", err)
	}

	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if err := env.Client.Get(env.Ctx, dpKey, &got); err == nil {
			if got.Status.HibernatedAt == nil && got.Status.Phase != devpodv1alpha1.DevPodStopped {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("HibernatedAt not cleared and/or phase not transitioned after resume")
}

func TestDevPodReconciler_DeleteDetachesPVC(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "ivy"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA test"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "detach", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "ivy",
			Running: true,
			Persistence: &devpodv1alpha1.PersistenceSpec{
				Size:      resource.MustParse("1Gi"),
				MountPath: "/workspace",
			},
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create dp: %v", err)
	}

	pvcKey := types.NamespacedName{Name: "ivy-detach-home", Namespace: "devpods"}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var pvc corev1.PersistentVolumeClaim
		if err := env.Client.Get(env.Ctx, pvcKey, &pvc); err == nil {
			if len(pvc.OwnerReferences) > 0 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Delete the DevPod.
	if err := env.Client.Delete(env.Ctx, dp); err != nil {
		t.Fatalf("delete dp: %v", err)
	}

	// Wait for DevPod to vanish.
	dpKey := types.NamespacedName{Name: "detach", Namespace: "devpods"}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		err := env.Client.Get(env.Ctx, dpKey, &got)
		if apierrors.IsNotFound(err) {
			goto checkPVC
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("DevPod was not removed within 15s")

checkPVC:
	// PVC must still exist; ownerRefs must not include this DevPod.
	var pvc corev1.PersistentVolumeClaim
	if err := env.Client.Get(env.Ctx, pvcKey, &pvc); err != nil {
		t.Fatalf("PVC vanished after DevPod delete: %v", err)
	}
	for _, or := range pvc.OwnerReferences {
		if or.Kind == "DevPod" {
			t.Errorf("PVC still has DevPod ownerRef: %+v", or)
		}
	}
}

func TestReconcile_OwnerWithoutUserCRD_StillMaterializesPod(t *testing.T) {
	// Spec §1: LDAP-only users don't have a User CRD. The reconciler
	// must accept owner as an opaque string and materialize the Pod
	// anyway — the gateway proves identity at auth time.
	setupSuite(t)
	env := newTestEnv(t)

	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "ldap-only", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:   "lonely", // no User CRD exists
			Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	if err := env.Client.Create(env.Ctx, dp); err != nil {
		t.Fatalf("create devpod: %v", err)
	}

	podKey := types.NamespacedName{Name: "lonely-ldap-only", Namespace: "devpods"}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var pod corev1.Pod
		if err := env.Client.Get(env.Ctx, podKey, &pod); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("Pod was not rendered for LDAP-only owner without User CRD")
}

func TestReconcile_OwnerAllowNetPolCleanedOnLastDevPod(t *testing.T) {
	setupSuite(t)
	env := newTestEnv(t)

	user := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "jules"},
		Spec:       devpodv1alpha1.UserSpec{Pubkeys: []string{"ssh-ed25519 AAAA test"}},
	}
	if err := env.Client.Create(env.Ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	dp1 := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "devpods"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner: "jules", Running: true,
			Pod: &devpodv1alpha1.PodWorkloadSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "x"}}},
			},
		},
	}
	dp2 := dp1.DeepCopy()
	dp2.Name = "two"
	if err := env.Client.Create(env.Ctx, dp1); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Create(env.Ctx, dp2); err != nil {
		t.Fatal(err)
	}

	policyKey := types.NamespacedName{Name: "devpod-allow-jules", Namespace: "devpods"}

	// Wait for the policy to exist.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var p networkingv1.NetworkPolicy
		if env.Client.Get(env.Ctx, policyKey, &p) == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	var p networkingv1.NetworkPolicy
	if err := env.Client.Get(env.Ctx, policyKey, &p); err != nil {
		t.Fatalf("policy never created: %v", err)
	}

	// Delete dp1; policy must remain (dp2 still alive).
	if err := env.Client.Delete(env.Ctx, dp1); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got devpodv1alpha1.DevPod
		if apierrors.IsNotFound(env.Client.Get(env.Ctx, types.NamespacedName{Name: "one", Namespace: "devpods"}, &got)) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := env.Client.Get(env.Ctx, policyKey, &p); err != nil {
		t.Fatalf("policy gone after deleting only one of two DevPods: %v", err)
	}

	// Delete dp2; policy must vanish.
	if err := env.Client.Delete(env.Ctx, dp2); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got networkingv1.NetworkPolicy
		if apierrors.IsNotFound(env.Client.Get(env.Ctx, policyKey, &got)) {
			return // pass
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("owner-allow policy never deleted after the last DevPod went away")
}
