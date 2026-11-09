// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/decode"
	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/spool"
	"github.com/mrhaoxx/devpod/internal/audit/sshmsg"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

type pendingReq struct {
	typ     string
	want    bool
	payload []byte
}

// channel records one SSH channel. It implements ChannelHooks.
type channel struct {
	s          *Session
	id         uint32
	opened     time.Time
	recordData bool
	bytes      [2]atomic.Int64

	mu          sync.Mutex
	row         store.ChannelRow
	pending     [2]*pendingReq
	dec         decode.Decoder
	decDisabled bool
	files       int
	cmd         *store.CommandRow
	closed      bool
}

var _ ChannelHooks = (*channel)(nil)

func newChannel(s *Session, id uint32, dir Dir, typ string, extra []byte) *channel {
	forward := isForward(typ)
	c := &channel{s: s, id: id, opened: s.r.cfg.Now()}
	c.recordData = !forward || s.pol.RecordForwardContent
	c.row = store.ChannelRow{SessionID: s.meta.ID, ChannelID: id, Type: typ, OpenedAt: c.opened, RawRecorded: c.recordData, Version: 1}
	switch {
	case typ == "direct-tcpip" || typ == "forwarded-tcpip":
		if t, err := sshmsg.ParseTCPIPExtra(extra); err == nil {
			c.row.Target = net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
		}
	case forward:
		if path, err := sshmsg.ParseStreamlocalExtra(extra); err == nil {
			c.row.Target = path
		}
	}
	return c
}

// isForward reports port / Unix-socket forwarding channels, whose content
// is recorded only with forwarding.recordContent.
func isForward(typ string) bool {
	switch typ {
	case "direct-tcpip", "forwarded-tcpip", "direct-streamlocal@openssh.com", "forwarded-streamlocal@openssh.com":
		return true
	}
	return false
}

func (c *channel) Request(dir Dir, typ string, want bool, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	pol := c.s.pol
	deny := func(rule, msg string) error {
		c.s.record(c.id, rawlog.KindRequest, rawlog.Encode(rawlog.Request{Dir: uint8(dir), Type: typ, WantReply: want, Denied: true, Payload: payload}))
		return c.s.deny(rule, msg)
	}
	if dir == ClientToBackend {
		switch typ {
		case "pty-req":
			if p, err := sshmsg.ParsePtyReq(payload); err == nil {
				c.row.Pty, c.row.Term = true, p.Term
			}
		case "x11-req":
			if pol.DenyX11 {
				return deny("x11Forwarding", MsgX11Denied)
			}
		case "auth-agent-req@openssh.com":
			if pol.DenyAgentForward {
				return deny("agentForwarding", MsgAgentDenied)
			}
		case "exec":
			cmd, _ := sshmsg.ParseExec(payload)
			if mode, ok := decode.ParseSCPCommand(cmd); ok {
				switch {
				case mode == decode.SCPSource && pol.DenyDownload:
					c.s.fileEvent(store.FileEventRow{ChannelID: c.id, Protocol: "scp", Op: "download", Path: lastArg(cmd), Blocked: true})
					return deny("download", decode.MsgDownloadDenied)
				case mode == decode.SCPSink && pol.DenyUpload:
					c.s.fileEvent(store.FileEventRow{ChannelID: c.id, Protocol: "scp", Op: "upload", Path: lastArg(cmd), Blocked: true})
					return deny("upload", decode.MsgUploadDenied)
				}
				c.dec = newSCP(mode, pol, &chanSink{c: c})
			} else if IsOpaqueTransfer(cmd) {
				c.s.fileEvent(store.FileEventRow{ChannelID: c.id, Protocol: "opaque", Op: "suspected_transfer", Path: cmd})
			}
			c.row.Command = cmd
			c.cmd = &store.CommandRow{
				EventID: fmt.Sprintf("%s-%d", c.s.meta.ID, c.id), TS: c.s.r.cfg.Now(), SessionID: c.s.meta.ID,
				DevPodUID: c.s.meta.DevPodUID, User: c.s.meta.User, Source: "gateway_exec", ChannelID: c.id,
				Cmdline: cmd, Attribution: "exact", Version: 1,
			}
			c.s.addRows(func(b *store.Batch) { b.Commands = append(b.Commands, *c.cmd) })
			c.startLocked()
		case "subsystem":
			name, _ := sshmsg.ParseSubsystem(payload)
			c.row.Subsystem = name
			if name == "sftp" {
				c.dec = newSFTP(pol, &chanSink{c: c})
			}
			c.startLocked()
		case "shell":
			c.startLocked()
		}
	} else if typ == "exit-status" {
		if v, err := sshmsg.ParseExitStatus(payload); err == nil {
			code := int32(v)
			c.row.ExitStatus = &code
		}
	}
	c.pending[dir] = &pendingReq{typ: typ, want: want, payload: payload}
	return nil
}

// startLocked attaches the ZMODEM decoder to pty channels that did not
// get a protocol decoder.
func (c *channel) startLocked() {
	if c.row.Pty && c.dec == nil {
		c.dec = newZmodem(c.s.pol, &chanSink{c: c})
	}
}

func (c *channel) RequestResult(dir Dir, typ string, ok bool) {
	c.mu.Lock()
	p := c.pending[dir]
	if p == nil || p.typ != typ {
		c.mu.Unlock()
		return
	}
	c.pending[dir] = nil
	c.mu.Unlock()
	c.s.record(c.id, rawlog.KindRequest, rawlog.Encode(rawlog.Request{Dir: uint8(dir), Type: typ, WantReply: p.want, OK: ok, Payload: p.payload}))
}

func (c *channel) Data(dir Dir, stderr bool, p []byte) (res Result) {
	c.bytes[dir].Add(int64(len(p)))
	if c.recordData {
		kind := rawlog.KindDataIn
		switch {
		case stderr:
			kind = rawlog.KindDataErr
		case dir == BackendToClient:
			kind = rawlog.KindDataOut
		}
		c.s.record(c.id, kind, p)
	}
	c.mu.Lock()
	dec, disabled := c.dec, c.decDisabled
	c.mu.Unlock()
	if dec == nil || disabled || stderr {
		return Result{Pass: p}
	}
	res = c.feed(dec, dir, p)
	if len(res.Notice) > 0 {
		c.s.record(c.id, rawlog.KindInjectToClient, res.Notice)
	}
	if len(res.Reply) > 0 {
		kind := rawlog.KindInjectToBackend
		if dir == ClientToBackend {
			kind = rawlog.KindInjectToClient
		}
		c.s.record(c.id, kind, res.Reply)
	}
	return res
}

// feed runs the decoder; a panic disables it for the rest of the
// channel (fail-closed + a content rule ⇒ close the channel instead).
func (c *channel) feed(dec decode.Decoder, dir Dir, p []byte) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			c.mu.Lock()
			c.decDisabled = true
			c.mu.Unlock()
			DecoderErrorsTotal.Inc()
			c.s.alert("decoder_error", fmt.Sprintf("channel %d: %v", c.id, r))
			if c.s.pol.FailClosed && c.s.pol.HasDeny() {
				res = Result{CloseChannel: true}
			} else {
				res = Result{Pass: p}
			}
		}
	}()
	return dec.Feed(dir, p)
}

func (c *channel) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	dec, disabled := c.dec, c.decDisabled
	c.mu.Unlock()
	if dec != nil && !disabled {
		func() {
			defer func() { _ = recover() }()
			dec.Close(true)
		}()
	}

	now := c.s.r.cfg.Now()
	c.mu.Lock()
	row := c.row
	row.ClosedAt, row.BytesIn, row.BytesOut, row.Version = &now, c.bytes[0].Load(), c.bytes[1].Load(), 2
	var cmd *store.CommandRow
	if c.cmd != nil {
		v := *c.cmd
		v.ExitCode, v.DurationMS, v.Version = row.ExitStatus, now.Sub(v.TS).Milliseconds(), 2
		cmd = &v
	}
	c.mu.Unlock()
	c.s.record(c.id, rawlog.KindChannelClose, rawlog.Encode(rawlog.ChannelClose{BytesIn: row.BytesIn, BytesOut: row.BytesOut}))
	c.s.addRows(func(b *store.Batch) {
		b.Channels = append(b.Channels, row)
		if cmd != nil {
			b.Commands = append(b.Commands, *cmd)
		}
	})
}

func lastArg(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// --- decoder sink ------------------------------------------------------------

type chanSink struct{ c *channel }

func (k *chanSink) OpenFile(m decode.FileMeta) decode.FileWriter {
	c := k.c
	c.mu.Lock()
	c.files++
	n := c.files
	c.mu.Unlock()
	key := store.FileKey(c.s.meta.OpenedAt, c.s.meta.ID, c.id, n)
	fw := &fileWriter{c: c, meta: m, key: key}
	f, err := c.s.r.spool.Create(key)
	if err != nil {
		fw.fail(err)
	} else {
		fw.f = f
	}
	return fw
}

func (k *chanSink) Emit(e decode.Event) {
	s := k.c.s
	switch e.Kind {
	case decode.EventFileOp:
		s.fileEvent(store.FileEventRow{ChannelID: k.c.id, Protocol: e.Protocol, Op: e.Op, Path: e.Path, Path2: e.Path2})
	case decode.EventPolicy:
		s.policyEvent(e.Rule, e.Action, e.Detail)
		if e.Path != "" {
			s.fileEvent(store.FileEventRow{ChannelID: k.c.id, Protocol: e.Protocol, Op: e.Op, Path: e.Path, Blocked: true})
		}
	case decode.EventDecoderError:
		DecoderErrorsTotal.Inc()
		s.alert("decoder_error", fmt.Sprintf("channel %d %s: %s", k.c.id, e.Protocol, e.Detail))
	}
}

// fileWriter captures one transfer into the spool.
type fileWriter struct {
	c    *channel
	meta decode.FileMeta
	key  string

	mu     sync.Mutex
	f      *spool.File
	failed bool
}

func (w *fileWriter) fail(err error) {
	w.failed = true
	if w.f != nil {
		_ = w.f.Discard()
		w.f = nil
	}
	w.c.s.mu.Lock()
	w.c.s.gapLocked(err)
	w.c.s.mu.Unlock()
}

func (w *fileWriter) WriteAt(p []byte, off int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed {
		return len(p), nil
	}
	if _, err := w.f.WriteAt(p, off); err != nil {
		w.fail(err)
	}
	return len(p), nil
}

func (w *fileWriter) Finish(complete bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	row := store.FileEventRow{ChannelID: w.c.id, Protocol: w.meta.Protocol, Op: w.meta.Direction, Path: w.meta.Path}
	if !w.failed && w.f != nil {
		sum, size, err := hashFile(w.f.Path())
		if err == nil && w.f.Seal() == nil {
			row.SHA256, row.Size, row.S3Key, row.Complete = sum, size, w.key, complete
		}
		w.f = nil
	}
	w.c.s.fileEvent(row)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
