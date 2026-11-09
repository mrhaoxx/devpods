// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package decode turns the byte streams of an SSH channel into file
// events (with content) and enforces content policies by filtering the
// stream. Decoders understand SFTP, the legacy scp protocol and ZMODEM.
package decode

import "io"

// Dir is the direction bytes travel through the gateway.
type Dir uint8

const (
	ClientToBackend Dir = 0
	BackendToClient Dir = 1
)

// Result is what a decoder wants done with one chunk.
type Result struct {
	// Pass is forwarded in the same direction (possibly altered).
	Pass []byte
	// Reply is written back to the side that sent the chunk.
	Reply []byte
	// Notice is human-readable text for the client's terminal, written
	// to the channel's stderr (OpenSSH sftp/scp print it verbatim but
	// show only status codes for protocol-level errors).
	Notice []byte
	// CloseChannel asks the proxy to close the channel.
	CloseChannel bool
}

// Decoder inspects one direction-tagged byte stream.
//
// Contract: Pass and Reply only ever contain whole protocol units. The
// proxy writes each of them under the destination's write mutex, so a
// unit injected by one direction never interleaves inside a unit being
// forwarded by the other. Feed is called concurrently for the two
// directions; implementations synchronise internally.
type Decoder interface {
	Feed(dir Dir, p []byte) Result
	// Close finalises the stream; clean is false when the channel ended
	// abnormally. Every file still open is finished as incomplete.
	Close(clean bool)
}

// FileMeta describes a captured transfer.
type FileMeta struct {
	Protocol     string // sftp | scp | zmodem
	Direction    string // upload | download
	Path         string
	DeclaredSize int64 // -1 when unknown
}

// FileWriter receives the content of one transfer at its offsets.
type FileWriter interface {
	io.WriterAt
	// Finish ends the transfer; complete reports whether the captured
	// bytes form the whole file.
	Finish(complete bool)
}

// EventKind classifies a decoder event.
type EventKind uint8

const (
	EventFileOp EventKind = iota + 1
	EventPolicy
	EventDecoderError
)

// Event is a non-content occurrence: a file operation (remove, rename,
// …), a policy action, or a decoder failure.
type Event struct {
	Kind     EventKind
	Protocol string
	Op       string
	Path     string
	Path2    string
	Rule     string
	Action   string
	Detail   string
}

// Sink receives a decoder's output.
type Sink interface {
	OpenFile(m FileMeta) FileWriter
	Emit(e Event)
}

// User-visible policy messages.
const (
	MsgDownloadDenied = "blocked by DevPod audit policy: downloads disabled"
	MsgUploadDenied   = "blocked by DevPod audit policy: uploads disabled"
	MsgFileTooLarge   = "blocked by DevPod audit policy: file exceeds size limit"
)

// coverage tracks which byte ranges of a file were captured.
type coverage struct {
	spans [][2]int64 // sorted, non-overlapping, non-adjacent [start, end)
}

func (c *coverage) add(off, n int64) {
	if n <= 0 {
		return
	}
	s, e := off, off+n
	var out [][2]int64
	placed := false
	for _, sp := range c.spans {
		switch {
		case sp[1] < s:
			out = append(out, sp)
		case e < sp[0]:
			if !placed {
				out = append(out, [2]int64{s, e})
				placed = true
			}
			out = append(out, sp)
		default: // overlap or touch: merge
			s, e = min(s, sp[0]), max(e, sp[1])
		}
	}
	if !placed {
		out = append(out, [2]int64{s, e})
	}
	c.spans = out
}

// contiguous reports whether the captured bytes are exactly [0, n) for
// some n (an empty file counts).
func (c *coverage) contiguous() bool {
	return len(c.spans) == 0 || (len(c.spans) == 1 && c.spans[0][0] == 0)
}

// end is the offset one past the last captured byte.
func (c *coverage) end() int64 {
	if len(c.spans) == 0 {
		return 0
	}
	return c.spans[len(c.spans)-1][1]
}
