// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"sort"
	"strings"
)

// KorePrefix is the annotation namespace of the Kore CPU-pinning
// system (github.com/zjusct/kore). Only admins write these annotations,
// in a DevPod's YAML; anyone else's submission carrying them is refused.
const KorePrefix = "kore.zjusct.io/"

// KoreAnnotationKeys returns the sorted kore.zjusct.io/* keys present
// in ann. Non-admin create/patch handlers reject any submission where
// this is non-empty.
func KoreAnnotationKeys(ann map[string]string) []string {
	var out []string
	for k := range ann {
		if strings.HasPrefix(k, KorePrefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
