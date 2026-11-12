// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui_test

import (
	"testing"

	"github.com/mrhaoxx/devpod/internal/webui"
)

func TestKoreAnnotationKeys(t *testing.T) {
	got := webui.KoreAnnotationKeys(map[string]string{
		"kore.zjusct.io/pin":  "true",
		"example.com/other":   "x",
		"kore.zjusct.io/pool": "team",
	})
	if len(got) != 2 || got[0] != "kore.zjusct.io/pin" || got[1] != "kore.zjusct.io/pool" {
		t.Fatalf("got %v", got)
	}
	if got := webui.KoreAnnotationKeys(map[string]string{"a": "b"}); len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
	if got := webui.KoreAnnotationKeys(nil); len(got) != 0 {
		t.Fatalf("want empty for nil, got %v", got)
	}
}
