// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

// DevPodSnapshotReconciler reconciles DevPodSnapshot objects.
type DevPodSnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=devpod.io,resources=devpodsnapshots,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=devpod.io,resources=devpodsnapshots/status,verbs=patch
// +kubebuilder:rbac:groups=devpod.io,resources=devpods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;delete

func (r *DevPodSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("snapshot", req.NamespacedName)
	_ = log

	var snap devpodv1alpha1.DevPodSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get snapshot: %w", err)
	}

	if snap.Status.Phase == devpodv1alpha1.SnapshotSucceeded ||
		snap.Status.Phase == devpodv1alpha1.SnapshotFailed {
		return ctrl.Result{}, nil
	}

	if snap.Status.JobRef != nil {
		return r.reconcileExistingJob(ctx, &snap)
	}

	var dp devpodv1alpha1.DevPod
	dpKey := types.NamespacedName{Name: snap.Spec.DevPodName, Namespace: snap.Namespace}
	if err := r.Get(ctx, dpKey, &dp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setFailed(ctx, &snap, "DevPod %q not found", snap.Spec.DevPodName)
		}
		return ctrl.Result{}, fmt.Errorf("get devpod: %w", err)
	}
	if dp.Status.Phase != devpodv1alpha1.DevPodRunning {
		return ctrl.Result{}, r.setFailed(ctx, &snap, "DevPod %q is not running (phase: %s)", snap.Spec.DevPodName, dp.Status.Phase)
	}

	var pod corev1.Pod
	podKey := types.NamespacedName{Name: render.PodName(&dp), Namespace: snap.Namespace}
	if err := r.Get(ctx, podKey, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setFailed(ctx, &snap, "Pod %q not found", podKey.Name)
		}
		return ctrl.Result{}, fmt.Errorf("get pod: %w", err)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return ctrl.Result{}, r.setFailed(ctx, &snap, "Pod %q is not running (phase: %s)", pod.Name, pod.Status.Phase)
	}

	containerID, err := extractContainerID(pod.Status.ContainerStatuses, pod.Spec.Containers[0].Name)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &snap, "container ID: %v", err)
	}

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return ctrl.Result{}, r.setFailed(ctx, &snap, "Pod %q has no nodeName", pod.Name)
	}

	var pushSecret *string
	if snap.Spec.PushSecretRef != nil {
		pushSecret = &snap.Spec.PushSecretRef.Name
	}
	job := render.SnapshotJob(&snap, containerID, nodeName, pushSecret)

	if err := controllerutil.SetControllerReference(&snap, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set ownerref on job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("create snapshot job: %w", err)
	}

	return ctrl.Result{}, r.setRunning(ctx, &snap, job.Name)
}

func (r *DevPodSnapshotReconciler) reconcileExistingJob(ctx context.Context, snap *devpodv1alpha1.DevPodSnapshot) (ctrl.Result, error) {
	var job batchv1.Job
	jobKey := types.NamespacedName{Name: snap.Status.JobRef.Name, Namespace: snap.Namespace}
	if err := r.Get(ctx, jobKey, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setFailed(ctx, snap, "snapshot Job %q disappeared", jobKey.Name)
		}
		return ctrl.Result{}, fmt.Errorf("get snapshot job: %w", err)
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			digest := r.extractDigest(ctx, &job)
			return ctrl.Result{}, r.setSucceeded(ctx, snap, digest)
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return ctrl.Result{}, r.setFailed(ctx, snap, "snapshot Job failed: %s", c.Message)
		}
	}

	return ctrl.Result{}, nil
}

func (r *DevPodSnapshotReconciler) extractDigest(ctx context.Context, job *batchv1.Job) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil || len(pods.Items) == 0 {
		return ""
	}
	for _, cs := range pods.Items[0].Status.ContainerStatuses {
		if cs.Name == "snapshot" && cs.State.Terminated != nil {
			return strings.TrimSpace(cs.State.Terminated.Message)
		}
	}
	return ""
}

func extractContainerID(statuses []corev1.ContainerStatus, name string) (string, error) {
	for _, cs := range statuses {
		if cs.Name == name {
			id := cs.ContainerID
			if id == "" {
				return "", fmt.Errorf("container %q has no containerID", name)
			}
			if idx := strings.Index(id, "://"); idx >= 0 {
				id = id[idx+3:]
			}
			return id, nil
		}
	}
	return "", fmt.Errorf("container %q not found in pod status", name)
}

func (r *DevPodSnapshotReconciler) setFailed(ctx context.Context, snap *devpodv1alpha1.DevPodSnapshot, format string, args ...any) error {
	patch := client.MergeFrom(snap.DeepCopy())
	snap.Status.Phase = devpodv1alpha1.SnapshotFailed
	snap.Status.Message = fmt.Sprintf(format, args...)
	apimeta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type:               "Complete",
		Status:             metav1.ConditionFalse,
		Reason:             "Failed",
		Message:            snap.Status.Message,
		LastTransitionTime: metav1.Now(),
	})
	return r.Status().Patch(ctx, snap, patch)
}

func (r *DevPodSnapshotReconciler) setRunning(ctx context.Context, snap *devpodv1alpha1.DevPodSnapshot, jobName string) error {
	patch := client.MergeFrom(snap.DeepCopy())
	snap.Status.Phase = devpodv1alpha1.SnapshotRunning
	snap.Status.JobRef = &devpodv1alpha1.JobRef{Name: jobName}
	return r.Status().Patch(ctx, snap, patch)
}

func (r *DevPodSnapshotReconciler) setSucceeded(ctx context.Context, snap *devpodv1alpha1.DevPodSnapshot, digest string) error {
	patch := client.MergeFrom(snap.DeepCopy())
	snap.Status.Phase = devpodv1alpha1.SnapshotSucceeded
	snap.Status.Digest = digest
	apimeta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type:               "Complete",
		Status:             metav1.ConditionTrue,
		Reason:             "Succeeded",
		LastTransitionTime: metav1.Now(),
	})
	return r.Status().Patch(ctx, snap, patch)
}

// SetupWithManager registers the controller with mgr.
func (r *DevPodSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&devpodv1alpha1.DevPodSnapshot{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
