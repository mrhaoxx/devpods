// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the API group / version this package serves.
var GroupVersion = schema.GroupVersion{Group: "devpod.io", Version: "v1alpha1"}

// SchemeBuilder collects the types for registration.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme registers DevPod types with a runtime.Scheme.
var AddToScheme = SchemeBuilder.AddToScheme
