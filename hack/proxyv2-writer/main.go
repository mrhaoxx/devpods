// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command proxyv2-writer dials a TCP address, writes a PROXY protocol
// v2 header claiming the configured source/destination, and then
// keeps the connection open briefly so the receiver can react.
//
// Used by hack/e2e-m3.sh to confirm the gateway parses the spoofed
// client IP out of the v2 header.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

func main() {
	var addr, src string
	flag.StringVar(&addr, "addr", "", "gateway addr (host:port)")
	flag.StringVar(&src, "src", "1.2.3.4:5678", "spoofed source addr (host:port)")
	flag.Parse()
	if addr == "" {
		fmt.Fprintln(os.Stderr, "missing -addr")
		os.Exit(2)
	}

	c, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer c.Close()

	srcHost, srcPort, err := net.SplitHostPort(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse src:", err)
		os.Exit(2)
	}
	port, err := strconv.Atoi(srcPort)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse src port:", err)
		os.Exit(2)
	}

	hdr := &proxyproto.Header{
		Version: 2, Command: proxyproto.PROXY,
		TransportProtocol: proxyproto.TCPv4,
		SourceAddr:        &net.TCPAddr{IP: net.ParseIP(srcHost), Port: port},
		DestinationAddr:   &net.TCPAddr{IP: net.ParseIP(strings.Split(addr, ":")[0]), Port: 22},
	}
	if _, err := hdr.WriteTo(c); err != nil {
		fmt.Fprintln(os.Stderr, "write hdr:", err)
		os.Exit(1)
	}
	time.Sleep(300 * time.Millisecond)
}
