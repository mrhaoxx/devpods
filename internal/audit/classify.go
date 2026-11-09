// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"path"
	"slices"
	"strings"
)

// IsOpaqueTransfer flags exec commands that typically move file content
// over the channel in a form the decoders cannot parse (rsync's server
// protocol, tar/dd/base64 streams, `cat > file`). Their bytes are still
// in the raw stream; this only labels them for review.
func IsOpaqueTransfer(cmd string) bool {
	args := strings.Fields(cmd)
	if len(args) == 0 {
		return false
	}
	switch path.Base(args[0]) {
	case "rsync":
		return slices.Contains(args[1:], "--server")
	case "tar", "dd", "base64":
		return true
	case "cat":
		return strings.Contains(cmd, ">")
	}
	return false
}
