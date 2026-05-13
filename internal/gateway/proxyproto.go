// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// WrapProxyProtocolListener returns a net.Listener that expects a
// PROXY protocol v2 header on connections originating from trusted
// IPs. Connections from non-trusted sources are REJECTed (closed at
// accept time) — there is no fallback to raw TCP because that lets
// attackers bypass the trust boundary by simply not sending a header.
//
// readHeaderTimeout caps how long the wrapper waits for the PROXY
// header bytes.
func WrapProxyProtocolListener(inner net.Listener, trusted []*net.IPNet, readHeaderTimeout time.Duration) net.Listener {
	return &proxyproto.Listener{
		Listener:          inner,
		ReadHeaderTimeout: readHeaderTimeout,
		ConnPolicy: func(opts proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			ip := remoteIP(opts.Upstream)
			for _, n := range trusted {
				if n.Contains(ip) {
					return proxyproto.USE, nil
				}
			}
			return proxyproto.REJECT, nil
		},
	}
}

func remoteIP(addr net.Addr) net.IP {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.IP
	case *net.UDPAddr:
		return a.IP
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return nil
		}
		return net.ParseIP(host)
	}
}
