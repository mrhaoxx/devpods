// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package gateway implements the DevPod SSH gateway. In this milestone
// the package contains only the login-name parser; full SSH proxy logic
// lands in the next milestone.
package gateway

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidLoginName is the sentinel error returned by ParseLoginName
// when the input does not conform to "<user>+<pod>" where both
// components match `[a-z0-9-]{1,32}`.
var ErrInvalidLoginName = errors.New("invalid login name")

var loginRE = regexp.MustCompile(`^([a-z0-9-]{1,32})\+([a-z0-9-]{1,32})$`)

// ParseLoginName splits an SSH login user string into (devpodOwner,
// devpodName).
//
// The required form is `<owner>+<pod>` per the spec. Anything else
// (missing pod, empty parts, uppercase, traversal characters, trailing
// whitespace) returns ErrInvalidLoginName.
func ParseLoginName(s string) (owner, pod string, err error) {
	m := loginRE.FindStringSubmatch(s)
	if m == nil {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidLoginName, s)
	}
	return m[1], m[2], nil
}
