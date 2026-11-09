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

func testZmodemFixture(t *testing.T, name string) {
	recs, want := loadFixture(t, name)
	for _, chunk := range []int{1, 3, 4096} {
		t.Run(fmt.Sprint("chunk=", chunk), func(t *testing.T) {
			sink := &fakeSink{}
			d := NewZmodem(policy.Effective{}, sink)
			f := feedRecords(d, recs, chunk)
			d.Close(true)
			assertFilesMatch(t, sink, want)
			for dir := 0; dir < 2; dir++ {
				if !bytes.Equal(f.pass[dir], f.in[dir]) {
					t.Fatalf("dir %d: stream altered without a policy (%d in, %d out)", dir, len(f.in[dir]), len(f.pass[dir]))
				}
			}
			if errs := sink.eventsOf(EventDecoderError); len(errs) != 0 {
				t.Fatalf("decoder errors: %+v", errs)
			}
		})
	}
}

func TestZmodem_SzFixture(t *testing.T) { testZmodemFixture(t, "zmodem-sz") }
func TestZmodem_RzFixture(t *testing.T) { testZmodemFixture(t, "zmodem-rz") }

func TestZmodem_PlainTextUntouched(t *testing.T) {
	sink := &fakeSink{}
	d := NewZmodem(policy.Effective{DenyDownload: true, DenyUpload: true}, sink)
	in := []byte("hello * world\r\n$ ls **/*.go\r\nTODO: **bold**\r\n")
	if res := d.Feed(BackendToClient, in); !bytes.Equal(res.Pass, in) || len(res.Reply) != 0 {
		t.Fatalf("pass %q reply %q", res.Pass, res.Reply)
	}
	if len(sink.files) != 0 || len(sink.events) != 0 {
		t.Fatal("plain text produced output")
	}
}

// Terminal output is almost never ZMODEM: chunks without ZPAD / ZDLE
// must pass through without being copied (the gateway feeds every pty
// byte through this decoder).
func TestZmodem_PlainChunkZeroCopy(t *testing.T) {
	d := NewZmodem(policy.Effective{DenyDownload: true}, &fakeSink{})
	in := bytes.Repeat([]byte("yes\r\n"), 700)
	if res := d.Feed(BackendToClient, in); len(res.Pass) != len(in) || &res.Pass[0] != &in[0] {
		t.Fatal("plain chunk was copied")
	}
	if n := testing.AllocsPerRun(50, func() { d.Feed(BackendToClient, in) }); n != 0 {
		t.Fatalf("%v allocs per plain chunk, want 0", n)
	}
}

// denyRun feeds a fixture record by record under pol and returns the
// result of the first B2C feed containing marker plus the totals.
func denyRun(t *testing.T, name string, pol policy.Effective, marker string) (first Result, pass [2][]byte, sink *fakeSink) {
	t.Helper()
	recs, _ := loadFixture(t, name)
	sink = &fakeSink{}
	d := NewZmodem(pol, sink)
	found := false
	for _, r := range recs {
		dir, ok := fixtureDir(r.Kind)
		if !ok {
			continue
		}
		res := d.Feed(dir, r.Payload)
		pass[dir] = append(pass[dir], res.Pass...)
		if !found && dir == BackendToClient && bytes.Contains(r.Payload, []byte(marker)) {
			first, found = res, true
		}
	}
	d.Close(true)
	if !found {
		t.Fatalf("%s: marker %q not found in backend output", name, marker)
	}
	return first, pass, sink
}

func TestZmodem_DenyDownload(t *testing.T) {
	first, pass, sink := denyRun(t, "zmodem-sz", policy.Effective{DenyDownload: true}, "**\x18B00")
	if string(first.Reply) != ZmodemCancel {
		t.Fatalf("reply to sz = %q, want the cancel sequence", first.Reply)
	}
	notice := "\r\n[devpod] " + MsgDownloadDenied + "\r\n"
	if !bytes.HasSuffix(first.Pass, []byte(notice)) {
		t.Fatalf("client output %q must end with the notice", first.Pass)
	}
	if len(sink.files) != 0 {
		t.Fatal("no file may be opened for a denied download")
	}
	if pol := sink.eventsOf(EventPolicy); len(pol) != 1 || pol[0].Rule != "download" || pol[0].Action != "deny" {
		t.Fatalf("policy events %+v", pol)
	}
	if n := len(pass[BackendToClient]); n > 4<<10 {
		t.Fatalf("%d bytes of the 1 MiB transfer still reached the client", n)
	}
}

func TestZmodem_DenyUpload(t *testing.T) {
	first, pass, sink := denyRun(t, "zmodem-rz", policy.Effective{DenyUpload: true}, "**\x18B01")
	if string(first.Reply) != ZmodemCancel {
		t.Fatalf("reply to rz = %q", first.Reply)
	}
	if notice := "\r\n[devpod] " + MsgUploadDenied + "\r\n"; !bytes.HasSuffix(first.Pass, []byte(notice)) {
		t.Fatalf("client output %q must end with the notice", first.Pass)
	}
	if len(sink.files) != 0 {
		t.Fatal("no file may be opened for a denied upload")
	}
	if n := len(pass[ClientToBackend]); n > 4<<10 {
		t.Fatalf("%d bytes of the upload still reached the backend", n)
	}
}

func TestZmodem_MaxFileSize(t *testing.T) {
	recs, _ := loadFixture(t, "zmodem-sz")
	sink := &fakeSink{}
	d := NewZmodem(policy.Effective{MaxFileSize: 1000}, sink)
	var replies, toClient []byte
	for _, r := range recs {
		dir, ok := fixtureDir(r.Kind)
		if !ok {
			continue
		}
		res := d.Feed(dir, r.Payload)
		if dir == BackendToClient {
			replies = append(replies, res.Reply...)
			toClient = append(toClient, res.Pass...)
		}
	}
	d.Close(true)
	if !bytes.HasPrefix(replies, []byte(ZmodemCancel)) {
		t.Fatalf("sz must be cancelled, replies %q", replies)
	}
	if !bytes.Contains(toClient, []byte(ZmodemCancel+"\r\n[devpod] "+MsgFileTooLarge+"\r\n")) {
		t.Fatal("client rz must be cancelled and told why")
	}
	if len(sink.files) != 0 {
		t.Fatal("oversize file must not be captured")
	}
	if pol := sink.eventsOf(EventPolicy); len(pol) != 1 || pol[0].Rule != "maxFileSize" || pol[0].Path != "zm.bin" {
		t.Fatalf("policy events %+v", pol)
	}
	if len(toClient) > 8<<10 {
		t.Fatalf("%d bytes still reached the client", len(toClient))
	}
}

func TestZmodem_TruncatedTransferIncomplete(t *testing.T) {
	recs, _ := loadFixture(t, "zmodem-sz")
	var cut []rawlog.Record
	out := 0
	for _, r := range recs {
		cut = append(cut, r)
		if r.Kind == rawlog.KindDataOut {
			if out += len(r.Payload); out > 512<<10 {
				break
			}
		}
	}
	sink := &fakeSink{}
	d := NewZmodem(policy.Effective{}, sink)
	feedRecords(d, cut, 4096)
	d.Close(false)
	if len(sink.files) != 1 || !sink.files[0].finished || sink.files[0].complete {
		t.Fatalf("want one finished incomplete file, got %d", len(sink.files))
	}
}
