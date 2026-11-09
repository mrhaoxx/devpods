// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrNoLeader means the ingest Service has no published endpoint.
var ErrNoLeader = errors.New("no webui leader published")

func sliceName(service string) string { return service + "-leader" }

// PublishIngestEndpoint points the selectorless ingest Service at this
// pod: its EndpointSlice holds exactly one endpoint.
func PublishIngestEndpoint(ctx context.Context, c client.Client, ns, service, podName, podIP string, port int32) error {
	at := discoveryv1.AddressTypeIPv4
	if strings.Contains(podIP, ":") {
		at = discoveryv1.AddressTypeIPv6
	}
	want := discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sliceName(service),
			Namespace: ns,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: service,
				discoveryv1.LabelManagedBy:   "devpod-webui",
			},
		},
		AddressType: at,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{podIP},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: podName},
		}},
		Ports: []discoveryv1.EndpointPort{{Name: ptr.To("ingest"), Port: ptr.To(port), Protocol: ptr.To(corev1.ProtocolTCP)}},
	}
	var cur discoveryv1.EndpointSlice
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: want.Name}, &cur)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, &want)
	}
	if err != nil {
		return err
	}
	cur.Labels, cur.AddressType, cur.Endpoints, cur.Ports = want.Labels, want.AddressType, want.Endpoints, want.Ports
	return c.Update(ctx, &cur)
}

// LeaderAddr returns the published leader's ip:port.
func LeaderAddr(ctx context.Context, c client.Client, ns, service string) (string, error) {
	var s discoveryv1.EndpointSlice
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: sliceName(service)}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return "", ErrNoLeader
		}
		return "", err
	}
	if len(s.Endpoints) == 0 || len(s.Endpoints[0].Addresses) == 0 || len(s.Ports) == 0 || s.Ports[0].Port == nil {
		return "", ErrNoLeader
	}
	return net.JoinHostPort(s.Endpoints[0].Addresses[0], strconv.Itoa(int(*s.Ports[0].Port))), nil
}
