// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
)

const devpodFinalizer = "devpod.io/devpod-cleanup"

// DevPodReconciler reconciles DevPod objects into Pod + Service + Secret + NetworkPolicy.
type DevPodReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// GwConfig is the (typically singleton) GatewayConfig used at render
	// time. In this skeleton plan we inject the value directly; a
	// follow-up plan will wire it from a real GatewayConfig Get.
	GwConfig *devpodv1alpha1.GatewayConfig

	// GatewayNamespace is the namespace where the gateway Deployment
	// runs. Per-owner NetworkPolicy uses it as the trusted source for
	// ingress on port 22.
	GatewayNamespace string

	// Telemetry hands each DevPod its ingest token and telemetry.json
	// through the host-key Secret; nil = telemetry off.
	Telemetry *render.TelemetrySettings

	// GatewayInternalPub is the gateway's internal public key in
	// OpenSSH authorized_keys-line form, embedded into every per-DevPod
	// host-key Secret as the sidecar's sole authorized key.
	GatewayInternalPub []byte

	// Reservations turns on the node-reservation rules (reservation spec
	// §4): Pods keep off nodes someone else holds, and a DevPod pinned to
	// such a node waits without a Pod. Now is the clock (nil = time.Now).
	Reservations bool
	Now          func() time.Time

	// excluded is set within a single reconcile pass: the nodes other
	// users hold right now, kept off a Pod created in this pass.
	excluded []string

	// requeueAfter is set within a single reconcile pass when the
	// controller wants to delay before retrying (e.g. failure backoff).
	// Reconcile reads and clears it after applyAll returns.
	requeueAfter time.Duration
}

// +kubebuilder:rbac:groups=devpod.io,resources=devpods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=devpod.io,resources=devpods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=devpod.io,resources=devpods/finalizers,verbs=update
// +kubebuilder:rbac:groups=devpod.io,resources=users,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods;services;secrets;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one round of DevPod state.
func (r *DevPodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("devpod", req.NamespacedName)

	var dp devpodv1alpha1.DevPod
	if err := r.Get(ctx, req.NamespacedName, &dp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get devpod: %w", err)
	}

	if !dp.DeletionTimestamp.IsZero() {
		if err := r.detachPVC(ctx, &dp); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.cleanupOwnerNetPolIfLast(ctx, &dp); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&dp, devpodFinalizer)
		if err := r.Update(ctx, &dp); err != nil {
			return ctrl.Result{}, fmt.Errorf("clear finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&dp, devpodFinalizer) {
		if err := r.Update(ctx, &dp); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// spec.owner is an opaque username string. A User CRD is one of
	// several identity sources (others: LDAP, future OIDC); the gateway
	// proves identity at auth time. Spec §1: an LDAP-only owner has no
	// User CRD and the reconciler must still materialize the Pod.

	if !dp.Spec.Running {
		if err := r.hibernate(ctx, &dp); err != nil {
			return ctrl.Result{}, err
		}
		if r.Reservations { // a stopped DevPod waits for nothing
			if _, err := setDevPodCondition(ctx, r.Client, &dp, devpodv1alpha1.ConditionNodeReserved, false, ""); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, r.updateStatus(ctx, &dp)
	}

	// VM path is recognized but not implemented in this plan.
	if dp.Spec.VM != nil {
		log.Info("VM-backed DevPod is not implemented in this milestone; skipping render")
		return ctrl.Result{}, nil
	}
	if dp.Spec.Pod == nil {
		// CEL should have rejected this at admission. Returning an error
		// here would burn workqueue cycles with exponential backoff against
		// a permanently-broken object; log instead and stop retrying. The
		// admission webhook (Task 14) is the authoritative defense.
		log.Error(errors.New("invalid DevPod"), "neither spec.pod nor spec.vm set; CEL should have rejected", "name", dp.Name)
		return ctrl.Result{}, nil
	}

	r.requeueAfter, r.excluded = 0, nil
	if r.Reservations {
		wait, err := r.reservationGate(ctx, &dp)
		if err != nil {
			return ctrl.Result{}, err
		}
		if wait > 0 { // pinned to a node someone else holds: no Pod until it is free
			return ctrl.Result{RequeueAfter: wait}, r.updateStatus(ctx, &dp)
		}
	}
	if err := r.applyAll(ctx, &dp); err != nil {
		return ctrl.Result{}, err
	}
	if r.requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: r.requeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *DevPodReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// reservationGate applies the reservation rules to a running DevPod. It
// records the nodes other users hold (r.excluded). A DevPod pinned to
// one of them, with no Pod yet, gets the NodeReserved condition and a
// positive wait (until that reservation ends); otherwise the condition
// is cleared and the wait is zero. A Pod that already runs there is left
// to the reservation's own clearing step.
func (r *DevPodReconciler) reservationGate(ctx context.Context, dp *devpodv1alpha1.DevPod) (time.Duration, error) {
	var all devpodv1alpha1.ReservationList
	if err := r.List(ctx, &all); err != nil {
		return 0, fmt.Errorf("list reservations: %w", err)
	}
	now := r.now()
	held := heldByOthers(all.Items, dp.Spec.Owner, now)
	for node := range held {
		r.excluded = append(r.excluded, node)
	}
	if res, ok := held[PinnedNode(dp)]; ok {
		var pod corev1.Pod
		err := r.Get(ctx, types.NamespacedName{Name: render.PodName(dp), Namespace: r.GwConfig.Spec.DevPodNamespace}, &pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("get pod: %w", err)
		}
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("%s 被 %s 预约至 %s", res.Spec.Node, res.Spec.User, clock(res.Spec.End.Time))
			if _, err := setDevPodCondition(ctx, r.Client, dp, devpodv1alpha1.ConditionNodeReserved, true, msg); err != nil {
				return 0, err
			}
			return max(res.Spec.End.Time.Sub(now), time.Second), nil
		}
	}
	_, err := setDevPodCondition(ctx, r.Client, dp, devpodv1alpha1.ConditionNodeReserved, false, "")
	return 0, err
}

// reservationDevPods maps a Reservation event to the running DevPods
// pinned to its node: one of them may be waiting for the node, or must
// start waiting.
func (r *DevPodReconciler) reservationDevPods(ctx context.Context, obj client.Object) []reconcile.Request {
	res, ok := obj.(*devpodv1alpha1.Reservation)
	if !ok {
		return nil
	}
	var list devpodv1alpha1.DevPodList
	if err := r.List(ctx, &list, client.InNamespace(r.GwConfig.Spec.DevPodNamespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if dp := &list.Items[i]; dp.Spec.Running && PinnedNode(dp) == res.Spec.Node {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
		}
	}
	return out
}

// applyAll renders and applies the full object set for one DevPod.
func (r *DevPodReconciler) applyAll(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	// 1. Host key Secret (idempotent: keep the existing one if present so we
	// don't churn the key on every reconcile).
	if err := r.ensureHostKeySecret(ctx, dp); err != nil {
		return fmt.Errorf("host key secret: %w", err)
	}

	// 2. Home PVC (only when spec.persistence is set).
	if err := r.applyPVC(ctx, dp); err != nil {
		return fmt.Errorf("apply pvc: %w", err)
	}

	// 3. Pod. Most Pod spec fields (command, args, volumeMounts, env, ...)
	// are immutable post-create, so server-side apply on a changed
	// spec returns Invalid. Instead: stamp a hash of the rendered
	// spec into an annotation; if the existing Pod's hash differs,
	// delete it and let the next reconcile recreate it on the new
	// spec. (Active SSH sessions on the Pod drop — same semantics as
	// hibernate.)
	pod, err := render.Pod(dp, r.GwConfig)
	if err != nil {
		return fmt.Errorf("render pod: %w", err)
	}
	if err := r.applyPodWithDriftRecreate(ctx, dp, pod); err != nil {
		return fmt.Errorf("apply pod: %w", err)
	}

	// 4. Service.
	svc, err := render.Service(dp, r.GwConfig)
	if err != nil {
		return fmt.Errorf("render service: %w", err)
	}
	if err := r.applyOwned(ctx, dp, svc); err != nil {
		return fmt.Errorf("apply service: %w", err)
	}

	// 5. NetworkPolicies — only when IsolateNetwork is enabled.
	if r.GwConfig.Spec.IsolateNetwork {
		dd := render.DefaultDenyNetworkPolicy(r.GwConfig.Spec.DevPodNamespace)
		if err := r.applyUnowned(ctx, dd); err != nil {
			return fmt.Errorf("apply default-deny: %w", err)
		}
		gwNS := r.GatewayNamespace
		if gwNS == "" {
			gwNS = "devpod-system"
		}
		var ingestPort int32
		if r.Telemetry != nil {
			ingestPort = render.IngestPort
		}
		allow := render.OwnerAllowNetworkPolicy(r.GwConfig.Spec.DevPodNamespace, dp.Spec.Owner, gwNS, render.BackendPort(dp, r.GwConfig), ingestPort)
		if err := r.applyUnowned(ctx, allow); err != nil {
			return fmt.Errorf("apply allow: %w", err)
		}
	}

	return r.updateStatus(ctx, dp)
}

// hibernate deletes the workload Pod (if present) and leaves all other
// per-DevPod objects (Service, host-key Secret, NetworkPolicy, PVC)
// intact so that flipping spec.running back to true is a cheap
// re-create of just the Pod.
func (r *DevPodReconciler) hibernate(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      render.PodName(dp),
			Namespace: r.GwConfig.Spec.DevPodNamespace,
		},
	}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod: %w", err)
	}
	return nil
}

// detachPVC strips this DevPod's OwnerReference from the home PVC so
// it survives Kubernetes garbage collection. If the PVC does not exist
// (was never created, or persistence was off), this is a no-op.
func (r *DevPodReconciler) detachPVC(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	if dp.Spec.Persistence == nil {
		return nil
	}
	var pvc corev1.PersistentVolumeClaim
	key := types.NamespacedName{Name: render.HomePVCName(dp), Namespace: r.GwConfig.Spec.DevPodNamespace}
	if err := r.Get(ctx, key, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get pvc: %w", err)
	}
	filtered := make([]metav1.OwnerReference, 0, len(pvc.OwnerReferences))
	changed := false
	for _, or := range pvc.OwnerReferences {
		if or.Kind == "DevPod" && or.UID == dp.UID {
			changed = true
			continue
		}
		filtered = append(filtered, or)
	}
	if !changed {
		return nil
	}
	patch := client.MergeFrom(pvc.DeepCopy())
	pvc.OwnerReferences = filtered
	return r.Patch(ctx, &pvc, patch)
}

// cleanupOwnerNetPolIfLast deletes the per-owner allow NetworkPolicy
// when the DevPod being deleted is the owner's last one. Idempotent;
// a concurrent finalizer running for a sibling will see this DevPod's
// DeletionTimestamp (so skip it) and the second Delete returns
// NotFound which we ignore.
func (r *DevPodReconciler) cleanupOwnerNetPolIfLast(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	var siblings devpodv1alpha1.DevPodList
	if err := r.List(ctx, &siblings, client.InNamespace(r.GwConfig.Spec.DevPodNamespace)); err != nil {
		return fmt.Errorf("list devpods: %w", err)
	}
	for i := range siblings.Items {
		s := &siblings.Items[i]
		if s.UID == dp.UID {
			continue
		}
		if s.Spec.Owner != dp.Spec.Owner {
			continue
		}
		if s.DeletionTimestamp.IsZero() {
			// A live sibling exists — keep the policy.
			return nil
		}
	}
	// No live sibling for this owner — remove the policy.
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      render.OwnerNetPolName(dp.Spec.Owner),
			Namespace: r.GwConfig.Spec.DevPodNamespace,
		},
	}
	if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete owner allow netpol: %w", err)
	}
	return nil
}

// applyPVC creates the home PVC when spec.persistence is set. Idempotent
// via the existing applyOwned path.
func (r *DevPodReconciler) applyPVC(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	if dp.Spec.Persistence == nil {
		return nil
	}
	pvc, err := render.HomePVC(dp, r.GwConfig)
	if err != nil {
		return fmt.Errorf("render pvc: %w", err)
	}
	return r.applyOwned(ctx, dp, pvc)
}

// updateStatus reads the rendered Pod and patches DevPod.status.{phase,endpoint,workloadRef,...}.
func (r *DevPodReconciler) updateStatus(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	desired := devpodv1alpha1.DevPodStatus{
		Conditions: dp.Status.Conditions,
	}
	if dp.Spec.Persistence != nil {
		desired.PersistentVolumeClaimRef = &devpodv1alpha1.LocalObjectRef{Name: render.HomePVCName(dp)}
	}

	if !dp.Spec.Running {
		desired.Phase = devpodv1alpha1.DevPodStopped
		if dp.Status.HibernatedAt != nil && dp.Status.Phase == devpodv1alpha1.DevPodStopped {
			desired.HibernatedAt = dp.Status.HibernatedAt
		} else {
			now := metav1.Now()
			desired.HibernatedAt = &now
		}
	} else {
		desired.HibernatedAt = nil // sticky-cleared on resume
		var pod corev1.Pod
		err := r.Get(ctx, types.NamespacedName{Name: render.PodName(dp), Namespace: r.GwConfig.Spec.DevPodNamespace}, &pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get rendered pod: %w", err)
		}
		if err == nil {
			desired.WorkloadRef = &devpodv1alpha1.WorkloadRef{APIVersion: "v1", Kind: "Pod", Name: pod.Name}
			switch {
			case pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "":
				port := render.BackendPort(dp, r.GwConfig)
				desired.Phase = devpodv1alpha1.DevPodRunning
				desired.Endpoint = fmt.Sprintf("%s:%d", pod.Status.PodIP, port)
				desired.RetryCount = 0
				desired.LastFailureAt = nil
				desired.Message = ""
			case pod.Status.Phase == corev1.PodFailed:
				desired.Phase = devpodv1alpha1.DevPodFailed
				desired.RetryCount = dp.Status.RetryCount
				desired.LastFailureAt = dp.Status.LastFailureAt
				desired.Message = dp.Status.Message
			default:
				desired.Phase = devpodv1alpha1.DevPodPending
			}
		} else {
			desired.Phase = devpodv1alpha1.DevPodPending
			desired.Message = reservedMessage(dp) // waiting for a reserved node
		}
	}

	if statusEqual(dp.Status, desired) {
		return nil
	}
	patch := client.MergeFrom(dp.DeepCopy())
	dp.Status.Phase = desired.Phase
	dp.Status.Endpoint = desired.Endpoint
	dp.Status.WorkloadRef = desired.WorkloadRef
	dp.Status.PersistentVolumeClaimRef = desired.PersistentVolumeClaimRef
	dp.Status.HibernatedAt = desired.HibernatedAt
	dp.Status.RetryCount = desired.RetryCount
	dp.Status.LastFailureAt = desired.LastFailureAt
	dp.Status.Message = desired.Message
	return r.Status().Patch(ctx, dp, patch)
}

func statusEqual(a, b devpodv1alpha1.DevPodStatus) bool {
	return a.Phase == b.Phase &&
		a.Endpoint == b.Endpoint &&
		a.RetryCount == b.RetryCount &&
		a.Message == b.Message &&
		workloadRefEqual(a.WorkloadRef, b.WorkloadRef) &&
		localRefEqual(a.PersistentVolumeClaimRef, b.PersistentVolumeClaimRef) &&
		timePtrEqual(a.HibernatedAt, b.HibernatedAt) &&
		timePtrEqual(a.LastFailureAt, b.LastFailureAt)
}

func timePtrEqual(a, b *metav1.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Time.Equal(b.Time)
	}
}

func workloadRefEqual(a, b *devpodv1alpha1.WorkloadRef) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func localRefEqual(a, b *devpodv1alpha1.LocalObjectRef) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Name == b.Name
	}
}

func (r *DevPodReconciler) ensureHostKeySecret(ctx context.Context, dp *devpodv1alpha1.DevPod) error {
	var existing corev1.Secret
	key := types.NamespacedName{Name: render.HostKeySecretName(dp), Namespace: r.GwConfig.Spec.DevPodNamespace}
	err := r.Get(ctx, key, &existing)
	if err == nil {
		// Defense-in-depth: the host-key Secret name is derived from
		// (owner, name), so a prior orphan or a name-collision attack
		// could leave a same-named Secret with no ownerRef to us or
		// with empty contents. Bail loudly if the existing Secret
		// isn't recognisably ours; the admission webhook owns the
		// stronger PodName-collision check (see FOLLOWUPS.md).
		if !metav1.IsControlledBy(&existing, dp) {
			return fmt.Errorf("host-key Secret %q exists but is not controlled by this DevPod", key)
		}
		if len(existing.Data["ssh_host_ed25519_key"]) == 0 {
			return fmt.Errorf("host-key Secret %q exists but is missing ssh_host_ed25519_key", key)
		}
		return r.syncTelemetryKeys(ctx, dp, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get host key secret: %w", err)
	}
	sec, err := render.HostKeySecret(dp, r.GwConfig, nil, r.GatewayInternalPub)
	if err != nil {
		return err
	}
	if r.Telemetry != nil {
		data, err := render.TelemetrySecretData(dp, r.Telemetry)
		if err != nil {
			return err
		}
		for k, v := range data {
			sec.Data[k] = v
		}
	}
	if err := controllerutil.SetControllerReference(dp, sec, r.Scheme); err != nil {
		return fmt.Errorf("set ownerref on secret: %w", err)
	}
	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create host key secret: %w", err)
	}
	return nil
}

// syncTelemetryKeys keeps the Secret's ingest-token / telemetry.json in
// step with the controller's settings (spec §10.3: reconciled, not only
// written at creation). The SSH keys are never touched.
func (r *DevPodReconciler) syncTelemetryKeys(ctx context.Context, dp *devpodv1alpha1.DevPod, sec *corev1.Secret) error {
	if r.Telemetry == nil {
		return nil
	}
	want, err := render.TelemetrySecretData(dp, r.Telemetry)
	if err != nil {
		return err
	}
	changed := false
	for k, v := range want {
		if !bytes.Equal(sec.Data[k], v) {
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			sec.Data[k] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := r.Update(ctx, sec); err != nil {
		return fmt.Errorf("update telemetry keys: %w", err)
	}
	return nil
}

// attachRegistrySecret adds the owner's registry credentials Secret to a
// new Pod's imagePullSecrets when the Secret exists.
func (r *DevPodReconciler) attachRegistrySecret(ctx context.Context, dp *devpodv1alpha1.DevPod, pod *corev1.Pod) error {
	name := devpodv1alpha1.RegistryCredentialsSecretName(dp.Spec.Owner)
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pod.Namespace}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get registry secret: %w", err)
	}
	// only the Secret the user controller wrote for this owner
	if sec.Labels[devpodv1alpha1.RegistryCredentialsLabel] != "true" || sec.Labels[devpodv1alpha1.OwnerLabel] != dp.Spec.Owner {
		return nil
	}
	for _, ref := range pod.Spec.ImagePullSecrets {
		if ref.Name == name {
			return nil
		}
	}
	pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
	return nil
}

// specHashAnnotation tags a rendered Pod with a SHA256 of its spec.
// On each reconcile the controller compares it against the live Pod;
// a mismatch triggers a delete-then-recreate cycle since most Pod
// fields are immutable.
const specHashAnnotation = "devpod.io/spec-hash"

// applyPodWithDriftRecreate compares the rendered Pod's spec-hash
// against the live Pod's annotation. If they match, we still apply
// (no-op for unchanged spec; converges any controller-owned field
// drift the apiserver might surface, e.g. labels). If they differ,
// we Delete the live Pod and return — the next reconcile recreates
// it from the new render.
func (r *DevPodReconciler) applyPodWithDriftRecreate(ctx context.Context, dp *devpodv1alpha1.DevPod, pod *corev1.Pod) error {
	if err := controllerutil.SetControllerReference(dp, pod, r.Scheme); err != nil {
		return fmt.Errorf("set ownerref: %w", err)
	}
	hash := podSpecHash(pod)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[specHashAnnotation] = hash

	var live corev1.Pod
	key := types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}
	err := r.Get(ctx, key, &live)
	switch {
	case err == nil:
		if live.Status.Phase == corev1.PodFailed {
			return r.handleFailedPod(ctx, dp, &live)
		}
		if live.Annotations[specHashAnnotation] != hash {
			if delErr := r.Delete(ctx, &live); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("delete pod for drift recreate: %w", delErr)
			}
			return nil
		}
		if r.Reservations {
			// affinity is immutable: keep the exclusion the Pod was created
			// with, whatever is reserved now
			pod.Spec.Affinity = live.Spec.Affinity.DeepCopy()
		}
		// imagePullSecrets are immutable too: the Pod keeps the ones it
		// was created with, whatever credentials its owner has now
		pod.Spec.ImagePullSecrets = live.Spec.ImagePullSecrets
	case apierrors.IsNotFound(err):
		// First create — check backoff before recreating.
		if dp.Status.LastFailureAt != nil {
			backoff := failureBackoff(int(dp.Status.RetryCount))
			elapsed := time.Since(dp.Status.LastFailureAt.Time)
			if elapsed < backoff {
				r.requeueAfter = backoff - elapsed
				return nil
			}
		}
		// Keep a new Pod off nodes other users hold right now (reservation
		// spec §4.1). Added after the hash on purpose: reservations coming
		// and going must never read as drift and recreate a running Pod.
		excludeNodes(pod, r.excluded)
		// The owner's registry credentials (snapshot spec v2 §5.2), also
		// after the hash: saving or removing a credential must not
		// recreate running Pods, it only reaches Pods created later.
		if err := r.attachRegistrySecret(ctx, dp, pod); err != nil {
			return err
		}
	default:
		return fmt.Errorf("get live pod: %w", err)
	}
	return r.serverSideApply(ctx, pod)
}

// podSpecHash returns a hex SHA256 over the rendered Pod's spec.
// Stable across reconciles for the same render output.
// podSpecHash fingerprints the parts of the rendered spec whose drift justifies
// recreating the Pod — i.e. everything that can actually change a RUNNING pod.
//
// The controller-injected bootstrap initContainer is deliberately excluded: it
// has already completed on a live Pod, so changing its image (the operator-side
// --supervisor-image lever) cannot affect that Pod at all. Hashing it meant a
// supervisor bump deleted and recreated every DevPod in the fleet — dropping
// everyone's SSH sessions — to deliver exactly nothing; the new image takes
// effect naturally the next time the Pod is created for any other reason.
//
// Only that one container is skipped. Everything the user or template author
// wrote — including their own initContainers — keeps counting as drift, so
// editing a DevPod still applies with the restart the UI already warns about.
func podSpecHash(pod *corev1.Pod) string {
	spec := pod.Spec.DeepCopy()
	if len(spec.InitContainers) > 0 &&
		spec.InitContainers[0].Name == render.SupervisorBootstrapContainerName {
		spec.InitContainers = spec.InitContainers[1:]
	}
	b, _ := json.Marshal(spec)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fieldOwner is the field-manager name the controller uses for
// server-side apply. Stable identifier so the apiserver tracks which
// fields we own across reconciles.
const fieldOwner = "devpod-controller"

// applyOwned applies obj via server-side apply with the DevPod as its
// controller ownerRef. Subsequent reconciles converge any drift back
// to the rendered shape.
func (r *DevPodReconciler) applyOwned(ctx context.Context, dp *devpodv1alpha1.DevPod, obj client.Object) error {
	if err := controllerutil.SetControllerReference(dp, obj, r.Scheme); err != nil {
		return fmt.Errorf("set ownerref: %w", err)
	}
	return r.serverSideApply(ctx, obj)
}

// applyUnowned applies an object that should NOT have the DevPod as
// owner (namespace-wide objects: default-deny policy, per-owner allow
// policy).
func (r *DevPodReconciler) applyUnowned(ctx context.Context, obj client.Object) error {
	return r.serverSideApply(ctx, obj)
}

// serverSideApply patches obj with PatchType=Apply, ForceOwnership so
// the apiserver does the three-way merge for us. Resource ownership
// of every field we set is taken over from any other field manager
// (operator's kubectl edit is welcome until next reconcile undoes it,
// which is the intended convergent behavior).
//
// The object must have its TypeMeta populated; serverSideApply fills
// it from the scheme when needed so callers can keep using zero-value
// TypeMeta.
func (r *DevPodReconciler) serverSideApply(ctx context.Context, obj client.Object) error {
	if obj.GetObjectKind().GroupVersionKind().Empty() {
		gvks, _, err := r.Scheme.ObjectKinds(obj)
		if err != nil {
			return fmt.Errorf("scheme.ObjectKinds: %w", err)
		}
		if len(gvks) > 0 {
			obj.GetObjectKind().SetGroupVersionKind(gvks[0])
		}
	}
	// Apply must not include resourceVersion (it implies optimistic
	// concurrency, but Apply does its own conflict resolution).
	obj.SetResourceVersion("")
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership)
}

// handleFailedPod implements CrashLoopBackOff-style retry for Failed pods.
// It records the failure in status, deletes the Failed pod, and sets
// requeueAfter so the next reconcile respects the backoff window.
func (r *DevPodReconciler) handleFailedPod(ctx context.Context, dp *devpodv1alpha1.DevPod, pod *corev1.Pod) error {
	log := log.FromContext(ctx)

	msg := podFailureMessage(pod)
	now := metav1.Now()
	retryCount := dp.Status.RetryCount + 1

	patch := client.MergeFrom(dp.DeepCopy())
	dp.Status.Phase = devpodv1alpha1.DevPodFailed
	dp.Status.RetryCount = retryCount
	dp.Status.LastFailureAt = &now
	dp.Status.Message = msg
	if err := r.Status().Patch(ctx, dp, patch); err != nil {
		return fmt.Errorf("patch failure status: %w", err)
	}

	if delErr := r.Delete(ctx, pod); delErr != nil && !apierrors.IsNotFound(delErr) {
		return fmt.Errorf("delete failed pod: %w", delErr)
	}

	backoff := failureBackoff(int(retryCount))
	log.Info("pod failed, backing off", "retryCount", retryCount, "backoff", backoff, "message", msg)
	r.requeueAfter = backoff
	return nil
}

func failureBackoff(retryCount int) time.Duration {
	d := 10 * time.Second
	for i := 0; i < retryCount-1 && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func podFailureMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
			return cs.State.Terminated.Message
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	return "pod failed"
}

// SetupWithManager registers the controller with mgr.
func (r *DevPodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&devpodv1alpha1.DevPod{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.NetworkPolicy{})
	if r.Reservations {
		b = b.Watches(&devpodv1alpha1.Reservation{}, handler.EnqueueRequestsFromMapFunc(r.reservationDevPods))
	}
	return b.Complete(r)
}
