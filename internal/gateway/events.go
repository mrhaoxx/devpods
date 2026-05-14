// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

const eventSource = "devpod-gateway"

// EventRecorder writes Kubernetes Events on DevPod objects.
type EventRecorder struct {
	reader    client.Reader
	clientset kubernetes.Interface
	dpNS      string
}

func NewEventRecorder(reader client.Reader, clientset kubernetes.Interface, devpodNamespace string) *EventRecorder {
	return &EventRecorder{reader: reader, clientset: clientset, dpNS: devpodNamespace}
}

func (r *EventRecorder) record(ctx context.Context, podName, eventType, reason, message string) {
	if r == nil {
		return
	}
	var dp devpodv1alpha1.DevPod
	if err := r.reader.Get(ctx, types.NamespacedName{Name: podName, Namespace: r.dpNS}, &dp); err != nil {
		return
	}
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: dp.Name + "-",
			Namespace:    dp.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "devpod.io/v1alpha1",
			Kind:       "DevPod",
			Name:       dp.Name,
			Namespace:  dp.Namespace,
			UID:        dp.UID,
		},
		Reason:              reason,
		Message:             message,
		Type:                eventType,
		EventTime:           metav1.NewMicroTime(now.Time),
		ReportingController: eventSource,
		ReportingInstance:   eventSource,
		Action:              reason,
		FirstTimestamp:      now,
		LastTimestamp:        now,
	}
	_, _ = r.clientset.CoreV1().Events(dp.Namespace).Create(ctx, event, metav1.CreateOptions{})
}

// SessionConnected records a successful SSH session.
func (r *EventRecorder) SessionConnected(ctx context.Context, podName, user, clientIP, authPath string) {
	r.record(ctx, podName, corev1.EventTypeNormal, "SessionConnected",
		fmt.Sprintf("SSH session from %s by %s (%s)", clientIP, user, authPath))
}

// SessionDisconnected records a session close.
func (r *EventRecorder) SessionDisconnected(ctx context.Context, podName, user, reason string) {
	r.record(ctx, podName, corev1.EventTypeNormal, "SessionDisconnected",
		fmt.Sprintf("SSH session ended for %s: %s", user, reason))
}

// AuthRejected records a failed authentication attempt.
func (r *EventRecorder) AuthRejected(ctx context.Context, podName, user, clientIP, reason string) {
	r.record(ctx, podName, corev1.EventTypeWarning, "AuthRejected",
		fmt.Sprintf("Auth rejected for %s from %s: %s", user, clientIP, reason))
}

// DialFailed records a backend dial failure.
func (r *EventRecorder) DialFailed(ctx context.Context, podName, endpoint, errMsg string) {
	r.record(ctx, podName, corev1.EventTypeWarning, "DialFailed",
		fmt.Sprintf("Failed to dial backend %s: %s", endpoint, errMsg))
}
