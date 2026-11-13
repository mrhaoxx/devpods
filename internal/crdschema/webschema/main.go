// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// webschema writes the JSON schema of a DevPod manifest, from the CRD,
// where the console's YAML editor reads it:
//
//	go run ./internal/crdschema/webschema   (from the repository root; also run by go generate in api/v1alpha1)
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mrhaoxx/devpod/internal/crdschema"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	crd, err := os.ReadFile(filepath.Join(root, "deploy", "chart", "crds", "devpod.io_devpods.yaml"))
	if err == nil {
		var out []byte
		if out, err = crdschema.DevPod(crd); err == nil {
			err = os.WriteFile(filepath.Join(root, "web", "src", "design", "yaml", "devpod.schema.json"), append(out, '\n'), 0o644)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "webschema:", err)
		os.Exit(1)
	}
}
