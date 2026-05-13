// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DefaultDenyNetworkPolicy returns the namespace-wide default-deny policy
// installed once in the devpods namespace. Selector is empty (every pod);
// no Ingress or Egress rules ⇒ deny all by default.
func DefaultDenyNetworkPolicy(ns string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "devpod-default-deny",
			Namespace: ns,
			Labels: map[string]string{
				LabelManaged: "devpod-controller",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
}

// OwnerAllowNetworkPolicy returns the per-owner allow policy:
//   - ingress from same-owner pods
//   - ingress from the gateway namespace on port 22
//   - egress to DNS (UDP 53)
//   - egress to anywhere else not in the cluster (egress to 0.0.0.0/0 except RFC1918)
//
// gatewayNS is the namespace where the gateway runs (typically `devpod-system`).
func OwnerAllowNetworkPolicy(ns, owner, gatewayNS string) *networkingv1.NetworkPolicy {
	tcp22 := port(corev1.ProtocolTCP, 22)
	udp53 := port(corev1.ProtocolUDP, 53)
	tcp53 := port(corev1.ProtocolTCP, 53)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OwnerNetPolName(owner),
			Namespace: ns,
			Labels: map[string]string{
				LabelOwner:   owner,
				LabelManaged: "devpod-controller",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{LabelOwner: owner},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{LabelOwner: owner},
						},
					}},
				},
				{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": gatewayNS},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{tcp22},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{}, // any cluster namespace; reaches kube-dns
					}},
					Ports: []networkingv1.NetworkPolicyPort{udp53, tcp53},
				},
				{
					// Egress to anywhere outside the cluster; restricting
					// further is a deployment-time decision.
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{
							CIDR: "0.0.0.0/0",
							Except: []string{
								"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
							},
						},
					}},
				},
			},
		},
	}
}

// port is a small helper that returns a NetworkPolicyPort value for clean
// nested literal use; the Protocol field is a pointer so we take the
// address of a local copy.
func port(proto corev1.Protocol, num int32) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(num)
	return networkingv1.NetworkPolicyPort{
		Protocol: &proto,
		Port:     &p,
	}
}
