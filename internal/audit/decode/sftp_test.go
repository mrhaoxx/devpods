// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package decode

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
)

func TestSFTP_PutGetFixture(t *testing.T) {
	recs, want := loadFixture(t, "sftp-put-get")
	for _, chunk := range []int{1, 7, 4096, 65536} {
		t.Run(fmt.Sprint("chunk=", chunk), func(t *testing.T) {
			sink := &fakeSink{}
			d := NewSFTP(policy.Effective{}, sink)
			f := feedRecords(d, recs, chunk)
			d.Close(true)

			assertFilesMatch(t, sink, want)
			for dir := 0; dir < 2; dir++ {
				if !bytes.Equal(f.pass[dir], f.in[dir]) {
					t.Fatalf("dir %d: pass-through altered the stream (%d in, %d out)", dir, len(f.in[dir]), len(f.pass[dir]))
				}
				if len(f.reply[dir]) != 0 {
					t.Fatalf("dir %d: unexpected reply bytes", dir)
				}
			}
			ops := map[string]Event{}
			for _, e := range sink.eventsOf(EventFileOp) {
				ops[e.Op] = e
			}
			for _, op := range []string{"mkdir", "rename", "remove", "rmdir"} {
				if _, ok := ops[op]; !ok {
					t.Errorf("missing %s op event; got %v", op, ops)
				}
			}
			if ops["rename"].Path2 == "" {
				t.Error("rename must carry the target path")
			}
		})
	}
}

func TestSFTP_PipelinedFixture(t *testing.T) {
	recs, want := loadFixture(t, "sftp-pipelined")
	sink := &fakeSink{}
	d := NewSFTP(policy.Effective{}, sink).(*sftpDecoder)
	var in, pass [2]int
	for _, r := range recs {
		dir, ok := fixtureDir(r.Kind)
		if !ok {
			continue
		}
		res := d.Feed(dir, r.Payload)
		in[dir] += len(r.Payload)
		pass[dir] += len(res.Pass)
		carried := d.carry(dir)
		if pass[dir]+carried != in[dir] {
			t.Fatalf("dir %d: bytes not conserved: in %d pass %d carry %d", dir, in[dir], pass[dir], carried)
		}
		if carried > 5<<10 {
			t.Fatalf("dir %d: decoder holds %d bytes; must never buffer more than one ~4 KiB packet", dir, carried)
		}
	}
	d.Close(true)
	assertFilesMatch(t, sink, want)
}

func TestSFTP_DenyDownload(t *testing.T) {
	for _, flags := range []uint32{fxfRead, fxfRead | fxfWrite} {
		sink := &fakeSink{}
		d := NewSFTP(policy.Effective{DenyDownload: true}, sink)
		res := d.Feed(ClientToBackend, sftpOpen(7, "/etc/passwd", flags))
		if len(res.Pass) != 0 {
			t.Fatalf("flags %#x: denied OPEN was forwarded", flags)
		}
		id, code, msg := parseStatus(t, res.Reply)
		if id != 7 || code != fxPermissionDenied || msg != MsgDownloadDenied {
			t.Fatalf("flags %#x: status id=%d code=%d msg=%q", flags, id, code, msg)
		}
		pol := sink.eventsOf(EventPolicy)
		if len(pol) != 1 || pol[0].Rule != "download" || pol[0].Action != "deny" || pol[0].Path != "/etc/passwd" {
			t.Fatalf("flags %#x: policy events %+v", flags, pol)
		}
	}
}

func TestSFTP_DenyUpload(t *testing.T) {
	sink := &fakeSink{}
	d := NewSFTP(policy.Effective{DenyUpload: true}, sink)
	res := d.Feed(ClientToBackend, sftpOpen(9, "/tmp/x", fxfWrite|fxfCreat|fxfTrunc))
	if len(res.Pass) != 0 {
		t.Fatal("denied OPEN was forwarded")
	}
	id, code, msg := parseStatus(t, res.Reply)
	if id != 9 || code != fxPermissionDenied || msg != MsgUploadDenied {
		t.Fatalf("status id=%d code=%d msg=%q", id, code, msg)
	}
	if pol := sink.eventsOf(EventPolicy); len(pol) != 1 || pol[0].Rule != "upload" {
		t.Fatalf("policy events %+v", pol)
	}
	// A read-only open is still allowed.
	if res := d.Feed(ClientToBackend, sftpOpen(10, "/tmp/y", fxfRead)); len(res.Pass) == 0 || len(res.Reply) != 0 {
		t.Fatal("read-only open must pass when only uploads are denied")
	}
}

func TestSFTP_MaxFileSize(t *testing.T) {
	sink := &fakeSink{}
	d := NewSFTP(policy.Effective{MaxFileSize: 1024}, sink)
	d.Feed(ClientToBackend, sftpOpen(1, "/tmp/up", fxfWrite|fxfCreat))
	d.Feed(BackendToClient, sftpHandle(1, "H1"))
	ok := sftpWrite(2, "H1", 0, bytes.Repeat([]byte("a"), 1000))
	if res := d.Feed(ClientToBackend, ok); !bytes.Equal(res.Pass, ok) || len(res.Reply) != 0 {
		t.Fatal("write within the limit must pass")
	}
	res := d.Feed(ClientToBackend, sftpWrite(3, "H1", 1000, bytes.Repeat([]byte("b"), 100)))
	if len(res.Pass) != 0 {
		t.Fatal("oversize write was forwarded")
	}
	if id, code, msg := parseStatus(t, res.Reply); id != 3 || code != fxFailure || msg != MsgFileTooLarge {
		t.Fatalf("status id=%d code=%d msg=%q", id, code, msg)
	}

	d.Feed(ClientToBackend, sftpOpen(4, "/tmp/down", fxfRead))
	d.Feed(BackendToClient, sftpHandle(4, "H2"))
	res = d.Feed(ClientToBackend, sftpRead(5, "H2", 1000, 100))
	if len(res.Pass) != 0 {
		t.Fatal("oversize read was forwarded")
	}
	if id, code, msg := parseStatus(t, res.Reply); id != 5 || code != fxFailure || msg != MsgFileTooLarge {
		t.Fatalf("status id=%d code=%d msg=%q", id, code, msg)
	}
	var rules []string
	for _, e := range sink.eventsOf(EventPolicy) {
		rules = append(rules, e.Rule)
	}
	if fmt.Sprint(rules) != "[maxFileSize maxFileSize]" {
		t.Fatalf("policy rules %v", rules)
	}
}

func TestSFTP_TruncatedTransfer(t *testing.T) {
	recs, _ := loadFixture(t, "sftp-put-get")
	// Cut the stream once ~half of the 1 MiB upload went client→backend.
	var cut []rawlog.Record
	in := 0
	for _, r := range recs {
		cut = append(cut, r)
		if r.Kind == rawlog.KindDataIn {
			in += len(r.Payload)
			if in > 512<<10 {
				break
			}
		}
	}
	sink := &fakeSink{}
	d := NewSFTP(policy.Effective{}, sink)
	feedRecords(d, cut, 65536)
	d.Close(false)
	if len(sink.files) != 1 || sink.files[0].meta.Direction != "upload" {
		t.Fatalf("want the one partial upload, got %d files", len(sink.files))
	}
	if f := sink.files[0]; !f.finished || f.complete {
		t.Fatalf("partial upload finished=%v complete=%v", f.finished, f.complete)
	}
}

func TestSFTP_OversizePacketDisablesDecoder(t *testing.T) {
	sink := &fakeSink{}
	d := NewSFTP(policy.Effective{DenyDownload: true}, sink)
	head := []byte{0x00, 0x20, 0x00, 0x00, fxpOpen, 'a', 'b', 'c'} // claims 2 MiB
	res := d.Feed(ClientToBackend, head)
	if !bytes.Equal(res.Pass, head) {
		t.Fatalf("disabled decoder must pass bytes through, got %x", res.Pass)
	}
	if errs := sink.eventsOf(EventDecoderError); len(errs) != 1 {
		t.Fatalf("decoder errors %+v", errs)
	}
	later := sftpOpen(1, "/etc/passwd", fxfRead)
	if res := d.Feed(ClientToBackend, later); !bytes.Equal(res.Pass, later) {
		t.Fatal("after disabling, everything passes through verbatim")
	}
}
