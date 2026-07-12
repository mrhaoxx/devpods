// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// KorePrefix is the annotation namespace of the Kore CPU-pinning
// system (github.com/zjusct/kore). Non-admin DevPod submissions must
// never carry these annotations; they are stamped server-side from
// DevPodTemplates only.
const KorePrefix = "kore.zjusct.io/"

const (
	annPin          = KorePrefix + "pin"
	annPool         = KorePrefix + "pool"
	annPoolSize     = KorePrefix + "pool-size"
	annNUMAPolicy   = KorePrefix + "numa-policy"
	annMemoryPolicy = KorePrefix + "memory-policy"
	annPlacement    = KorePrefix + "placement"
	annSMTPolicy    = KorePrefix + "smt-policy"
)

// allowedBindingValues is the template whitelist: key → allowed values
// (nil = any non-empty value). kore.zjusct.io/cpuset is deliberately
// absent — explicit core numbers only make sense pinned to a node and
// stay an admin YAML escape hatch.
var allowedBindingValues = map[string][]string{
	annPin:          {"true"},
	annPool:         nil,
	annPoolSize:     nil, // validated as positive integer below
	annNUMAPolicy:   {"single", "preferred", "spread"},
	annMemoryPolicy: {"strict", "preferred"},
	annPlacement:    {"pack", "scatter"},
	annSMTPolicy:    {"full-core", "logical"},
}

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

// ValidateBinding mirrors Kore's admission rules for the template
// binding block, so authoring errors surface at template-save time
// (webui is NOT the real gate — Kore's webhook/scheduler is).
func ValidateBinding(b *devpodv1alpha1.BindingSpec) error {
	if b == nil {
		return fmt.Errorf("binding is nil")
	}
	for k, v := range b.Annotations {
		allowed, ok := allowedBindingValues[k]
		if !ok {
			return fmt.Errorf("annotation %q is not templatable (whitelist: pin, pool, pool-size, numa-policy, memory-policy, placement, smt-policy)", k)
		}
		if allowed == nil {
			if v == "" {
				return fmt.Errorf("annotation %q must not be empty", k)
			}
			continue
		}
		found := false
		for _, a := range allowed {
			if v == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("annotation %q: value %q not in %v", k, v, allowed)
		}
	}

	_, pin := b.Annotations[annPin]
	_, pool := b.Annotations[annPool]
	switch {
	case pin && pool:
		return fmt.Errorf("pin and pool are mutually exclusive")
	case !pin && !pool:
		return fmt.Errorf("binding must set %s or %s", annPin, annPool)
	}

	if pool {
		size, ok := b.Annotations[annPoolSize]
		if !ok {
			return fmt.Errorf("%s requires %s", annPool, annPoolSize)
		}
		if n, err := strconv.Atoi(size); err != nil || n < 1 {
			return fmt.Errorf("%s must be a positive integer, got %q", annPoolSize, size)
		}
	}

	if pin {
		cpu := b.Resources.Limits.Cpu()
		if cpu.IsZero() {
			return fmt.Errorf("pin binding requires an integer cpu limit")
		}
		if cpu.MilliValue()%1000 != 0 {
			return fmt.Errorf("pin binding requires integer cpu, got %s", cpu.String())
		}
		if req := b.Resources.Requests.Cpu(); !req.IsZero() && req.Cmp(*cpu) != 0 {
			return fmt.Errorf("pin binding requires cpu requests == limits (%s != %s)", req.String(), cpu.String())
		}
	}
	return nil
}
