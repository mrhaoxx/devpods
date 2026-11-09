// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/render"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

func TestTelemetrySecretData_TokenAndHostNetwork(t *testing.T) {
	dp := &devpodv1alpha1.DevPod{
		ObjectMeta: metav1.ObjectMeta{Name: "d", UID: "uid-9"},
		Spec: devpodv1alpha1.DevPodSpec{
			Owner:       "alice",
			Pod:         &devpodv1alpha1.PodWorkloadSpec{Spec: corev1.PodSpec{HostNetwork: true}},
			Persistence: &devpodv1alpha1.PersistenceSpec{Size: resource.MustParse("1Gi"), MountPath: "/root"},
		},
	}
	s := &render.TelemetrySettings{IngestURL: "http://in/ingest/v1/batch", Key: []byte("k"), Defaults: config.DefaultConfig()}
	s.Defaults.TopN = 7
	data, err := render.TelemetrySecretData(dp, s)
	if err != nil {
		t.Fatal(err)
	}
	if string(data["ingest-token"]) != wire.Token([]byte("k"), "uid-9") {
		t.Fatal("token must be HMAC(key, DevPod UID)")
	}
	var c config.Config
	if err := json.Unmarshal(data["telemetry.json"], &c); err != nil {
		t.Fatal(err)
	}
	if !c.HostNetwork || c.DevPodUID != "uid-9" || c.IngestURL != "http://in/ingest/v1/batch" || c.TopN != 7 ||
		len(c.PVCMounts) != 1 || c.PVCMounts[0] != "/root" {
		t.Fatalf("%+v", c)
	}
	dp.UID = ""
	if _, err := render.TelemetrySecretData(dp, s); err == nil {
		t.Fatal("a DevPod without UID cannot get a token")
	}
}

func TestOwnerAllowNetPol_IngestEgress(t *testing.T) {
	on := render.OwnerAllowNetworkPolicy("devpods", "alice", "devpod-system", 2222, 8081)
	off := render.OwnerAllowNetworkPolicy("devpods", "alice", "devpod-system", 2222, 0)
	find := func(rules []networkingv1.NetworkPolicyEgressRule) bool {
		for _, r := range rules {
			for _, p := range r.Ports {
				if p.Port != nil && p.Port.IntValue() == 8081 && len(r.To) == 1 && r.To[0].PodSelector != nil &&
					r.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] == "devpod-webui" &&
					r.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "devpod-system" {
					return true
				}
			}
		}
		return false
	}
	if !find(on.Spec.Egress) || find(off.Spec.Egress) {
		t.Fatalf("ingest egress rule must exist only when enabled")
	}
}
