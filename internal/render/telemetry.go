// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"encoding/json"
	"errors"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/telemetry/config"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

// Host-key Secret keys carrying the telemetry configuration (spec §10.3).
// They are mounted at /etc/devpod/ like the SSH keys; changing them does
// not touch the Pod spec, so it never recreates a running DevPod.
const (
	SecretKeyIngestToken     = "ingest-token"
	SecretKeyTelemetryConfig = "telemetry.json"
)

// IngestPort is the webui's ingest listener.
const IngestPort = 8081

// TelemetrySettings is the controller-wide telemetry configuration; nil
// on the reconciler means telemetry is off.
type TelemetrySettings struct {
	IngestURL string
	Key       []byte
	Defaults  config.Config
}

// TelemetrySecretData renders the per-DevPod token and telemetry.json.
func TelemetrySecretData(dp *devpodv1alpha1.DevPod, s *TelemetrySettings) (map[string][]byte, error) {
	if dp.UID == "" {
		return nil, errors.New("telemetry: DevPod has no UID yet")
	}
	c := s.Defaults
	c.IngestURL, c.DevPodUID = s.IngestURL, string(dp.UID)
	c.HostNetwork = dp.Spec.Pod != nil && dp.Spec.Pod.Spec.HostNetwork
	c.PVCMounts = nil
	if dp.Spec.Persistence != nil && dp.Spec.Persistence.MountPath != "" {
		c.PVCMounts = []string{dp.Spec.Persistence.MountPath}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		SecretKeyIngestToken:     []byte(wire.Token(s.Key, string(dp.UID))),
		SecretKeyTelemetryConfig: b,
	}, nil
}
