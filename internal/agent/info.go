// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"time"
)

// Info is what a cluster tells the console about itself.
type Info struct {
	Cluster           string   `json:"cluster"`
	AgentVersion      string   `json:"agentVersion,omitempty"`
	ControllerVersion string   `json:"controllerVersion,omitempty"`
	KubernetesVersion string   `json:"kubernetesVersion,omitempty"`
	Capabilities      []string `json:"capabilities,omitempty"`
}

// serveInfo writes the cluster's Info at once and again whenever it
// changes, until the console closes the stream.
func (a *Agent) serveInfo(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	// the console sends nothing: a read ends when it hangs up
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		buf := make([]byte, 1)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
	current := func() Info {
		var in Info
		if a.o.Info != nil {
			in = a.o.Info(ctx)
		}
		in.Cluster = a.o.Cluster
		return in
	}
	enc := json.NewEncoder(conn)
	last := current()
	if err := enc.Encode(last); err != nil {
		return
	}
	tick := time.NewTicker(a.o.InfoInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if now := current(); !reflect.DeepEqual(now, last) {
				if err := enc.Encode(now); err != nil {
					return
				}
				last = now
			} else if _, err := conn.Write([]byte("\n")); err != nil {
				return // nothing new: a blank line, so that nothing in between takes the stream for dead
			}
		case <-gone:
			return
		case <-ctx.Done():
			return
		}
	}
}
