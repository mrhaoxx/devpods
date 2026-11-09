// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package rawlog is the audit raw-stream format: a sequence of framed
// records describing everything that crossed one SSH session at the
// gateway. Segments of records are zstd-compressed and stored in S3;
// every derived view (replay, file events) can be rebuilt from them.
package rawlog

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Kind identifies a record.
type Kind uint8

const (
	KindChannelOpen     Kind = iota + 1 // payload: JSON ChannelOpen
	KindRequest                         // payload: JSON Request
	KindDataIn                          // raw bytes client→backend
	KindDataOut                         // raw bytes backend→client (stdout)
	KindDataErr                         // raw bytes backend→client (stderr)
	KindInjectToClient                  // raw bytes the gateway wrote to the client
	KindInjectToBackend                 // raw bytes the gateway wrote to the backend
	KindChannelClose                    // payload: JSON ChannelClose
	KindGap                             // payload: JSON Gap
)

// MaxPayload bounds a single record so a corrupt length cannot make the
// reader allocate unbounded memory.
const MaxPayload = 64 << 20

// Record is one framed entry.
type Record struct {
	TS      time.Time
	Channel uint32
	Kind    Kind
	Payload []byte
}

// ChannelOpen is the payload of KindChannelOpen.
type ChannelOpen struct {
	Dir   uint8  `json:"dir"`
	Type  string `json:"type"`
	Extra []byte `json:"extra,omitempty"`
}

// Request is the payload of KindRequest; written once the outcome is known.
type Request struct {
	Dir       uint8  `json:"dir"`
	Type      string `json:"type"`
	WantReply bool   `json:"want"`
	OK        bool   `json:"ok"`
	Denied    bool   `json:"denied,omitempty"`
	Payload   []byte `json:"payload,omitempty"`
}

// ChannelClose is the payload of KindChannelClose.
type ChannelClose struct {
	BytesIn  int64 `json:"in"`
	BytesOut int64 `json:"out"`
}

// Gap is the payload of KindGap: data that could not be recorded.
type Gap struct {
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
}

// Encode marshals a metadata payload.
func Encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // payload structs are plain data
	}
	return b
}

// Decode unmarshals a metadata payload.
func Decode(p []byte, v any) error { return json.Unmarshal(p, v) }

// Append frames r onto dst:
// uvarint(unix nanos) uvarint(channel) byte(kind) uvarint(len) payload.
func Append(dst []byte, r Record) []byte {
	dst = binary.AppendUvarint(dst, uint64(r.TS.UnixNano()))
	dst = binary.AppendUvarint(dst, uint64(r.Channel))
	dst = append(dst, byte(r.Kind))
	dst = binary.AppendUvarint(dst, uint64(len(r.Payload)))
	return append(dst, r.Payload...)
}

// Reader decodes framed records.
type Reader struct {
	br *bufio.Reader
}

// NewReader reads records from r.
func NewReader(r io.Reader) *Reader {
	if br, ok := r.(*bufio.Reader); ok {
		return &Reader{br: br}
	}
	return &Reader{br: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next record, io.EOF at a clean end, and
// io.ErrUnexpectedEOF when the stream stops inside a record.
func (r *Reader) Next() (Record, error) {
	ts, err := binary.ReadUvarint(r.br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Record{}, io.EOF
		}
		return Record{}, err
	}
	ch, err := binary.ReadUvarint(r.br)
	if err != nil {
		return Record{}, unexpected(err)
	}
	kind, err := r.br.ReadByte()
	if err != nil {
		return Record{}, unexpected(err)
	}
	n, err := binary.ReadUvarint(r.br)
	if err != nil {
		return Record{}, unexpected(err)
	}
	if n > MaxPayload {
		return Record{}, fmt.Errorf("rawlog: record payload %d exceeds limit", n)
	}
	var payload []byte
	if n > 0 {
		payload = make([]byte, n)
		if _, err := io.ReadFull(r.br, payload); err != nil {
			return Record{}, unexpected(err)
		}
	}
	return Record{TS: time.Unix(0, int64(ts)), Channel: uint32(ch), Kind: Kind(kind), Payload: payload}, nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
