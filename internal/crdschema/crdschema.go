// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package crdschema turns the DevPod CRD's schema into the JSON schema the
// console's YAML editor checks a manifest against, offers completions
// from and shows field descriptions of.
package crdschema

import (
	"encoding/json"
	"errors"
	"fmt"

	"sigs.k8s.io/yaml"
)

// DevPod is the JSON schema of a DevPod manifest, from the CRD (its
// YAML). Next to the CRD's own schema it says what apiVersion, kind and
// metadata are, leaves out status (the controller's), and refuses fields
// that do not exist — where the API server would drop them without a
// word, the editor points them out.
func DevPod(crd []byte) ([]byte, error) {
	var doc struct {
		Spec struct {
			Versions []struct {
				Name    string `json:"name"`
				Storage bool   `json:"storage"`
				Schema  struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
			Group string `json:"group"`
			Names struct {
				Kind string `json:"kind"`
			} `json:"names"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(crd, &doc); err != nil {
		return nil, fmt.Errorf("read the CRD: %w", err)
	}
	var schema map[string]any
	version := ""
	for _, v := range doc.Spec.Versions {
		if v.Storage {
			schema, version = v.Schema.OpenAPIV3Schema, v.Name
		}
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, errors.New("the CRD has no schema with properties")
	}
	delete(props, "status")
	props["apiVersion"] = map[string]any{"type": "string", "enum": []any{doc.Spec.Group + "/" + version},
		"description": "The API version of a " + doc.Spec.Names.Kind + "."}
	props["kind"] = map[string]any{"type": "string", "enum": []any{doc.Spec.Names.Kind}}
	props["metadata"] = metadata()
	closeObjects(schema)
	schema["title"] = doc.Spec.Names.Kind
	return json.Marshal(schema)
}

// metadata is what of an object's metadata a manifest sets; the rest the
// server fills in, and is let through.
func metadata() map[string]any {
	str := map[string]any{"type": "string"}
	strMap := func(d string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": str, "description": d}
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": true,
		"required":             []any{"name"},
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "The DevPod's name: lower-case letters, digits and hyphens. The console names what it creates <owner>-<name>."},
			"namespace":   map[string]any{"type": "string", "description": "Set by the console."},
			"labels":      strMap("Labels: string keys and values."),
			"annotations": strMap("Annotations: string keys and values."),
		},
	}
}

// closeObjects refuses unknown fields in every object that lists its
// fields, unless it says it keeps unknown fields.
func closeObjects(n any) {
	switch v := n.(type) {
	case map[string]any:
		_, listed := v["properties"]
		_, said := v["additionalProperties"]
		if listed && !said && v["x-kubernetes-preserve-unknown-fields"] != true {
			v["additionalProperties"] = false
		}
		for _, c := range v {
			closeObjects(c)
		}
	case []any:
		for _, c := range v {
			closeObjects(c)
		}
	}
}
