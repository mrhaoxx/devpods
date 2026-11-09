// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package decode

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
)

func TestParseSCPCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		mode SCPMode
		ok   bool
	}{
		{"scp -t /tmp", SCPSink, true},
		{"scp -r -f a", SCPSource, true},
		{"/usr/bin/scp -p -t .", SCPSink, true},
		{"scp -r -t /var/x/rdst/", SCPSink, true},
		{"scp -v -d -t -- /dst", SCPSink, true},
		{"ls -t", 0, false},
		{"scp", 0, false},
		{"scp a b", 0, false},
		{"scp -t -f x", 0, false},
	}
	for _, c := range cases {
		mode, ok := ParseSCPCommand(c.cmd)
		if ok != c.ok || mode != c.mode {
			t.Errorf("%q: got (%v,%v) want (%v,%v)", c.cmd, mode, ok, c.mode, c.ok)
		}
	}
}

func testSCPFixture(t *testing.T, name string, mode SCPMode) {
	recs, want := loadFixture(t, name)
	for _, chunk := range []int{1, 4096} {
		t.Run(fmt.Sprint("chunk=", chunk), func(t *testing.T) {
			sink := &fakeSink{}
			d := NewSCP(mode, policy.Effective{}, sink)
			f := feedRecords(d, recs, chunk)
			d.Close(true)
			assertFilesMatch(t, sink, want)
			for dir := 0; dir < 2; dir++ {
				if !bytes.Equal(f.pass[dir], f.in[dir]) {
					t.Fatalf("dir %d: stream altered", dir)
				}
			}
			for _, f := range sink.files {
				if f.meta.Protocol != "scp" || f.meta.DeclaredSize != int64(len(f.data)) {
					t.Errorf("%s: meta %+v, %d bytes", f.meta.Path, f.meta, len(f.data))
				}
			}
		})
	}
}

func TestSCP_UploadFixture(t *testing.T)    { testSCPFixture(t, "scp-upload", SCPSink) }
func TestSCP_DownloadFixture(t *testing.T)  { testSCPFixture(t, "scp-download", SCPSource) }
func TestSCP_RecursiveFixture(t *testing.T) { testSCPFixture(t, "scp-recursive", SCPSink) }

func TestSCP_MaxFileSizeUpload(t *testing.T) {
	sink := &fakeSink{}
	d := NewSCP(SCPSink, policy.Effective{MaxFileSize: 1024}, sink)
	d.Feed(BackendToClient, []byte{0}) // sink ready
	res := d.Feed(ClientToBackend, []byte("C0644 5000 f\n"))
	if len(res.Pass) != 0 {
		t.Fatalf("oversize C line was forwarded: %q", res.Pass)
	}
	if want := "\x01" + MsgFileTooLarge + "\n"; string(res.Reply) != want || !res.CloseChannel {
		t.Fatalf("reply %q close %v", res.Reply, res.CloseChannel)
	}
	if pol := sink.eventsOf(EventPolicy); len(pol) != 1 || pol[0].Rule != "maxFileSize" || pol[0].Path != "f" {
		t.Fatalf("policy events %+v", pol)
	}
	if len(sink.files) != 0 {
		t.Fatal("no file may be opened for a rejected transfer")
	}
}

func TestSCP_MaxFileSizeDownload(t *testing.T) {
	sink := &fakeSink{}
	d := NewSCP(SCPSource, policy.Effective{MaxFileSize: 1024}, sink)
	d.Feed(ClientToBackend, []byte{0})
	res := d.Feed(BackendToClient, []byte("C0644 5000 secret.db\n"))
	want := "\x01" + MsgFileTooLarge + "\n"
	if string(res.Pass) != want || string(res.Reply) != want || !res.CloseChannel {
		t.Fatalf("pass %q reply %q close %v", res.Pass, res.Reply, res.CloseChannel)
	}
}

func TestSCP_SmallFileWithinLimitPasses(t *testing.T) {
	sink := &fakeSink{}
	d := NewSCP(SCPSink, policy.Effective{MaxFileSize: 1024}, sink)
	in := []byte("C0644 3 ok.txt\nabc\x00")
	if res := d.Feed(ClientToBackend, in); !bytes.Equal(res.Pass, in) || res.CloseChannel {
		t.Fatalf("pass %q", res.Pass)
	}
	if len(sink.files) != 1 || string(sink.files[0].data) != "abc" || !sink.files[0].complete {
		t.Fatalf("files %+v", sink.files)
	}
}
