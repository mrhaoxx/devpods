// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"path/filepath"
	"testing"
	"time"
)

func rdmaFixture(t *testing.T) (dev, sys string) {
	dev = writeTree(t, map[string]string{"infiniband/uverbs0": ""})
	sys = writeTree(t, map[string]string{
		"class/infiniband_verbs/uverbs0/ibdev":                        "mlx5_0\n",
		"class/infiniband_verbs/uverbs1/ibdev":                        "mlx5_1\n",
		"class/infiniband/mlx5_0/device/physfn":                       "",
		"class/infiniband/mlx5_0/ports/1/counters/port_xmit_data":     "100\n",
		"class/infiniband/mlx5_0/ports/1/counters/port_rcv_data":      "50\n",
		"class/infiniband/mlx5_0/ports/1/counters/port_xmit_packets":  "10\n",
		"class/infiniband/mlx5_0/ports/1/counters/port_rcv_packets":   "5\n",
		"class/infiniband/mlx5_0/ports/1/hw_counters/np_cnp_sent":     "3\n",
		"class/infiniband/mlx5_0/ports/1/hw_counters/out_of_sequence": "1\n",
		"class/infiniband/mlx5_1/ports/1/counters/port_xmit_data":     "7\n",
	})
	return dev, sys
}

func TestRDMA_DiscoverOnlyContainerDevices(t *testing.T) {
	dev, sys := rdmaFixture(t)
	devs, err := DiscoverRDMA(dev, sys)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].IBDev != "mlx5_0" || len(devs[0].Ports) != 1 || devs[0].Ports[0] != 1 {
		t.Fatalf("only the device behind /dev/infiniband/uverbs0: %+v", devs)
	}
}

func TestRDMA_ScopeAndBytes(t *testing.T) {
	dev, sys := rdmaFixture(t)
	devs, _ := DiscoverRDMA(dev, sys)
	d := devs[0]
	if d.Scope != "vf_exclusive" {
		t.Fatalf("physfn present → vf_exclusive, got %q", d.Scope)
	}
	_, c, err := d.Sample(sys, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, sys, filepath.Join("class/infiniband/mlx5_0/ports/1/counters/port_xmit_data"), "350\n")
	writeTreeFile(t, sys, filepath.Join("class/infiniband/mlx5_0/ports/1/counters/port_xmit_packets"), "30\n")
	s, _, err := d.Sample(sys, &c, time.Second)
	if err != nil || len(s) != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	if s[0].TxBps != 1000 || s[0].RxBps != 0 || s[0].TxPps != 20 || s[0].Health["np_cnp_sent"] != 3 || s[0].Scope != "vf_exclusive" || s[0].Port != 1 {
		t.Fatalf("%+v", s[0])
	}
	// no physfn → pf_shared
	d.Scope = ""
	if scope := rdmaScope(sys, "mlx5_1"); scope != "pf_shared" {
		t.Fatalf("got %q", scope)
	}
}

func TestRDMA_NoDevices(t *testing.T) {
	devs, err := DiscoverRDMA(t.TempDir(), t.TempDir())
	if err != nil || len(devs) != 0 {
		t.Fatalf("%+v %v", devs, err)
	}
}
