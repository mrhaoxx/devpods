// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package tunnel connects a party that can only dial out (an agent in a
// cluster, a gateway) to every instance of a server, so that the server
// can open streams back to it:
//
//   - one control connection per instance, for heartbeats;
//   - a pool of idle data connections per instance. The server puts one to
//     use by writing what it is for on that very connection; from then on
//     it is a plain byte stream, and the initiator dials a replacement.
//
// Each stream is a connection of its own: nothing is multiplexed, one
// stream cannot hold up another, and a broken connection takes one stream
// with it. The instances of a server sit behind one address; every
// handshake names the instance reached and all the instances there are,
// and the initiator keeps dialling until each has what it should — so
// instances never forward to, or know of, each other.
package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Hello says who an initiator is.
type Hello struct {
	Kind string `json:"kind"` // "agent", "gateway"
	Name string `json:"name"`
}

const (
	roleControl = "control"
	roleData    = "data"
	maxFrame    = 64 << 10
)

// serverHello opens every connection.
type serverHello struct {
	Instance  string   `json:"instance"`
	Instances []string `json:"instances"`
	Nonce     []byte   `json:"nonce"`
}

type clientHello struct {
	// Nonce, when set, asks the server to prove itself over it.
	Nonce []byte `json:"nonce,omitempty"`
	Hello Hello  `json:"hello"`
	Role  string `json:"role"`
	Proof []byte `json:"proof"`
}

type result struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Proof []byte `json:"proof,omitempty"` // the server's, over the initiator's nonce
}

// ctrlMsg is all a control connection carries after the handshake.
type ctrlMsg struct {
	T string `json:"t"` // "ping", "pong"
	// N numbers a server's ping; the pong says it back, and the server
	// knows the round trip. (An initiator's pings carry none: the server
	// answers those without asking.)
	N uint64 `json:"n,omitempty"`
}

// useMsg is what a data connection carries before it becomes a stream:
// while it is idle, a word now and then that it is still there (Idle), so
// that neither end, nor anything in between, takes it for dead; then, once,
// what it is taken for.
type useMsg struct {
	Idle    bool   `json:"idle,omitempty"`
	Purpose []byte `json:"purpose,omitempty"`
}

func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return fmt.Errorf("tunnel: frame of %d bytes", len(b))
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// readFrame reads exactly one frame and not a byte more: what follows on
// a data connection belongs to the stream.
func readFrame(r io.Reader, v any) error {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n > maxFrame {
		return errors.New("tunnel: frame too large")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
