// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mrhaoxx/devpod/internal/audit/decode"
	"github.com/mrhaoxx/devpod/internal/audit/policy"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// --- fakes -----------------------------------------------------------------

type memObjects struct {
	mu   sync.Mutex
	objs map[string][]byte
	fail bool
}

func (m *memObjects) Put(_ context.Context, key string, body io.Reader, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("s3 down")
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if m.objs == nil {
		m.objs = map[string][]byte{}
	}
	m.objs[key] = b
	return nil
}

func (m *memObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

type memEvents struct {
	mu   sync.Mutex
	all  store.Batch
	fail bool
}

func (m *memEvents) Insert(_ context.Context, b store.Batch, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("clickhouse down")
	}
	m.all.Sessions = append(m.all.Sessions, b.Sessions...)
	m.all.Channels = append(m.all.Channels, b.Channels...)
	m.all.Commands = append(m.all.Commands, b.Commands...)
	m.all.FileEvents = append(m.all.FileEvents, b.FileEvents...)
	m.all.PolicyEvents = append(m.all.PolicyEvents, b.PolicyEvents...)
	m.all.RawSegments = append(m.all.RawSegments, b.RawSegments...)
	m.all.Alerts = append(m.all.Alerts, b.Alerts...)
	return nil
}

type rig struct {
	t    *testing.T
	r    *Recorder
	objs *memObjects
	evs  *memEvents
}

func newRig(t *testing.T, spoolMax int64) *rig {
	t.Helper()
	objs, evs := &memObjects{}, &memEvents{}
	r, err := NewRecorder(Config{SpoolDir: t.TempDir(), SpoolMaxBytes: spoolMax, Objects: objs, Events: evs, GatewayPod: "gw-0"})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, r: r, objs: objs, evs: evs}
}

func (g *rig) start(pol policy.Effective) *Session {
	return g.r.Start(SessionMeta{ID: NewSessionID(time.Now()), User: "alice", DevPod: "alice-dev", DevPodUID: "uid-1", Owner: "alice", OpenedAt: time.Now()}, pol)
}

func (g *rig) drain() store.Batch {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.r.Drain(ctx); err != nil {
		g.t.Fatal(err)
	}
	g.evs.mu.Lock()
	defer g.evs.mu.Unlock()
	return g.evs.all
}

func str(s string) []byte { return ssh.Marshal(struct{ S string }{s}) }
func u32(v uint32) []byte { return ssh.Marshal(struct{ V uint32 }{v}) }

func req(t *testing.T, ch ChannelHooks, dir Dir, typ string, payload []byte) error {
	t.Helper()
	err := ch.Request(dir, typ, true, payload)
	if err == nil {
		ch.RequestResult(dir, typ, true)
	}
	return err
}

func isDeny(err error, msg string) bool {
	var d *DenyError
	return errors.As(err, &d) && d.Msg == msg
}

// --- tests -----------------------------------------------------------------

func TestSession_ExecCommandRecorded(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	ch, err := s.OpenChannel(ClientToBackend, "session", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req(t, ch, ClientToBackend, "exec", str("make test")); err != nil {
		t.Fatal(err)
	}
	ch.Data(BackendToClient, false, []byte("ok\n"))
	_ = req(t, ch, BackendToClient, "exit-status", u32(2))
	ch.Close()
	s.Close("client_disconnect", 10, 3)
	all := g.drain()

	var last store.CommandRow
	for _, c := range all.Commands {
		if c.Version >= last.Version {
			last = c
		}
	}
	if last.Cmdline != "make test" || last.ExitCode == nil || *last.ExitCode != 2 || last.Source != "gateway_exec" || last.Attribution != "exact" {
		t.Fatalf("command row %+v", last)
	}
	var closed store.SessionRow
	for _, r := range all.Sessions {
		if r.Version == 2 {
			closed = r
		}
	}
	if closed.ClosedAt == nil || closed.CloseReason != "client_disconnect" || closed.BytesIn != 10 || closed.PolicySnapshot == "" {
		t.Fatalf("closed session row %+v", closed)
	}
}

type sidecar struct {
	Files []struct{ Direction, Path, SHA256 string } `json:"files"`
}

func feedFixture(t *testing.T, ch ChannelHooks, name string) (passed [2][]byte) {
	t.Helper()
	f, err := os.Open("decode/testdata/" + name + ".raw")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd := rawlog.NewReader(f)
	for {
		r, err := rd.Next()
		if err == io.EOF {
			return passed
		}
		if err != nil {
			t.Fatal(err)
		}
		switch r.Kind {
		case rawlog.KindDataIn:
			passed[0] = append(passed[0], ch.Data(ClientToBackend, false, r.Payload).Pass...)
		case rawlog.KindDataOut:
			passed[1] = append(passed[1], ch.Data(BackendToClient, false, r.Payload).Pass...)
		}
	}
}

func TestSession_SFTPUploadStoresObjectAndEvent(t *testing.T) {
	g := newRig(t, 256<<20)
	s := g.start(policy.Effective{})
	ch, _ := s.OpenChannel(ClientToBackend, "session", nil)
	_ = req(t, ch, ClientToBackend, "subsystem", str("sftp"))
	feedFixture(t, ch, "sftp-put-get")
	ch.Close()
	s.Close("client_disconnect", 0, 0)
	all := g.drain()

	b, _ := os.ReadFile("decode/testdata/sftp-put-get.json")
	var sc sidecar
	_ = json.Unmarshal(b, &sc)
	found := 0
	for _, fe := range all.FileEvents {
		if fe.Protocol != "sftp" || (fe.Op != "upload" && fe.Op != "download") {
			continue
		}
		found++
		obj := g.objs.objs[fe.S3Key]
		sum := sha256.Sum256(obj)
		if hex.EncodeToString(sum[:]) != fe.SHA256 || !fe.Complete || fe.Size != int64(len(obj)) {
			t.Errorf("%s %s: object/sha mismatch (%d bytes)", fe.Op, fe.Path, len(obj))
		}
		for _, want := range sc.Files {
			if want.Direction == fe.Op && want.SHA256 != fe.SHA256 {
				t.Errorf("%s sha %s, sidecar %s", fe.Op, fe.SHA256, want.SHA256)
			}
		}
	}
	if found != 2 {
		t.Fatalf("%d transfer events, want 2: %+v", found, all.FileEvents)
	}
	if len(all.RawSegments) == 0 {
		t.Fatal("no raw segments indexed")
	}
	for _, seg := range all.RawSegments {
		if _, ok := g.objs.objs[seg.S3Key]; !ok {
			t.Errorf("segment %s not uploaded", seg.S3Key)
		}
	}
}

func TestSession_SCPDownloadDeniedAtRequest(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{DenyDownload: true})
	ch, _ := s.OpenChannel(ClientToBackend, "session", nil)
	if err := ch.Request(ClientToBackend, "exec", true, str("scp -f /etc/shadow")); !isDeny(err, decode.MsgDownloadDenied) {
		t.Fatalf("got %v", err)
	}
	if err := ch.Request(ClientToBackend, "exec", true, str("scp -t /tmp")); err != nil {
		t.Fatalf("uploads stay allowed: %v", err)
	}
	ch.Close()
	s.Close("x", 0, 0)
	all := g.drain()
	if len(all.PolicyEvents) != 1 || all.PolicyEvents[0].Rule != "download" {
		t.Fatalf("policy rows %+v", all.PolicyEvents)
	}
}

func TestSession_DenyLocalForward(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{DenyLocalForward: true})
	extra := ssh.Marshal(struct {
		H  string
		P  uint32
		OH string
		OP uint32
	}{"127.0.0.1", 8888, "1.2.3.4", 5})
	if _, err := s.OpenChannel(ClientToBackend, "direct-tcpip", extra); !isDeny(err, MsgForwardDenied) {
		t.Fatalf("got %v", err)
	}
	s.Close("x", 0, 0)
	if all := g.drain(); len(all.PolicyEvents) != 1 || all.PolicyEvents[0].Rule != "forwarding.local" {
		t.Fatalf("policy rows %+v", all.PolicyEvents)
	}
}

func TestSession_DenyRemoteForward(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{DenyRemoteForward: true})
	p := ssh.Marshal(struct {
		H string
		P uint32
	}{"0.0.0.0", 9000})
	if err := s.GlobalRequest(ClientToBackend, "tcpip-forward", p); !isDeny(err, MsgForwardDenied) {
		t.Fatalf("got %v", err)
	}
	if err := s.GlobalRequest(ClientToBackend, "keepalive@openssh.com", nil); err != nil {
		t.Fatalf("other global requests pass: %v", err)
	}
	s.Close("x", 0, 0)
	if all := g.drain(); len(all.PolicyEvents) != 1 || all.PolicyEvents[0].Rule != "forwarding.remote" {
		t.Fatalf("policy rows %+v", all.PolicyEvents)
	}
}

func TestSession_DenyAgentAndX11(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{DenyAgentForward: true, DenyX11: true})
	ch, _ := s.OpenChannel(ClientToBackend, "session", nil)
	if err := ch.Request(ClientToBackend, "auth-agent-req@openssh.com", true, nil); !isDeny(err, MsgAgentDenied) {
		t.Fatalf("agent req: %v", err)
	}
	if err := ch.Request(ClientToBackend, "x11-req", true, nil); !isDeny(err, MsgX11Denied) {
		t.Fatalf("x11 req: %v", err)
	}
	if _, err := s.OpenChannel(BackendToClient, "auth-agent@openssh.com", nil); !isDeny(err, MsgAgentDenied) {
		t.Fatalf("agent channel: %v", err)
	}
	if _, err := s.OpenChannel(BackendToClient, "x11", nil); !isDeny(err, MsgX11Denied) {
		t.Fatalf("x11 channel: %v", err)
	}
	ch.Close()
	s.Close("x", 0, 0)
	rules := map[string]int{}
	for _, p := range g.drain().PolicyEvents {
		rules[p.Rule]++
	}
	if rules["agentForwarding"] != 2 || rules["x11Forwarding"] != 2 {
		t.Fatalf("rules %v", rules)
	}
}

// rawRecords decodes every uploaded raw segment of the drained session.
func rawRecords(t *testing.T, g *rig, all store.Batch) []rawlog.Record {
	t.Helper()
	var recs []rawlog.Record
	for _, seg := range all.RawSegments {
		rd, err := rawlog.OpenSegment(bytes.NewReader(g.objs.objs[seg.S3Key]))
		if err != nil {
			t.Fatal(err)
		}
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
	}
	return recs
}

func TestSession_ForwardContentRecordingToggle(t *testing.T) {
	for _, record := range []bool{false, true} {
		g := newRig(t, 64<<20)
		s := g.start(policy.Effective{RecordForwardContent: record})
		ch, err := s.OpenChannel(ClientToBackend, "direct-tcpip", nil)
		if err != nil {
			t.Fatal(err)
		}
		ch.Data(ClientToBackend, false, []byte("GET / HTTP/1.1\r\n"))
		ch.Data(BackendToClient, false, []byte("HTTP/1.1 200 OK\r\n"))
		ch.Close()
		s.Close("x", 0, 0)
		all := g.drain()
		data := 0
		for _, r := range rawRecords(t, g, all) {
			if r.Kind == rawlog.KindDataIn || r.Kind == rawlog.KindDataOut {
				data++
			}
		}
		if (data > 0) != record {
			t.Errorf("record=%v: %d data records", record, data)
		}
		if all.Channels[len(all.Channels)-1].BytesIn == 0 {
			t.Errorf("byte counts must be kept even without content")
		}
	}
}

func TestSession_ZmodemOnlyOnPty(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{DenyDownload: true})
	ch, _ := s.OpenChannel(ClientToBackend, "session", nil)
	_ = req(t, ch, ClientToBackend, "exec", str("cat blob.bin"))
	in := []byte("x**\x18B00000000000000\r\ny")
	if res := ch.Data(BackendToClient, false, in); !bytes.Equal(res.Pass, in) || len(res.Reply) != 0 {
		t.Fatalf("exec output altered: %q", res.Pass)
	}
	ch.Close()
	s.Close("x", 0, 0)
	all := g.drain()
	for _, fe := range all.FileEvents {
		if fe.Protocol == "zmodem" {
			t.Fatalf("zmodem event on a non-pty channel: %+v", fe)
		}
	}
	if len(all.PolicyEvents) != 0 {
		t.Fatalf("policy fired on a non-pty channel: %+v", all.PolicyEvents)
	}
}

func TestSession_ConcurrentChannelsRace(t *testing.T) {
	g := newRig(t, 64<<20)
	s := g.start(policy.Effective{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ch, err := s.OpenChannel(ClientToBackend, "session", nil)
			if err != nil {
				t.Error(err)
				return
			}
			_ = req(t, ch, ClientToBackend, "pty-req", ssh.Marshal(struct {
				T          string
				C, R, W, H uint32
				M          string
			}{"xterm", 80, 24, 0, 0, ""}))
			_ = req(t, ch, ClientToBackend, "shell", nil)
			for j := 0; j < 50; j++ {
				ch.Data(ClientToBackend, false, []byte(fmt.Sprintf("echo %d %d\r", i, j)))
				ch.Data(BackendToClient, false, []byte(fmt.Sprintf("%d %d\r\n", i, j)))
			}
			ch.Close()
		}(i)
	}
	wg.Wait()
	s.Close("x", 0, 0)
	all := g.drain()
	ids := map[uint32]int{}
	for _, c := range all.Channels {
		ids[c.ChannelID]++
	}
	if len(ids) != 8 || len(all.Channels) != 16 {
		t.Fatalf("channel ids %v, %d rows", ids, len(all.Channels))
	}
}

type panicDecoder struct{}

func (panicDecoder) Feed(decode.Dir, []byte) decode.Result { panic("boom") }
func (panicDecoder) Close(bool)                            {}

func TestSession_DecoderPanicFailClosedClosesChannel(t *testing.T) {
	orig := newSFTP
	newSFTP = func(policy.Effective, decode.Sink) decode.Decoder { return panicDecoder{} }
	defer func() { newSFTP = orig }()

	for _, failClosed := range []bool{true, false} {
		g := newRig(t, 64<<20)
		s := g.start(policy.Effective{DenyDownload: true, FailClosed: failClosed})
		ch, _ := s.OpenChannel(ClientToBackend, "session", nil)
		_ = req(t, ch, ClientToBackend, "subsystem", str("sftp"))
		res := ch.Data(ClientToBackend, false, []byte("abc"))
		if failClosed && !res.CloseChannel {
			t.Fatal("fail-closed must close the channel when its decoder dies")
		}
		if !failClosed && (res.CloseChannel || string(res.Pass) != "abc") {
			t.Fatalf("fail-open must pass through, got %+v", res)
		}
		if res := ch.Data(ClientToBackend, false, []byte("def")); !failClosed && string(res.Pass) != "def" {
			t.Fatal("decoder must stay disabled after a panic")
		}
		ch.Close()
		s.Close("x", 0, 0)
		all := g.drain()
		if len(all.Alerts) != 1 || all.Alerts[0].Kind != "decoder_error" {
			t.Fatalf("alerts %+v", all.Alerts)
		}
	}
}
