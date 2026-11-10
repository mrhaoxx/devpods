// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestGatewayLiveResolver(t *testing.T) {
	setupSuite(t)
	ctx := context.Background()
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "lr-gw", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": "lr-gateway"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.1.2.3"}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "gw-1"}},
			{Addresses: []string{"10.1.2.4"}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "gw-2"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false), Terminating: ptr.To(true)}},
		},
	}
	if err := k8sClient.Create(ctx, slice); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, slice) })
	resolve := webui.GatewayLiveResolver(k8sManager.GetAPIReader(), "default", "lr-gateway", 8082)
	var got string
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if got, err = resolve(ctx, "gw-1"); err == nil {
			break
		}
	}
	if err != nil || got != "10.1.2.3:8082" {
		t.Fatalf("gw-1: %q %v", got, err)
	}
	if got, err := resolve(ctx, "gw-2"); err != nil || got != "10.1.2.4:8082" {
		t.Fatalf("a terminating gateway still drains its sessions: %q %v", got, err)
	}
	if _, err := resolve(ctx, "gw-9"); err == nil {
		t.Fatal("unknown pod must fail")
	}
}
