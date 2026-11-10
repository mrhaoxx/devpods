// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

// reservationFinalizer is added once the controller has touched DevPods
// for a reservation, so that deleting it still runs the end-of-reservation
// steps. A reservation deleted before that (the loser of a conflict)
// goes away at once.
const reservationFinalizer = "devpod.io/reservation"

// ReservationReconciler walks each Reservation along its timeline
// (reservation spec §5): the clearing notice, clearing other users'
// DevPods at the start, the ending notices, hibernating the holder's
// DevPods at the end, and deleting it after the retention.
//
// Every step re-derives what to do from the current time and the state
// of the objects, so a reconcile can be repeated or resumed after a
// restart: a DevPod already hibernated, a condition already set and a
// phase already written are left alone.
type ReservationReconciler struct {
	client.Client
	Recorder record.EventRecorder // optional

	Namespace     string // where DevPods live
	Now           func() time.Time
	ClearNotice   time.Duration
	EndingNotices []time.Duration
	Retention     time.Duration
}

// +kubebuilder:rbac:groups=devpod.io,resources=reservations,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=devpod.io,resources=reservations/status,verbs=get;update;patch

func (r *ReservationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ReservationReconciler) event(dp *devpodv1alpha1.DevPod, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(dp, corev1.EventTypeNormal, reason, msg)
	}
}

// nodeDevPod is a running DevPod that is on, or asks for, a node.
type nodeDevPod struct {
	dp     *devpodv1alpha1.DevPod
	onNode bool // its Pod is scheduled there
}

// devPodsOf lists the running DevPods whose Pod is on node, plus those
// pinned to it that have no Pod there yet.
func (r *ReservationReconciler) devPodsOf(ctx context.Context, node string) ([]nodeDevPod, error) {
	var list devpodv1alpha1.DevPodList
	if err := r.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
		return nil, fmt.Errorf("list devpods: %w", err)
	}
	var out []nodeDevPod
	for i := range list.Items {
		dp := &list.Items[i]
		if !dp.Spec.Running || !dp.DeletionTimestamp.IsZero() {
			continue
		}
		var pod corev1.Pod
		err := r.Get(ctx, types.NamespacedName{Name: render.PodName(dp), Namespace: r.Namespace}, &pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get pod of %s: %w", dp.Name, err)
		}
		onNode := err == nil && pod.Spec.NodeName == node
		if onNode || (PinnedNode(dp) == node && (err != nil || pod.Spec.NodeName == "")) {
			out = append(out, nodeDevPod{dp: dp, onNode: onNode})
		}
	}
	return out, nil
}

// hibernate stops a DevPod on behalf of the reservation.
func (r *ReservationReconciler) hibernate(ctx context.Context, dp *devpodv1alpha1.DevPod, res *devpodv1alpha1.Reservation) error {
	before := dp.DeepCopy()
	dp.Spec.Running = false
	if dp.Annotations == nil {
		dp.Annotations = map[string]string{}
	}
	dp.Annotations[devpodv1alpha1.HibernatedByAnnotation] = "reservation/" + res.Name
	return r.Patch(ctx, dp, client.MergeFrom(before))
}

// successor reports whether the same user holds the node right after res
// (an extension): then the end of res is not the end of their time.
func successor(res *devpodv1alpha1.Reservation, all []devpodv1alpha1.Reservation) bool {
	for i := range all {
		o := &all[i]
		if o.Name != res.Name && o.Spec.Node == res.Spec.Node && o.Spec.User == res.Spec.User && o.Spec.Cancelled == nil &&
			o.DeletionTimestamp.IsZero() && o.Spec.Start.Time.Equal(res.Spec.End.Time) {
			return true
		}
	}
	return false
}

func noPersistenceWarning(dp *devpodv1alpha1.DevPod) string {
	if dp.Spec.Persistence == nil {
		return "；休眠后容器内数据丢失"
	}
	return ""
}

// Reconcile moves one Reservation to where the clock says it is.
func (r *ReservationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var res devpodv1alpha1.Reservation
	if err := r.Get(ctx, req.NamespacedName, &res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var all devpodv1alpha1.ReservationList
	if err := r.List(ctx, &all); err != nil {
		return ctrl.Result{}, fmt.Errorf("list reservations: %w", err)
	}
	now := r.now()
	deleting := !res.DeletionTimestamp.IsZero()
	if deleting || res.Spec.Cancelled != nil || !now.Before(res.Spec.End.Time) {
		return r.finish(ctx, &res, all.Items, now)
	}
	// the loser of a conflict is about to be deleted by the webui: hands off
	if loses(&res, all.Items) {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	wake := nextWake(res.Spec.Start.Time, res.Spec.End.Time, now, r.ClearNotice, r.EndingNotices)
	after := wake.Sub(now)

	if now.Before(res.Spec.Start.Time) {
		if !now.Before(res.Spec.Start.Time.Add(-r.ClearNotice)) {
			if err := r.notifyClearing(ctx, &res); err != nil {
				return ctrl.Result{}, err
			}
			after = min(after, time.Minute) // DevPods may still land on the node
		}
		return ctrl.Result{RequeueAfter: after}, r.setStatus(ctx, &res, func(s *devpodv1alpha1.ReservationStatus) {
			s.Phase = devpodv1alpha1.ReservationUpcoming
		})
	}
	if err := r.activate(ctx, &res, all.Items, now); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: min(after, 5*time.Minute)}, nil
}

// setStatus applies fn and writes the status if it changed.
func (r *ReservationReconciler) setStatus(ctx context.Context, res *devpodv1alpha1.Reservation, fn func(*devpodv1alpha1.ReservationStatus)) error {
	before := res.DeepCopy()
	fn(&res.Status)
	if equalStatus(before.Status, res.Status) {
		return nil
	}
	return r.Status().Patch(ctx, res, client.MergeFrom(before))
}

func equalStatus(a, b devpodv1alpha1.ReservationStatus) bool {
	return a.Phase == b.Phase && timePtrEqual(a.NotifiedClearAt, b.NotifiedClearAt) &&
		slices.Equal(a.ClearedDevPods, b.ClearedDevPods) && slices.Equal(a.HibernatedAtEnd, b.HibernatedAtEnd)
}

func (r *ReservationReconciler) ensureFinalizer(ctx context.Context, res *devpodv1alpha1.Reservation) error {
	if !controllerutil.AddFinalizer(res, reservationFinalizer) {
		return nil
	}
	return r.Update(ctx, res)
}

// notifyClearing marks other users' DevPods on the node: they will be
// hibernated when the reservation starts.
func (r *ReservationReconciler) notifyClearing(ctx context.Context, res *devpodv1alpha1.Reservation) error {
	if err := r.ensureFinalizer(ctx, res); err != nil {
		return err
	}
	dps, err := r.devPodsOf(ctx, res.Spec.Node)
	if err != nil {
		return err
	}
	for _, d := range dps {
		if d.dp.Spec.Owner == res.Spec.User || !d.onNode {
			continue
		}
		msg := fmt.Sprintf("%s 将于 %s 被 %s 预约，届时自动休眠%s", res.Spec.Node, clock(res.Spec.Start.Time), res.Spec.User, noPersistenceWarning(d.dp))
		changed, err := setDevPodCondition(ctx, r.Client, d.dp, devpodv1alpha1.ConditionReservationClearing, true, msg)
		if err != nil {
			return err
		}
		if changed {
			r.event(d.dp, devpodv1alpha1.ConditionReservationClearing, msg)
		}
	}
	return r.setStatus(ctx, res, func(s *devpodv1alpha1.ReservationStatus) {
		if s.NotifiedClearAt == nil {
			t := metav1.NewTime(r.now())
			s.NotifiedClearAt = &t
		}
	})
}

// activate runs while the reservation holds the node: other users'
// DevPods on it are hibernated (at the start, and any that slipped in
// since), and the holder's DevPods get the ending notice when due.
func (r *ReservationReconciler) activate(ctx context.Context, res *devpodv1alpha1.Reservation, all []devpodv1alpha1.Reservation, now time.Time) error {
	if err := r.ensureFinalizer(ctx, res); err != nil {
		return err
	}
	dps, err := r.devPodsOf(ctx, res.Spec.Node)
	if err != nil {
		return err
	}
	cleared := append([]string(nil), res.Status.ClearedDevPods...)
	ending := !successor(res, all)
	if ending {
		ending = false
		for _, d := range r.EndingNotices {
			ending = ending || !now.Before(res.Spec.End.Time.Add(-d))
		}
	}
	for _, d := range dps {
		if d.dp.Spec.Owner != res.Spec.User {
			if !d.onNode {
				continue // pinned but without a Pod: the DevPod reconciler holds it back (NodeReserved)
			}
			if err := r.hibernate(ctx, d.dp, res); err != nil {
				return fmt.Errorf("clear %s: %w", d.dp.Name, err)
			}
			if _, err := setDevPodCondition(ctx, r.Client, d.dp, devpodv1alpha1.ConditionReservationClearing, false, ""); err != nil {
				return err
			}
			r.event(d.dp, "ReservationCleared", fmt.Sprintf("%s 被 %s 预约至 %s，已自动休眠", res.Spec.Node, res.Spec.User, clock(res.Spec.End.Time)))
			if !slices.Contains(cleared, d.dp.Name) {
				cleared = append(cleared, d.dp.Name)
			}
			continue
		}
		msg := fmt.Sprintf("预约将于 %s 结束，届时自动休眠%s", clock(res.Spec.End.Time), noPersistenceWarning(d.dp))
		changed, err := setDevPodCondition(ctx, r.Client, d.dp, devpodv1alpha1.ConditionReservationEnding, ending, msg)
		if err != nil {
			return err
		}
		if changed && ending {
			r.event(d.dp, devpodv1alpha1.ConditionReservationEnding, msg)
		}
	}
	return r.setStatus(ctx, res, func(s *devpodv1alpha1.ReservationStatus) {
		s.Phase = devpodv1alpha1.ReservationActive
		s.ClearedDevPods = cleared
	})
}

// finish handles a reservation that is over: it ran out, was cancelled
// (or released early), or is being deleted.
func (r *ReservationReconciler) finish(ctx context.Context, res *devpodv1alpha1.Reservation, all []devpodv1alpha1.Reservation, now time.Time) (ctrl.Result, error) {
	deleting := !res.DeletionTimestamp.IsZero()
	cancelled := res.Spec.Cancelled != nil || deleting
	overAt := res.Spec.End.Time
	if res.Spec.Cancelled != nil && res.Spec.Cancelled.At.Time.Before(overAt) {
		overAt = res.Spec.Cancelled.At.Time
	} else if deleting && res.Spec.Cancelled == nil && now.Before(overAt) {
		overAt = now
	}
	done := res.Status.Phase == devpodv1alpha1.ReservationEnded || res.Status.Phase == devpodv1alpha1.ReservationCancelled
	if !done {
		if cancelled { // the segments that extend it go with it
			by := "system"
			if res.Spec.Cancelled != nil {
				by = res.Spec.Cancelled.By
			}
			for i := range all {
				o := &all[i]
				if o.Spec.Extends != res.Name || o.Spec.Cancelled != nil || !o.DeletionTimestamp.IsZero() {
					continue
				}
				before := o.DeepCopy()
				o.Spec.Cancelled = &devpodv1alpha1.ReservationCancel{By: by, At: metav1.NewTime(now)}
				if err := r.Patch(ctx, o, client.MergeFrom(before)); err != nil {
					return ctrl.Result{}, fmt.Errorf("cancel extension %s: %w", o.Name, err)
				}
			}
		}
		// it held the node if it had started (and was not a conflict's loser)
		started := res.Status.Phase == devpodv1alpha1.ReservationActive || (!res.Spec.Start.Time.After(overAt) && !loses(res, all))
		keep := !cancelled && successor(res, all) // an extension follows: their time goes on
		dps, err := r.devPodsOf(ctx, res.Spec.Node)
		if err != nil {
			return ctrl.Result{}, err
		}
		hibernated := append([]string(nil), res.Status.HibernatedAtEnd...)
		for _, d := range dps {
			if d.dp.Spec.Owner != res.Spec.User {
				// a notice given for a reservation that will not happen
				if _, err := setDevPodCondition(ctx, r.Client, d.dp, devpodv1alpha1.ConditionReservationClearing, false, ""); err != nil {
					return ctrl.Result{}, err
				}
				continue
			}
			if !started {
				continue
			}
			if _, err := setDevPodCondition(ctx, r.Client, d.dp, devpodv1alpha1.ConditionReservationEnding, false, ""); err != nil {
				return ctrl.Result{}, err
			}
			if keep {
				continue
			}
			if err := r.hibernate(ctx, d.dp, res); err != nil {
				return ctrl.Result{}, fmt.Errorf("hibernate %s: %w", d.dp.Name, err)
			}
			r.event(d.dp, "ReservationEnded", fmt.Sprintf("%s 的预约已结束，已自动休眠", res.Spec.Node))
			if !slices.Contains(hibernated, d.dp.Name) {
				hibernated = append(hibernated, d.dp.Name)
			}
		}
		if err := r.setStatus(ctx, res, func(s *devpodv1alpha1.ReservationStatus) {
			s.Phase = devpodv1alpha1.ReservationEnded
			if cancelled {
				s.Phase = devpodv1alpha1.ReservationCancelled
			}
			s.HibernatedAtEnd = hibernated
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	if controllerutil.RemoveFinalizer(res, reservationFinalizer) {
		if err := r.Update(ctx, res); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if deleting {
		return ctrl.Result{}, nil
	}
	if left := overAt.Add(r.Retention).Sub(now); left > 0 {
		return ctrl.Result{RequeueAfter: left}, nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, res))
}

// SetupWithManager registers the controller with mgr.
func (r *ReservationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("devpod-reservations")
	}
	return ctrl.NewControllerManagedBy(mgr).For(&devpodv1alpha1.Reservation{}).Complete(r)
}

// reservedMessage is why a DevPod pinned to a held node has no Pod.
func reservedMessage(dp *devpodv1alpha1.DevPod) string {
	if c := meta.FindStatusCondition(dp.Status.Conditions, devpodv1alpha1.ConditionNodeReserved); c != nil && c.Status == metav1.ConditionTrue {
		return c.Message
	}
	return ""
}
