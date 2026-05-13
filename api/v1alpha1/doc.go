// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains the v1alpha1 API types for DevPod.
//
// +kubebuilder:object:generate=true
// +groupName=devpod.io
package v1alpha1

//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 object:headerFile=../../hack/boilerplate.go.txt paths=./...
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 crd paths=./... output:crd:dir=../../config/crd/bases
//go:generate bash ../../hack/sync-crd-chart.sh
