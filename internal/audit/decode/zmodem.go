// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package decode

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"strconv"
	"sync"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
)

// ZmodemCancel aborts a ZMODEM session: 8×CAN + 10×BS (lrzsz's canit).
const ZmodemCancel = "\x18\x18\x18\x18\x18\x18\x18\x18\x08\x08\x08\x08\x08\x08\x08\x08\x08\x08"

// zSpecial are the bytes that can start a ZMODEM frame or abort.
const zSpecial = "*\x18"

// zmodemIdle ends "drop" mode when no frame bytes were seen for this long.
const zmodemIdle = 2 * time.Second

// ZMODEM constants (Forsberg, "The ZMODEM Inter Application File Transfer Protocol").
const (
	zPad   = '*'
	zDLE   = 0x18
	zBin   = 'A'
	zHex   = 'B'
	zBin32 = 'C'

	zRQINIT  = 0
	zRINIT   = 1
	zSINIT   = 2
	zFILE    = 4
	zFIN     = 8
	zDATA    = 10
	zEOF     = 11
	zCOMMAND = 18

	zCRCE = 'h'
	zCRCG = 'i'
	zCRCQ = 'j'
	zCRCW = 'k'
	zRUB0 = 'l'
	zRUB1 = 'm'
)

type zstate uint8

const (
	zsIdle     zstate = iota
	zsHdrType         // saw ZPAD… ZDLE, expecting A/B/C
	zsHdrHex          // reading 14 hex digits
	zsHexTrail        // CR LF [XON] after a hex header
	zsHdrBin          // reading an escaped binary header
	zsData            // reading a data subpacket
	zsCRC             // reading a data subpacket's CRC
)

type zside struct {
	st      zstate
	stars   int
	cans    int
	kind    byte
	hex     []byte
	hdr     []byte
	esc     bool
	trail   int
	crc32   bool
	frame   byte
	sub     []byte
	end     byte
	crcb    []byte
	pos     int64
	hold    bool
	pending []byte
}

type zfile struct {
	w    FileWriter
	dir  Dir
	size int64
	cov  coverage
}

type zmodemDecoder struct {
	pol  policy.Effective
	sink Sink
	now  func() time.Time

	mu        sync.Mutex
	side      [2]zside
	dropping  bool
	lastFrame time.Time
	file      *zfile

	// per-Feed scratch
	out, reply []byte
	sawFrame   bool
}

// NewZmodem decodes ZMODEM (lrzsz sz/rz) inside a pty channel's byte
// streams, capturing transfers and cancelling denied ones.
func NewZmodem(pol policy.Effective, sink Sink) Decoder {
	return &zmodemDecoder{pol: pol, sink: sink, now: time.Now}
}

func (d *zmodemDecoder) Feed(dir Dir, p []byte) Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if d.dropping && now.Sub(d.lastFrame) > zmodemIdle {
		d.dropping = false
	}
	d.out, d.reply, d.sawFrame = nil, nil, false
	s := &d.side[dir]
	for i := 0; i < len(p); {
		// Outside a frame, bytes other than ZPAD / ZDLE only reset the
		// star and CAN counters and pass through: skip them in bulk.
		if s.st == zsIdle && !s.hold {
			n := bytes.IndexAny(p[i:], zSpecial)
			if n < 0 {
				n = len(p) - i
			}
			if n > 0 {
				s.stars, s.cans = 0, 0
				if i == 0 && n == len(p) {
					return Result{Pass: p} // plain terminal output: no copy
				}
				d.out = append(d.out, p[i:i+n]...)
				i += n
				continue
			}
		}
		d.step(dir, s, p[i])
		i++
	}
	if d.sawFrame {
		d.lastFrame = now
	}
	return Result{Pass: d.out, Reply: d.reply}
}

// put routes one raw byte: held while a header / ZFILE frame is being
// decided, dropped when it belongs to a frame in drop mode, else passed.
func (d *zmodemDecoder) put(s *zside, b byte, frame bool) {
	if frame {
		d.sawFrame = true
	}
	switch {
	case s.hold:
		s.pending = append(s.pending, b)
	case frame && d.dropping:
	default:
		d.out = append(d.out, b)
	}
}

func (d *zmodemDecoder) release(s *zside) {
	if !s.hold {
		return
	}
	s.hold = false
	if !d.dropping {
		d.out = append(d.out, s.pending...)
	}
	s.pending = s.pending[:0]
}

func (d *zmodemDecoder) step(dir Dir, s *zside, b byte) {
	if b == zDLE {
		s.cans++
	} else {
		s.cans = 0
	}
	d.handle(dir, s, b)
	if s.cans >= 5 { // CAN×5: the session was aborted
		s.cans = 0
		d.release(s)
		s.st, s.esc, s.stars = zsIdle, false, 0
		d.finishFile(false)
	}
}

func (d *zmodemDecoder) handle(dir Dir, s *zside, b byte) {
	switch s.st {
	case zsIdle:
		switch {
		case b == zPad:
			s.stars++
			d.put(s, b, d.dropping)
		case b == zDLE && s.stars > 0:
			s.stars = 0
			s.st = zsHdrType
			s.hold = true
			s.pending = append(s.pending[:0], b)
		default:
			s.stars = 0
			d.put(s, b, false)
		}
	case zsHdrType:
		switch b {
		case zHex:
			s.kind, s.st, s.hex = b, zsHdrHex, s.hex[:0]
			d.put(s, b, true)
		case zBin, zBin32:
			s.kind, s.st, s.hdr, s.esc = b, zsHdrBin, s.hdr[:0], false
			d.put(s, b, true)
		default:
			d.notHeader(dir, s, b)
		}
	case zsHdrHex:
		if !isHexDigit(b) {
			d.notHeader(dir, s, b)
			return
		}
		d.put(s, b, true)
		s.hex = append(s.hex, b)
		if len(s.hex) == 14 {
			var raw [7]byte
			for i := range raw {
				v, _ := strconv.ParseUint(string(s.hex[2*i:2*i+2]), 16, 8)
				raw[i] = byte(v)
			}
			s.trail = 0
			d.header(dir, s, raw[0], raw[1:5])
		}
	case zsHexTrail:
		switch b {
		case 0x0d, 0x8d, 0x0a, 0x8a, 0x11:
			if s.trail < 3 {
				s.trail++
				d.put(s, b, true)
				return
			}
		}
		s.st = zsIdle
		d.handle(dir, s, b)
	case zsHdrBin:
		d.put(s, b, true)
		v, ok := s.unescape(b)
		if !ok {
			return
		}
		s.hdr = append(s.hdr, v)
		need := 7
		if s.kind == zBin32 {
			need = 9
		}
		if len(s.hdr) == need {
			d.header(dir, s, s.hdr[0], s.hdr[1:5])
		}
	case zsData:
		d.put(s, b, true)
		if s.esc {
			s.esc = false
			switch b {
			case zCRCE, zCRCG, zCRCQ, zCRCW:
				s.end, s.st, s.crcb = b, zsCRC, s.crcb[:0]
			default:
				s.sub = append(s.sub, unescapeByte(b))
			}
			return
		}
		switch b {
		case zDLE:
			s.esc = true
		case 0x11, 0x13, 0x91, 0x93: // flow control, never data
		default:
			s.sub = append(s.sub, b)
		}
	case zsCRC:
		d.put(s, b, true)
		v, ok := s.unescape(b)
		if !ok {
			return
		}
		s.crcb = append(s.crcb, v)
		need := 2
		if s.crc32 {
			need = 4
		}
		if len(s.crcb) == need {
			d.subpacket(dir, s)
		}
	}
}

// unescape decodes one escaped byte; ok is false for a ZDLE prefix or a
// flow-control byte.
func (s *zside) unescape(b byte) (byte, bool) {
	if s.esc {
		s.esc = false
		return unescapeByte(b), true
	}
	switch b {
	case zDLE:
		s.esc = true
		return 0, false
	case 0x11, 0x13, 0x91, 0x93:
		return 0, false
	}
	return b, true
}

func unescapeByte(b byte) byte {
	switch b {
	case zRUB0:
		return 0x7f
	case zRUB1:
		return 0xff
	}
	return b ^ 0x40
}

// notHeader abandons a header candidate: the held bytes were plain
// output after all.
func (d *zmodemDecoder) notHeader(dir Dir, s *zside, b byte) {
	s.hold = false
	d.out = append(d.out, s.pending...)
	s.pending = s.pending[:0]
	s.st = zsIdle
	d.handle(dir, s, b)
}

func (d *zmodemDecoder) header(dir Dir, s *zside, typ byte, p []byte) {
	pos := int64(p[0]) | int64(p[1])<<8 | int64(p[2])<<16 | int64(p[3])<<24
	next := zsIdle
	if s.kind == zHex {
		next = zsHexTrail
	}
	switch typ {
	case zRQINIT:
		if dir == BackendToClient && d.pol.DenyDownload && !d.dropping {
			d.deny(dir, s, "download", MsgDownloadDenied, "", false)
			s.st = next
			return
		}
	case zRINIT:
		if dir == BackendToClient && d.pol.DenyUpload && !d.dropping {
			d.deny(dir, s, "upload", MsgUploadDenied, "", false)
			s.st = next
			return
		}
	case zFILE, zDATA, zSINIT, zCOMMAND:
		s.frame, s.pos, s.sub, s.esc = typ, pos, s.sub[:0], false
		s.crc32 = s.kind == zBin32
		s.st = zsData
		if typ != zFILE { // a ZFILE frame stays held until its size is known
			d.release(s)
		}
		return
	case zEOF:
		if f := d.file; f != nil && f.dir == dir {
			complete := f.cov.contiguous() && f.cov.end() == pos && (f.size < 0 || f.size == pos)
			d.finishFile(complete)
		}
	case zFIN:
		d.finishFile(false)
		d.release(s)
		d.dropping = false
		s.st = next
		return
	}
	d.release(s)
	s.st = next
}

func (d *zmodemDecoder) subpacket(dir Dir, s *zside) {
	data := append(s.sub, s.end)
	var ok bool
	if s.crc32 {
		ok = crc32.ChecksumIEEE(data) == binary.LittleEndian.Uint32(s.crcb)
	} else {
		ok = crc16(data) == binary.BigEndian.Uint16(s.crcb)
	}
	payload := s.sub

	switch s.frame {
	case zFILE:
		name, size := parseZfileInfo(payload)
		switch {
		case !ok || d.dropping:
			d.release(s)
		case dir == BackendToClient && d.pol.DenyDownload:
			d.deny(dir, s, "download", MsgDownloadDenied, name, true)
		case dir == ClientToBackend && d.pol.DenyUpload:
			d.deny(dir, s, "upload", MsgUploadDenied, name, true)
		case d.pol.MaxFileSize > 0 && size > d.pol.MaxFileSize:
			d.deny(dir, s, "maxFileSize", MsgFileTooLarge, name, true)
		default:
			d.finishFile(false)
			d.file = &zfile{
				w:    d.sink.OpenFile(FileMeta{Protocol: "zmodem", Direction: direction(dir), Path: name, DeclaredSize: size}),
				dir:  dir,
				size: size,
			}
			d.release(s)
		}
	case zDATA:
		if ok && !d.dropping {
			if f := d.file; f != nil && f.dir == dir {
				_, _ = f.w.WriteAt(payload, s.pos)
				f.cov.add(s.pos, int64(len(payload)))
			}
			s.pos += int64(len(payload))
		}
	}
	s.sub = s.sub[:0]
	if s.end == zCRCG || s.end == zCRCQ {
		s.st = zsData
	} else {
		s.st = zsIdle
	}
}

// deny drops the held frame, cancels the sender and tells the user why.
// cancelBoth also cancels the receiving side (it already started).
func (d *zmodemDecoder) deny(dir Dir, s *zside, rule, msg, path string, cancelBoth bool) {
	s.hold = false
	s.pending = s.pending[:0]
	d.finishFile(false)
	d.sink.Emit(Event{Kind: EventPolicy, Protocol: "zmodem", Op: direction(dir), Path: path, Rule: rule, Action: "deny", Detail: msg})
	notice := "\r\n[devpod] " + msg + "\r\n"
	if dir == BackendToClient {
		d.reply = append(d.reply, ZmodemCancel...)
		if cancelBoth {
			d.out = append(d.out, ZmodemCancel...)
		}
		d.out = append(d.out, notice...)
	} else {
		d.reply = append(d.reply, ZmodemCancel+notice...)
		d.out = append(d.out, ZmodemCancel...)
	}
	d.dropping = true
	d.sawFrame = true
}

func (d *zmodemDecoder) finishFile(complete bool) {
	if d.file != nil {
		d.file.w.Finish(complete)
		d.file = nil
	}
}

func (d *zmodemDecoder) Close(clean bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.finishFile(false)
}

func direction(dir Dir) string {
	if dir == BackendToClient {
		return "download"
	}
	return "upload"
}

// parseZfileInfo parses a ZFILE subpacket: "name\0length mtime mode …".
func parseZfileInfo(p []byte) (name string, size int64) {
	size = -1
	i := bytes.IndexByte(p, 0)
	if i < 0 {
		return string(p), size
	}
	name = string(p[:i])
	rest := p[i+1:]
	if j := bytes.IndexByte(rest, 0); j >= 0 {
		rest = rest[:j]
	}
	fields := bytes.Fields(rest)
	if len(fields) > 0 {
		if n, err := strconv.ParseInt(string(fields[0]), 10, 64); err == nil {
			size = n
		}
	}
	return name, size
}

// crc16 is the XMODEM CRC (poly 0x1021, init 0) ZMODEM uses.
func crc16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}
