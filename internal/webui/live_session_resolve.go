// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"fmt"
	"net"
	"strconv"

	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GatewayLiveResolver finds a gateway Pod's live endpoint through the
// gateway Service's EndpointSlices (terminating Pods included: they keep
// serving their sessions while they drain).
func GatewayLiveResolver(reader client.Reader, namespace, service string, port int) func(context.Context, string) (string, error) {
	return func(ctx context.Context, pod string) (string, error) {
		var slices discoveryv1.EndpointSliceList
		if err := reader.List(ctx, &slices, client.InNamespace(namespace), client.MatchingLabels{discoveryv1.LabelServiceName: service}); err != nil {
			return "", err
		}
		for _, sl := range slices.Items {
			for _, ep := range sl.Endpoints {
				if ep.TargetRef != nil && ep.TargetRef.Kind == "Pod" && ep.TargetRef.Name == pod && len(ep.Addresses) > 0 {
					return net.JoinHostPort(ep.Addresses[0], strconv.Itoa(port)), nil
				}
			}
		}
		return "", fmt.Errorf("gateway pod %q is not among %s's endpoints", pod, service)
	}
}
