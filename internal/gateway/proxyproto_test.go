// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"net"
	"testing"
	"time"

	proxyproto "github.com/pires/go-proxyproto"

	"github.com/mrhaoxx/devpod/internal/gateway"
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func TestWrapProxyProtocol_ParsesV2FromTrustedSource(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	wrapped := gateway.WrapProxyProtocolListener(ln, []*net.IPNet{mustCIDR("127.0.0.0/8")}, 2*time.Second)
	defer wrapped.Close()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Errorf("dial: %v", err)
			return
		}
		defer c.Close()
		hdr := &proxyproto.Header{
			Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv4,
			SourceAddr:      &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 5678},
			DestinationAddr: &net.TCPAddr{IP: net.ParseIP("9.8.7.6"), Port: 22},
		}
		if _, err := hdr.WriteTo(c); err != nil {
			t.Errorf("WriteTo: %v", err)
			return
		}
		// Hold the conn so accept-side won't see EOF immediately.
		_ = c.SetDeadline(time.Now().Add(time.Second))
		_, _ = c.Read(make([]byte, 1))
	}()

	server, err := wrapped.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer server.Close()
	got := server.RemoteAddr().String()
	if got != "1.2.3.4:5678" {
		t.Errorf("RemoteAddr = %q, want 1.2.3.4:5678", got)
	}
	<-clientDone
}

func TestWrapProxyProtocol_RejectsUntrustedSource(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// Trust ONLY 10.0.0.0/8; the test client connects via 127.0.0.1 -> not trusted.
	wrapped := gateway.WrapProxyProtocolListener(ln, []*net.IPNet{mustCIDR("10.0.0.0/8")}, 2*time.Second)
	defer wrapped.Close()

	go func() {
		c, _ := net.Dial("tcp", ln.Addr().String())
		if c != nil {
			_ = c.Close()
		}
	}()

	// proxyproto's REJECT closes the conn — either Accept errors, or yields
	// a conn that fails fast on read.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := wrapped.Accept()
		if err != nil {
			return // rejected at accept — pass
		}
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		b := make([]byte, 1)
		if _, rerr := c.Read(b); rerr != nil {
			c.Close()
			return // closed on read — pass
		}
		c.Close()
	}
	t.Fatal("untrusted source was neither rejected nor immediately closed")
}
