// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const userFinalizer = "devpod.io/user-cleanup"

// UserReconciler reconciles User objects. Its only job in this milestone is
// to install a finalizer that blocks User deletion while any DevPod still
// references the User, and to count owned DevPods into status.
type UserReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=devpod.io,resources=users,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=devpod.io,resources=users/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=devpod.io,resources=users/finalizers,verbs=update

func (r *UserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("user", req.NamespacedName)

	var u devpodv1alpha1.User
	if err := r.Get(ctx, req.NamespacedName, &u); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get user: %w", err)
	}

	// Count DevPods owned by this user across all namespaces.
	var devpods devpodv1alpha1.DevPodList
	if err := r.List(ctx, &devpods); err != nil {
		return ctrl.Result{}, fmt.Errorf("list devpods: %w", err)
	}
	var ownedCount int32
	for i := range devpods.Items {
		if devpods.Items[i].Spec.Owner == u.Name {
			ownedCount++
		}
	}

	if !u.DeletionTimestamp.IsZero() {
		if ownedCount > 0 {
			log.Info("blocking user deletion: DevPods still exist", "count", ownedCount)
			return ctrl.Result{}, nil
		}
		controllerutil.RemoveFinalizer(&u, userFinalizer)
		if err := r.Update(ctx, &u); err != nil {
			return ctrl.Result{}, fmt.Errorf("clear finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&u, userFinalizer) {
		if err := r.Update(ctx, &u); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if u.Status.DevPodCount != ownedCount {
		u.Status.DevPodCount = ownedCount
		if err := r.Status().Update(ctx, &u); err != nil {
			return ctrl.Result{}, fmt.Errorf("status update: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller with mgr. The DevPod watch
// ensures the User is re-reconciled when a DevPod referencing it is
// added or removed, so the finalizer releases when the last DevPod is
// gone.
func (r *UserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapDevPodToUser := func(_ context.Context, obj client.Object) []reconcile.Request {
		dp, ok := obj.(*devpodv1alpha1.DevPod)
		if !ok || dp.Spec.Owner == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: dp.Spec.Owner}}}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&devpodv1alpha1.User{}).
		Watches(
			&devpodv1alpha1.DevPod{},
			handler.EnqueueRequestsFromMapFunc(mapDevPodToUser),
		).
		Complete(r)
}
