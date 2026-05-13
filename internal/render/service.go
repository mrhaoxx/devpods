// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Service renders the headless Service exposing the in-container
// sshd. The gateway dials DevPod.status.endpoint, not this Service,
// but the Service is useful for kubectl port-forward, intra-cluster
// addressing, and as a stable selector for the per-DevPod Pod.
//
// Port and targetPort track GatewayConfig.spec.backendPort (default
// 2222 in v2 so the user container binds without
// CAP_NET_BIND_SERVICE).
func Service(dp *devpodv1alpha1.DevPod, cfg *devpodv1alpha1.GatewayConfig) (*corev1.Service, error) {
	port := BackendPort(dp, cfg)
	return &corev1.Service{
		ObjectMeta: ObjectMeta(ServiceName(dp), cfg.Spec.DevPodNamespace, dp),
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector: map[string]string{
				LabelOwner:  dp.Spec.Owner,
				LabelDevPod: dp.Name,
			},
			Ports: []corev1.ServicePort{{
				Name:       "ssh",
				Port:       port,
				TargetPort: intstr.FromInt32(port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}, nil
}
