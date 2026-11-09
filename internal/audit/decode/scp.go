// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package decode

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/mrhaoxx/devpod/internal/audit/policy"
)

// SCPMode is the role the remote scp plays.
type SCPMode uint8

const (
	// SCPSink is `scp -t`: the remote receives, the client uploads.
	SCPSink SCPMode = iota + 1
	// SCPSource is `scp -f`: the remote sends, the client downloads.
	SCPSource
)

// maxSCPLine bounds a control line; longer means "not scp".
const maxSCPLine = 8 << 10

// ParseSCPCommand recognises a legacy-protocol scp exec command and
// returns its mode. Exactly one of -t / -f must be present among the
// leading flags.
func ParseSCPCommand(cmd string) (SCPMode, bool) {
	args := strings.Fields(cmd)
	if len(args) == 0 || path.Base(args[0]) != "scp" {
		return 0, false
	}
	var sink, source bool
	for _, a := range args[1:] {
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		if strings.ContainsRune(a, 't') {
			sink = true
		}
		if strings.ContainsRune(a, 'f') {
			source = true
		}
	}
	switch {
	case sink && !source:
		return SCPSink, true
	case source && !sink:
		return SCPSource, true
	}
	return 0, false
}

type scpState uint8

const (
	scpLine scpState = iota // reading a control line
	scpData                 // reading file content
	scpEnd                  // expecting the status byte after content
)

type scpDecoder struct {
	mode SCPMode
	pol  policy.Effective
	sink Sink

	mu       sync.Mutex
	src      Dir // direction carrying control lines and data
	state    scpState
	line     []byte
	dirs     []string
	file     FileWriter
	off      int64
	remain   int64
	disabled bool
}

// NewSCP decodes the legacy scp protocol on an exec channel.
func NewSCP(mode SCPMode, pol policy.Effective, sink Sink) Decoder {
	src := ClientToBackend
	if mode == SCPSource {
		src = BackendToClient
	}
	return &scpDecoder{mode: mode, pol: pol, sink: sink, src: src}
}

func (d *scpDecoder) direction() string {
	if d.mode == SCPSource {
		return "download"
	}
	return "upload"
}

func (d *scpDecoder) Feed(dir Dir, p []byte) Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.disabled || dir != d.src {
		return Result{Pass: p}
	}
	var res Result
	for len(p) > 0 {
		switch d.state {
		case scpData:
			n := min(int64(len(p)), d.remain)
			_, _ = d.file.WriteAt(p[:n], d.off)
			d.off += n
			d.remain -= n
			res.Pass = append(res.Pass, p[:n]...)
			p = p[n:]
			if d.remain == 0 {
				d.state = scpEnd
			}
		case scpEnd:
			d.file.Finish(p[0] == 0)
			d.file = nil
			res.Pass = append(res.Pass, p[0])
			p = p[1:]
			d.state = scpLine
		case scpLine:
			i := indexByte(p, '\n')
			if i < 0 {
				d.line = append(d.line, p...)
				p = nil
				if len(d.line) > maxSCPLine {
					d.disable("scp control line too long")
					res.Pass = append(res.Pass, d.line...)
					d.line = nil
				}
				continue
			}
			line := append(d.line, p[:i+1]...)
			d.line = nil
			p = p[i+1:]
			if stop := d.control(line, &res); stop {
				return res
			}
		}
	}
	return res
}

// control handles one complete control line (including '\n'). It
// returns true when the channel is being torn down.
func (d *scpDecoder) control(line []byte, res *Result) bool {
	text := string(line[:len(line)-1])
	if text == "" {
		res.Pass = append(res.Pass, line...)
		return false
	}
	switch text[0] {
	case 'C':
		size, name, err := parseSCPFileLine(text)
		if err != nil {
			d.disable(err.Error())
			res.Pass = append(res.Pass, line...)
			return false
		}
		full := path.Join(append(append([]string{}, d.dirs...), name)...)
		if d.pol.MaxFileSize > 0 && size > d.pol.MaxFileSize {
			msg := "\x01" + MsgFileTooLarge + "\n"
			d.sink.Emit(Event{Kind: EventPolicy, Protocol: "scp", Op: d.direction(), Path: full, Rule: "maxFileSize", Action: "deny", Detail: MsgFileTooLarge})
			res.Reply = append(res.Reply, msg...)
			if d.mode == SCPSource {
				res.Pass = append(res.Pass, msg...) // tell the downloading client why
			}
			res.CloseChannel = true
			d.disabled = true
			return true
		}
		d.file = d.sink.OpenFile(FileMeta{Protocol: "scp", Direction: d.direction(), Path: full, DeclaredSize: size})
		d.off, d.remain = 0, size
		d.state = scpData
		if size == 0 {
			d.state = scpEnd
		}
	case 'D':
		_, name, err := parseSCPFileLine(text)
		if err == nil {
			d.dirs = append(d.dirs, name)
		}
	case 'E':
		if len(d.dirs) > 0 {
			d.dirs = d.dirs[:len(d.dirs)-1]
		}
	}
	// T (times), \x01 / \x02 (errors) and anything else pass through.
	res.Pass = append(res.Pass, line...)
	return false
}

func (d *scpDecoder) disable(why string) {
	d.disabled = true
	d.sink.Emit(Event{Kind: EventDecoderError, Protocol: "scp", Detail: why})
	if d.file != nil {
		d.file.Finish(false)
		d.file = nil
	}
}

func (d *scpDecoder) Close(clean bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file != nil {
		d.file.Finish(false)
		d.file = nil
	}
}

// parseSCPFileLine parses "C0644 <size> <name>" / "D0755 0 <name>".
func parseSCPFileLine(text string) (size int64, name string, err error) {
	parts := strings.SplitN(text[1:], " ", 3)
	if len(parts) != 3 || parts[2] == "" {
		return 0, "", fmt.Errorf("malformed scp control line %q", text)
	}
	if _, err := strconv.ParseUint(parts[0], 8, 32); err != nil {
		return 0, "", fmt.Errorf("malformed scp mode in %q", text)
	}
	size, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || size < 0 {
		return 0, "", fmt.Errorf("malformed scp size in %q", text)
	}
	return size, parts[2], nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
