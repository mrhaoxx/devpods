// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import "github.com/prometheus/client_golang/prometheus"

var (
	// SpoolBytes is the gateway's local audit spool usage.
	SpoolBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "devpod_gateway_audit_spool_bytes",
		Help: "Bytes waiting in the local audit spool.",
	})
	// UploadLag is the age of the oldest item waiting for upload.
	UploadLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "devpod_gateway_audit_upload_lag_seconds",
		Help: "Age of the oldest spooled audit item not yet uploaded.",
	})
	// GapsTotal counts raw records dropped because the spool was full.
	GapsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "devpod_gateway_audit_gaps_total",
		Help: "Raw audit records dropped (spool full).",
	})
	// DecoderErrorsTotal counts protocol decoders that failed.
	DecoderErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "devpod_gateway_audit_decoder_errors_total",
		Help: "Protocol decoders disabled after an error.",
	})
	// PolicyActionsTotal counts policy enforcements.
	PolicyActionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "devpod_gateway_audit_policy_actions_total",
		Help: "Audit policy enforcements.",
	}, []string{"rule", "action"})
)

// MustRegisterMetrics registers the audit metrics on reg.
func MustRegisterMetrics(reg prometheus.Registerer) {
	reg.MustRegister(SpoolBytes, UploadLag, GapsTotal, DecoderErrorsTotal, PolicyActionsTotal)
}
