// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gpures

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
)

// ModelLabel: an admin's word for a machine's cards, where no feature
// discovery labels them (or labels them wrong).
const ModelLabel = "devpod.io/gpu-model"

// Models: the models of n's cards (nil: it has none of a known type) —
// from the first that says: an admin's label; the whole machine's product
// label (<domain>/gpu.product: NVIDIA's feature discovery); each card's
// (<domain>/gpu<N>.product: Moore Threads'), the distinct ones sorted;
// else the vendor. <domain> is a GPU type's resource's.
func Models(n *corev1.Node) []string {
	var vendor string
	for _, t := range Types() {
		if hasCards(n.Status.Capacity, t.Resource) || hasCards(n.Status.Allocatable, t.Resource) {
			vendor = t.Label
			break
		}
	}
	if vendor == "" {
		return nil
	}
	l := n.Labels
	if m := strings.TrimSpace(l[ModelLabel]); m != "" {
		return []string{m}
	}
	domains := typeDomains()
	for _, d := range domains {
		if m := l[d+"/gpu.product"]; m != "" {
			return []string{m}
		}
	}
	for _, d := range domains {
		var per []string
		for k, v := range l {
			if v != "" && strings.HasPrefix(k, d+"/") && perCard.MatchString(k[len(d)+1:]) && !slices.Contains(per, v) {
				per = append(per, v)
			}
		}
		if len(per) > 0 {
			slices.Sort(per)
			return per
		}
	}
	return []string{vendor}
}

var perCard = regexp.MustCompile(`^gpu[0-9]+\.product$`)

func hasCards(l corev1.ResourceList, r corev1.ResourceName) bool {
	q, ok := l[r]
	return ok && !q.IsZero()
}

// typeDomains: the GPU types' resources' domains (nvidia.com …), once each.
func typeDomains() []string {
	var out []string
	for _, t := range Types() {
		d, _, _ := strings.Cut(string(t.Resource), "/")
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// ModelMatches: want, a model as people write it (a100, rtx-4090,
// MTTS5000), names every one of models (a machine's: of mixed cards, a
// rank could get any). Both are taken in words — lowercased, split at
// what is not a letter or digit — and each word of want must be a word of
// the model or adjacent ones joined, in any order: a100 is either A100,
// a100-40gb the A100-PCIE-40GB, rtx4090 the RTX-4090; a10 is no A100.
func ModelMatches(want string, models []string) bool {
	ws := modelWords(want)
	if len(ws) == 0 || len(models) == 0 {
		return false
	}
	for _, m := range models {
		mw := modelWords(m)
		for _, w := range ws {
			if !joinsTo(mw, w) {
				return false
			}
		}
	}
	return true
}

func modelWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// joinsTo: some run of adjacent words of ws, joined, is w.
func joinsTo(ws []string, w string) bool {
	for i := range ws {
		joined := ""
		for _, x := range ws[i:] {
			joined += x
			if joined == w {
				return true
			}
			if len(joined) >= len(w) {
				break
			}
		}
	}
	return false
}
