// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// The controller says what it can do in a ConfigMap of the DevPod
// namespace, written when it starts: a cluster takes queue jobs only
// where the controller running there runs job ranks (an older one would
// keep restarting them), and a job's working directory must be on
// storage the machines share.
const (
	// ControllerFeaturesConfigMap: the ConfigMap's name.
	ControllerFeaturesConfigMap = "devpod-controller"
	// ControllerVersionKey: the version of the controller that wrote it
	// (its image's tag): one that is not the one running says nothing.
	ControllerVersionKey = "version"
	// ControllerFeaturesKey: what it can do, comma separated.
	ControllerFeaturesKey = "features"
	// ControllerSharedHomesKey: where the owners' shared home directories
	// are mounted (<it>/<owner>); absent when homes are not shared.
	ControllerSharedHomesKey = "sharedHomes"
	// FeatureJobs: the controller runs queue jobs' ranks.
	FeatureJobs = "jobs"
)
