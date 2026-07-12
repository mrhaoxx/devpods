// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package webui implements the DevPod web UI backend: OAuth login,
// session cookies, template-mediated DevPod CRUD, quota enforcement,
// and the JSON API consumed by the embedded SPA.
package webui

import (
	"fmt"
	"regexp"
)

// MaxDevPodNameLen mirrors the CEL length cap on DevPod names.
const MaxDevPodNameLen = 22

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// MapUsername maps a GitLab username to the DevPod user name by
// prepending the configured prefix. The result must be a valid
// DNS-1123 label AND leave at least one character of DevPod name
// budget; otherwise login is refused with an explicit error.
func MapUsername(prefix, gitlabUsername string) (string, error) {
	if gitlabUsername == "" {
		return "", fmt.Errorf("empty GitLab username")
	}
	name := prefix + gitlabUsername
	if !dns1123Label.MatchString(name) {
		return "", fmt.Errorf("mapped username %q is not a valid DNS-1123 label (lowercase alphanumerics and '-' only)", name)
	}
	if NameBudget(name) < 1 {
		return "", fmt.Errorf("mapped username %q leaves no room for DevPod names (limit %d chars incl. %q prefix)", name, MaxDevPodNameLen, name+"-")
	}
	return name, nil
}

// NameBudget returns how many characters remain for the user-chosen
// DevPod name suffix after "<owner>-".
func NameBudget(owner string) int {
	return MaxDevPodNameLen - len(owner) - 1
}
