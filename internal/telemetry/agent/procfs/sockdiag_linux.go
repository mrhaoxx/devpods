// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procfs

import (
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type sockDiag struct{}

// NewSockDiag reads TCP socket counters with NETLINK_SOCK_DIAG (INET_DIAG_INFO)
// in the caller's network namespace.
func NewSockDiag() TCPStats { return sockDiag{} }

func (sockDiag) Bytes() (map[uint64]TCPBytes, error) {
	out := map[uint64]TCPBytes{}
	for _, fam := range []uint8{unix.AF_INET, unix.AF_INET6} {
		resp, err := netlink.SocketDiagTCPInfo(fam)
		if err != nil {
			return nil, err
		}
		for _, r := range resp {
			if r.InetDiagMsg == nil || r.TCPInfo == nil ||
				!CountedTCP(r.InetDiagMsg.ID.Source, r.InetDiagMsg.ID.Destination) {
				continue
			}
			out[uint64(r.InetDiagMsg.INode)] = TCPBytes{Acked: r.TCPInfo.Bytes_acked, Received: r.TCPInfo.Bytes_received}
		}
	}
	return out, nil
}
