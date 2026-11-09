// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func sampleRecords(ts time.Time) []Record {
	attr := &Attr{ConnPid: 50, ConnStart: 900, How: "tree"}
	return []Record{
		{K: KContainer, C: &Container{TS: ts, CPU: 1.5, CPULimit: 8, MemWorkingSet: 1 << 30, NetKind: "pod", NetRx: 1000}},
		{K: KGPU, G: &GPU{TS: ts, UUID: "GPU-ab", Name: "A100", Util: 97, MemUsed: 37000, MemTotal: 81920}},
		{K: KRDMA, R: &RDMA{TS: ts, IBDev: "mlx5_0", Port: 1, TxBps: 3e9, Scope: "vf_exclusive", Health: map[string]uint64{"np_cnp_sent": 3}}},
		{K: KProc, P: &Proc{TS: ts, Pid: 4121, Start: 1234, CPU: 6.8, RSS: 18 << 30, GPU: []int{0, 1}, Attr: attr}},
		{K: KProcEvent, PE: &ProcEvent{TS: ts, Event: "process_start", Pid: 4121, Start: 1234, PPid: 50, Exe: "/usr/bin/python3", Cmdline: "python train.py", Attr: attr}},
		{K: KSSHConn, SC: &SSHConn{TS: ts, Pid: 50, Start: 900, Peer: "10.78.3.4:41000"}},
		{K: KCommand, Cmd: &Command{TS: ts, EventID: "e1", ShellPid: 77, ShellStart: 5, TTY: "pts/0", Cwd: "/tmp", Cmd: "false", End: ts.Add(time.Second), Exit: 1}},
		{K: KInfo, Info: &Info{TS: ts, OS: "Ubuntu 24.04", Kernel: "6.8.0", Collectors: map[string]string{"gpu": "ok"}}},
		{K: KSnapshot, Snap: &Snapshot{TS: ts, Procs: 37, Listen: []Listen{{Port: 8888, Proto: "tcp", Pid: 30}}}},
		{K: KFullProcs, Full: &FullProcs{TS: ts, Procs: []SnapProc{{Pid: 1, Exe: "/opt/devpod/devpod-supervisor"}}}},
		{K: KAlert, Alert: &Alert{TS: ts, Kind: "gap", Detail: "3 audit records dropped"}},
	}
}

func TestWire_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	h := Header{V: Version, DevPodUID: "uid-1", BootID: "01J", Seq: 7, TS: ts, AgentVersion: "v0.3.0"}
	recs := sampleRecords(ts)
	var buf bytes.Buffer
	if err := Encode(&buf, h, recs); err != nil {
		t.Fatal(err)
	}
	gh, got, err := Decode(&buf, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if gh != h {
		t.Fatalf("header %+v != %+v", gh, h)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("records differ:\n got %+v\nwant %+v", got, recs)
	}
	kinds := map[Kind]bool{}
	for _, r := range got {
		kinds[r.K] = true
	}
	if len(kinds) != 11 {
		t.Fatalf("want all 11 kinds, got %d", len(kinds))
	}
}

func TestWire_DecodeRejectsOversize(t *testing.T) {
	ts := time.Now().UTC()
	var recs []Record
	for i := 0; i < 50000; i++ {
		recs = append(recs, Record{K: KAlert, Alert: &Alert{TS: ts, Kind: "x", Detail: strings.Repeat("y", 100)}})
	}
	var buf bytes.Buffer
	if err := Encode(&buf, Header{V: Version, DevPodUID: "u"}, recs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(&buf, 4<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestWire_UnknownKindIgnored(t *testing.T) {
	var raw bytes.Buffer
	zw, _ := zstd.NewWriter(&raw)
	_, _ = zw.Write([]byte(`{"v":1,"devpod_uid":"u","boot_id":"b","seq":1,"ts":"2026-11-09T08:00:00Z"}` + "\n" +
		`{"k":"zz","zz":{"a":1}}` + "\n" +
		`{"k":"alert","alert":{"ts":"2026-11-09T08:00:00Z","kind":"gap","detail":"d"}}` + "\n"))
	_ = zw.Close()
	_, recs, err := Decode(&raw, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].K != KAlert {
		t.Fatalf("unknown kind must be skipped, got %+v", recs)
	}
}

func TestKind_Audit(t *testing.T) {
	for k, want := range map[Kind]bool{KProcEvent: true, KSSHConn: true, KCommand: true, KAlert: true, KContainer: false, KProc: false, KFullProcs: false, KInfo: false} {
		if k.Audit() != want {
			t.Errorf("%s.Audit() = %v", k, !want)
		}
	}
}

func TestToken_StableAndKeyed(t *testing.T) {
	a := Token([]byte("k1"), "uid-1")
	if a != Token([]byte("k1"), "uid-1") || a == Token([]byte("k2"), "uid-1") || a == Token([]byte("k1"), "uid-2") {
		t.Fatal("token must depend on key and uid only")
	}
	if len(a) != 64 {
		t.Fatalf("hex sha256, got %d chars", len(a))
	}
}
