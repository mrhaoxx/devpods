// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package crdschema_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrhaoxx/devpod/internal/crdschema"
)

func load(t *testing.T) map[string]any {
	t.Helper()
	crd, err := os.ReadFile(filepath.Join("..", "..", "deploy", "chart", "crds", "devpod.io_devpods.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := crdschema.DevPod(crd)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func at(t *testing.T, s map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := s
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("no %v in the schema (stopped at %q)", path, p)
		}
		cur = next
	}
	return cur
}

func TestDevPod(t *testing.T) {
	s := load(t)
	props := at(t, s, "properties")
	// what a DevPod is, said where the CRD says only "a string" or "an object"
	if e, _ := at(t, props, "kind")["enum"].([]any); len(e) != 1 || e[0] != "DevPod" {
		t.Fatalf("kind: %v", props["kind"])
	}
	if e, _ := at(t, props, "apiVersion")["enum"].([]any); len(e) != 1 || e[0] != "devpod.io/v1alpha1" {
		t.Fatalf("apiVersion: %v", props["apiVersion"])
	}
	if _, ok := at(t, props, "metadata", "properties")["name"]; !ok {
		t.Fatal("metadata has no name")
	}
	// status is the controller's to write
	if _, ok := props["status"]; ok {
		t.Fatal("status is offered for editing")
	}
	// descriptions are kept: they are what a hover shows
	if d, _ := at(t, props, "spec", "properties", "owner")["description"].(string); d == "" {
		t.Fatal("spec.owner lost its description")
	}
	// a field that does not exist is pointed out: the API server would drop it without a word
	if at(t, props, "spec")["additionalProperties"] != false {
		t.Fatal("unknown fields of spec are allowed")
	}
	container := at(t, props, "spec", "properties", "pod", "properties", "spec", "properties", "containers", "items")
	if container["additionalProperties"] != false {
		t.Fatal("unknown fields of a container are allowed")
	}
	// …but maps stay maps, and free-form objects stay free
	limits := at(t, container, "properties", "resources", "properties", "limits")
	if _, ok := limits["additionalProperties"].(map[string]any); !ok {
		t.Fatalf("resources.limits is not a map any more: %v", limits["additionalProperties"])
	}
	labels := at(t, props, "metadata", "properties", "labels")
	if _, ok := labels["additionalProperties"].(map[string]any); !ok {
		t.Fatal("metadata.labels is not a map")
	}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if v["x-kubernetes-preserve-unknown-fields"] == true && v["additionalProperties"] == false {
				t.Fatalf("a field that keeps unknown fields refuses them: %v", v)
			}
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(s)
}

// The schema the console's YAML editor uses is generated from the CRD:
// it must be the CRD as it is now.
func TestWebCopyIsCurrent(t *testing.T) {
	crd, err := os.ReadFile(filepath.Join("..", "..", "deploy", "chart", "crds", "devpod.io_devpods.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := crdschema.DevPod(crd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "design", "yaml", "devpod.schema.json"))
	if err != nil {
		t.Fatalf("%v (go run ./hack/webschema)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Fatal("web/src/design/yaml/devpod.schema.json is not the CRD's schema: go run ./hack/webschema")
	}
}
