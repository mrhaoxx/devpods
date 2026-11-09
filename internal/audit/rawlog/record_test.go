// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package rawlog

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRecord_RoundTripAllKinds(t *testing.T) {
	base := time.Unix(1794210460, 123456789)
	var recs []Record
	for k := KindChannelOpen; k <= KindGap; k++ {
		recs = append(recs, Record{
			TS:      base.Add(time.Duration(k) * time.Nanosecond),
			Channel: uint32(k) * 1000,
			Kind:    k,
			Payload: bytes.Repeat([]byte{byte(k)}, int(k)*300),
		})
	}
	recs = append(recs, Record{TS: base, Channel: 7, Kind: KindDataIn}) // empty payload

	var buf []byte
	for _, r := range recs {
		buf = Append(buf, r)
	}
	rd := NewReader(bytes.NewReader(buf))
	for i, want := range recs {
		got, err := rd.Next()
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if !got.TS.Equal(want.TS) || got.Channel != want.Channel || got.Kind != want.Kind || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("record %d mismatch: got %+v", i, got)
		}
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

func TestReader_Truncated(t *testing.T) {
	buf := Append(nil, Record{TS: time.Now(), Channel: 1, Kind: KindDataOut, Payload: []byte("hello")})
	_, err := NewReader(bytes.NewReader(buf[:len(buf)-1])).Next()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want ErrUnexpectedEOF, got %v", err)
	}
}

func TestPayloadJSON_RoundTrip(t *testing.T) {
	in := Request{Dir: 1, Type: "exec", WantReply: true, OK: true, Payload: []byte{0, 1, 2}}
	var out Request
	if err := Decode(Encode(in), &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "exec" || !out.WantReply || !out.OK || !bytes.Equal(out.Payload, in.Payload) || out.Dir != 1 {
		t.Fatalf("got %+v", out)
	}
}
