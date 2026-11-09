// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package procfs

import "errors"

type sockDiag struct{}

// NewSockDiag is Linux-only; elsewhere (developer laptops running unit
// tests) it reports an error and per-process TCP is simply absent.
func NewSockDiag() TCPStats { return sockDiag{} }

func (sockDiag) Bytes() (map[uint64]TCPBytes, error) {
	return nil, errors.New("sock_diag is only available on linux")
}
