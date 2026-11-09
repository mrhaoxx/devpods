// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package decode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
)

// MaxSFTPPacket is the largest SFTP packet the decoder will buffer.
// OpenSSH caps packets at 256 KiB; anything larger disables the decoder
// for the rest of the channel.
const MaxSFTPPacket = 1 << 20

// SFTP v3 constants (draft-ietf-secsh-filexfer-02).
const (
	fxpInit     = 1
	fxpVersion  = 2
	fxpOpen     = 3
	fxpClose    = 4
	fxpRead     = 5
	fxpWrite    = 6
	fxpSetstat  = 9
	fxpFsetstat = 10
	fxpRemove   = 13
	fxpMkdir    = 14
	fxpRmdir    = 15
	fxpRename   = 18
	fxpStatus   = 101
	fxpHandle   = 102
	fxpData     = 103
	fxpExtended = 200

	fxfRead   = 0x01
	fxfWrite  = 0x02
	fxfAppend = 0x04
	fxfCreat  = 0x08
	fxfTrunc  = 0x10

	fxOK               = 0
	fxPermissionDenied = 3
	fxFailure          = 4
)

type sftpOpenReq struct {
	path             string
	download, upload bool
}

type sftpFile struct {
	path             string
	download, upload bool
	dl, ul           FileWriter
	dlCov, ulCov     coverage
}

type pendingRead struct {
	file *sftpFile
	off  int64
}

type sftpDecoder struct {
	pol  policy.Effective
	sink Sink

	mu       sync.Mutex
	buf      [2][]byte
	disabled bool
	opens    map[uint32]sftpOpenReq
	handles  map[string]*sftpFile
	reads    map[uint32]pendingRead
}

// NewSFTP decodes an SFTP subsystem channel (OpenSSH ≥ 9 `scp` uses it
// too), capturing uploads and downloads and enforcing pol.
func NewSFTP(pol policy.Effective, sink Sink) Decoder {
	return &sftpDecoder{
		pol:     pol,
		sink:    sink,
		opens:   map[uint32]sftpOpenReq{},
		handles: map[string]*sftpFile{},
		reads:   map[uint32]pendingRead{},
	}
}

func (d *sftpDecoder) carry(dir Dir) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.buf[dir])
}

func (d *sftpDecoder) Feed(dir Dir, p []byte) Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.disabled {
		return Result{Pass: p}
	}
	// Append in place (amortised O(1) per byte) and compact only after
	// whole packets were consumed, so byte-at-a-time input stays linear.
	d.buf[dir] = append(d.buf[dir], p...)
	buf := d.buf[dir]
	consumed := 0
	var res Result
	for len(buf)-consumed >= 4 {
		rest := buf[consumed:]
		n := binary.BigEndian.Uint32(rest)
		if n > MaxSFTPPacket || n == 0 {
			d.disable(fmt.Sprintf("sftp packet length %d out of range", n))
			res.Pass = append(res.Pass, rest...)
			d.buf[dir] = nil
			return res
		}
		if uint64(len(rest)) < 4+uint64(n) {
			break
		}
		frame := rest[:4+n]
		pass, reply := d.packet(dir, frame[4:])
		if pass {
			res.Pass = append(res.Pass, frame...)
		}
		res.Reply = append(res.Reply, reply...)
		consumed += int(4 + n)
	}
	if consumed > 0 {
		d.buf[dir] = append(buf[:0], buf[consumed:]...)
	}
	return res
}

func (d *sftpDecoder) disable(why string) {
	d.disabled = true
	d.sink.Emit(Event{Kind: EventDecoderError, Protocol: "sftp", Detail: why})
	d.finishAll(false)
}

// packet handles one SFTP packet body (type byte onward). It reports
// whether to forward the packet and what to reply to its sender.
func (d *sftpDecoder) packet(dir Dir, body []byte) (pass bool, reply []byte) {
	r := &reader{b: body}
	typ := r.u8()
	if typ == fxpInit || typ == fxpVersion {
		return true, nil
	}
	id := r.u32()
	if r.err != nil {
		return true, nil
	}
	if dir == BackendToClient {
		d.response(typ, id, r)
		return true, nil
	}

	switch typ {
	case fxpOpen:
		path := r.str()
		flags := r.u32()
		if r.err != nil {
			return true, nil
		}
		req := sftpOpenReq{path: path, download: flags&fxfRead != 0, upload: flags&(fxfWrite|fxfAppend) != 0}
		if req.download && d.pol.DenyDownload {
			return false, d.deny(id, fxPermissionDenied, MsgDownloadDenied, "download", "download", path)
		}
		if req.upload && d.pol.DenyUpload {
			return false, d.deny(id, fxPermissionDenied, MsgUploadDenied, "upload", "upload", path)
		}
		d.opens[id] = req
	case fxpRead:
		h := r.str()
		off := r.u64()
		n := r.u32()
		if r.err != nil {
			return true, nil
		}
		f := d.handles[h]
		if d.pol.MaxFileSize > 0 && int64(off)+int64(n) > d.pol.MaxFileSize {
			return false, d.deny(id, fxFailure, MsgFileTooLarge, "maxFileSize", "download", pathOf(f))
		}
		if f != nil {
			d.reads[id] = pendingRead{file: f, off: int64(off)}
		}
	case fxpWrite:
		h := r.str()
		off := r.u64()
		data := r.bytes()
		if r.err != nil {
			return true, nil
		}
		f := d.handles[h]
		if d.pol.MaxFileSize > 0 && int64(off)+int64(len(data)) > d.pol.MaxFileSize {
			return false, d.deny(id, fxFailure, MsgFileTooLarge, "maxFileSize", "upload", pathOf(f))
		}
		if f != nil {
			if f.ul == nil {
				f.ul = d.sink.OpenFile(FileMeta{Protocol: "sftp", Direction: "upload", Path: f.path, DeclaredSize: -1})
			}
			_, _ = f.ul.WriteAt(data, int64(off))
			f.ulCov.add(int64(off), int64(len(data)))
		}
	case fxpClose:
		h := r.str()
		if f := d.handles[h]; f != nil {
			d.finish(f, true)
			delete(d.handles, h)
		}
	case fxpRemove:
		d.op("remove", r.str(), "", r)
	case fxpRmdir:
		d.op("rmdir", r.str(), "", r)
	case fxpMkdir:
		d.op("mkdir", r.str(), "", r)
	case fxpSetstat:
		d.op("setstat", r.str(), "", r)
	case fxpFsetstat:
		d.op("setstat", pathOf(d.handles[r.str()]), "", r)
	case fxpRename:
		oldp := r.str()
		d.op("rename", oldp, r.str(), r)
	case fxpExtended:
		switch r.str() {
		case "posix-rename@openssh.com":
			oldp := r.str()
			d.op("rename", oldp, r.str(), r)
		case "hardlink@openssh.com":
			oldp := r.str()
			d.op("hardlink", oldp, r.str(), r)
		}
	}
	return true, nil
}

func (d *sftpDecoder) response(typ uint8, id uint32, r *reader) {
	switch typ {
	case fxpHandle:
		req, ok := d.opens[id]
		if !ok {
			return
		}
		delete(d.opens, id)
		h := r.str()
		if r.err != nil {
			return
		}
		f := &sftpFile{path: req.path, download: req.download, upload: req.upload}
		if f.upload {
			f.ul = d.sink.OpenFile(FileMeta{Protocol: "sftp", Direction: "upload", Path: f.path, DeclaredSize: -1})
		}
		d.handles[h] = f
	case fxpData:
		rd, ok := d.reads[id]
		if !ok {
			return
		}
		delete(d.reads, id)
		data := r.bytes()
		if r.err != nil {
			return
		}
		f := rd.file
		if f.dl == nil {
			f.dl = d.sink.OpenFile(FileMeta{Protocol: "sftp", Direction: "download", Path: f.path, DeclaredSize: -1})
		}
		_, _ = f.dl.WriteAt(data, rd.off)
		f.dlCov.add(rd.off, int64(len(data)))
	case fxpStatus:
		delete(d.opens, id)
		delete(d.reads, id)
	}
}

func (d *sftpDecoder) op(op, path, path2 string, r *reader) {
	if r.err != nil {
		return
	}
	d.sink.Emit(Event{Kind: EventFileOp, Protocol: "sftp", Op: op, Path: path, Path2: path2})
}

func (d *sftpDecoder) deny(id, code uint32, msg, rule, op, path string) []byte {
	d.sink.Emit(Event{Kind: EventPolicy, Protocol: "sftp", Op: op, Path: path, Rule: rule, Action: "deny", Detail: msg})
	return sftpStatus(id, code, msg)
}

func (d *sftpDecoder) finish(f *sftpFile, clean bool) {
	if f.ul != nil {
		f.ul.Finish(clean && f.ulCov.contiguous())
		f.ul = nil
	}
	if f.dl != nil {
		f.dl.Finish(clean && f.dlCov.contiguous())
		f.dl = nil
	}
}

func (d *sftpDecoder) finishAll(clean bool) {
	for h, f := range d.handles {
		d.finish(f, clean)
		delete(d.handles, h)
	}
}

func (d *sftpDecoder) Close(clean bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Handles still open at channel end were never closed by the client:
	// whatever was captured is a partial transfer.
	d.finishAll(false)
}

func pathOf(f *sftpFile) string {
	if f == nil {
		return ""
	}
	return f.path
}

// sftpStatus frames an SSH_FXP_STATUS response.
func sftpStatus(id, code uint32, msg string) []byte {
	body := []byte{fxpStatus}
	body = binary.BigEndian.AppendUint32(body, id)
	body = binary.BigEndian.AppendUint32(body, code)
	body = binary.BigEndian.AppendUint32(body, uint32(len(msg)))
	body = append(body, msg...)
	body = binary.BigEndian.AppendUint32(body, 0) // language tag ""
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)
}

var errShort = errors.New("short sftp packet")

// reader decodes SSH wire primitives; the first error sticks.
type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b) < n {
		r.err = errShort
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u8() uint8 {
	if v := r.take(1); v != nil {
		return v[0]
	}
	return 0
}

func (r *reader) u32() uint32 {
	if v := r.take(4); v != nil {
		return binary.BigEndian.Uint32(v)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if v := r.take(8); v != nil {
		return binary.BigEndian.Uint64(v)
	}
	return 0
}

func (r *reader) bytes() []byte {
	n := r.u32()
	if r.err != nil {
		return nil
	}
	return r.take(int(n))
}

func (r *reader) str() string { return string(r.bytes()) }
