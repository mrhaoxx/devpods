// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"context"
	"errors"
)

func newContainerdBackend(context.Context, options, *emitter) (backend, error) {
	return nil, errors.New("the containerd backend is Linux-only")
}
