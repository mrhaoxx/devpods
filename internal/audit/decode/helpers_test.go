// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package decode

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
)

type sidecarFile struct {
	Direction string `json:"direction"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
}

func loadFixture(t *testing.T, name string) ([]rawlog.Record, []sidecarFile) {
	t.Helper()
	f, err := os.Open("testdata/" + name + ".raw")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd := rawlog.NewReader(f)
	var recs []rawlog.Record
	for {
		r, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var side struct {
		Files []sidecarFile `json:"files"`
	}
	if err := json.Unmarshal(b, &side); err != nil {
		t.Fatal(err)
	}
	return recs, side.Files
}

// fixtureDir maps a data record to the direction it travelled.
func fixtureDir(k rawlog.Kind) (Dir, bool) {
	switch k {
	case rawlog.KindDataIn:
		return ClientToBackend, true
	case rawlog.KindDataOut:
		return BackendToClient, true
	}
	return 0, false
}

type fed struct {
	in, pass, reply [2][]byte
}

// feedRecords feeds the data records of recs through d, splitting each
// record into chunks of at most chunk bytes, and collects what came out.
func feedRecords(d Decoder, recs []rawlog.Record, chunk int) fed {
	var f fed
	for _, r := range recs {
		dir, ok := fixtureDir(r.Kind)
		if !ok {
			continue
		}
		for p := r.Payload; len(p) > 0; {
			n := min(chunk, len(p))
			res := d.Feed(dir, p[:n])
			f.in[dir] = append(f.in[dir], p[:n]...)
			f.pass[dir] = append(f.pass[dir], res.Pass...)
			f.reply[dir] = append(f.reply[dir], res.Reply...)
			p = p[n:]
		}
	}
	return f
}

type memFile struct {
	meta     FileMeta
	mu       sync.Mutex
	data     []byte
	finished bool
	complete bool
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if end := int(off) + len(p); end > len(m.data) {
		m.data = append(m.data, make([]byte, end-len(m.data))...)
	}
	copy(m.data[off:], p)
	return len(p), nil
}

func (m *memFile) Finish(complete bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finished, m.complete = true, complete
}

func (m *memFile) sha() string {
	s := sha256.Sum256(m.data)
	return hex.EncodeToString(s[:])
}

type fakeSink struct {
	mu     sync.Mutex
	files  []*memFile
	events []Event
}

func (s *fakeSink) OpenFile(m FileMeta) FileWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := &memFile{meta: m}
	s.files = append(s.files, f)
	return f
}

func (s *fakeSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *fakeSink) eventsOf(k EventKind) []Event {
	var out []Event
	for _, e := range s.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// assertFilesMatch checks that every sidecar entry has exactly one
// captured file with the same direction, a path ending in the sidecar
// path, identical content and complete=true.
func assertFilesMatch(t *testing.T, sink *fakeSink, want []sidecarFile) {
	t.Helper()
	if len(sink.files) != len(want) {
		t.Fatalf("captured %d files, want %d", len(sink.files), len(want))
	}
	for _, w := range want {
		found := false
		for _, f := range sink.files {
			if f.meta.Direction == w.Direction && strings.HasSuffix(f.meta.Path, w.Path) {
				found = true
				if f.sha() != w.SHA256 || int64(len(f.data)) != w.Size {
					t.Errorf("%s %s: sha/size mismatch (%d bytes)", w.Direction, w.Path, len(f.data))
				}
				if !f.finished || !f.complete {
					t.Errorf("%s %s: finished=%v complete=%v", w.Direction, w.Path, f.finished, f.complete)
				}
			}
		}
		if !found {
			t.Errorf("no captured %s file for %s", w.Direction, w.Path)
		}
	}
}

// --- SFTP packet builders --------------------------------------------------

type pkt struct{ b []byte }

func (p *pkt) u8(v byte) *pkt    { p.b = append(p.b, v); return p }
func (p *pkt) u32(v uint32) *pkt { p.b = binary.BigEndian.AppendUint32(p.b, v); return p }
func (p *pkt) u64(v uint64) *pkt { p.b = binary.BigEndian.AppendUint64(p.b, v); return p }
func (p *pkt) str(s string) *pkt { p.u32(uint32(len(s))); p.b = append(p.b, s...); return p }
func (p *pkt) frame() []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(p.b))), p.b...)
}

func sftpOpen(id uint32, path string, pflags uint32) []byte {
	return (&pkt{}).u8(fxpOpen).u32(id).str(path).u32(pflags).u32(0).frame()
}

func sftpHandle(id uint32, h string) []byte {
	return (&pkt{}).u8(fxpHandle).u32(id).str(h).frame()
}

func sftpWrite(id uint32, h string, off uint64, data []byte) []byte {
	return (&pkt{}).u8(fxpWrite).u32(id).str(h).u64(off).str(string(data)).frame()
}

func sftpRead(id uint32, h string, off uint64, n uint32) []byte {
	return (&pkt{}).u8(fxpRead).u32(id).str(h).u64(off).u32(n).frame()
}

// parseStatus decodes one SSH_FXP_STATUS frame.
func parseStatus(t *testing.T, b []byte) (id, code uint32, msg string) {
	t.Helper()
	if len(b) < 9 || b[4] != fxpStatus {
		t.Fatalf("not a status packet: %x", b)
	}
	n := binary.BigEndian.Uint32(b)
	if int(n)+4 != len(b) {
		t.Fatalf("status frame length %d, have %d bytes", n, len(b))
	}
	r := bytes.NewReader(b[5:])
	var mlen uint32
	_ = binary.Read(r, binary.BigEndian, &id)
	_ = binary.Read(r, binary.BigEndian, &code)
	_ = binary.Read(r, binary.BigEndian, &mlen)
	m := make([]byte, mlen)
	_, _ = io.ReadFull(r, m)
	return id, code, string(m)
}
