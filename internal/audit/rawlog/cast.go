// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package rawlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrhaoxx/devpod/internal/audit/sshmsg"
)

type castHeader struct {
	Version   int               `json:"version"`
	Width     uint32            `json:"width"`
	Height    uint32            `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Env       map[string]string `json:"env,omitempty"`
}

// WriteCast converts one channel of a record stream into asciicast v2.
// Output (stdout, stderr, gateway notices) becomes "o" events, client
// input "i" events and window-change requests "r" events; times are
// seconds since the channel was opened. Records are streamed: only the
// few records before the channel starts a program are buffered, to learn
// the terminal size for the header.
func WriteCast(w io.Writer, it Iterator, channel uint32) error {
	bw := bufio.NewWriter(w)
	c := &castWriter{w: bw, width: 80, height: 24}
	for {
		r, err := it.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if r.Channel != channel {
			continue
		}
		if err := c.add(r); err != nil {
			return err
		}
	}
	if err := c.finish(); err != nil {
		return err
	}
	return bw.Flush()
}

type castWriter struct {
	w             *bufio.Writer
	opened        time.Time
	width, height uint32
	term          string
	headerDone    bool
	pending       []Record
	carry         map[string][]byte
}

func (c *castWriter) add(r Record) error {
	switch r.Kind {
	case KindChannelOpen:
		if c.opened.IsZero() {
			c.opened = r.TS
		}
		return nil
	case KindRequest:
		var req Request
		if err := Decode(r.Payload, &req); err != nil {
			return nil // a corrupt metadata record must not break replay
		}
		switch req.Type {
		case "pty-req":
			if p, err := sshmsg.ParsePtyReq(req.Payload); err == nil {
				c.width, c.height, c.term = p.Cols, p.Rows, p.Term
			}
			return nil
		case "shell", "exec", "subsystem":
			return c.header()
		case "window-change":
			cols, rows, err := sshmsg.ParseWindowChange(req.Payload)
			if err != nil {
				return nil
			}
			return c.emit(r, "r", fmt.Sprintf("%dx%d", cols, rows))
		}
		return nil
	case KindDataOut, KindDataErr, KindInjectToClient:
		return c.text(r, "o")
	case KindDataIn:
		return c.text(r, "i")
	}
	return nil
}

func (c *castWriter) header() error {
	if c.headerDone {
		return nil
	}
	c.headerDone = true
	if c.opened.IsZero() && len(c.pending) > 0 {
		c.opened = c.pending[0].TS
	}
	h := castHeader{Version: 2, Width: c.width, Height: c.height, Timestamp: c.opened.Unix()}
	if c.term != "" {
		h.Env = map[string]string{"TERM": c.term}
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	pending := c.pending
	c.pending = nil
	for _, r := range pending {
		if err := c.add(r); err != nil {
			return err
		}
	}
	return nil
}

func (c *castWriter) text(r Record, code string) error {
	if !c.headerDone {
		c.pending = append(c.pending, r)
		// Data before any start request (unusual) still forces a header.
		return c.header()
	}
	if c.carry == nil {
		c.carry = map[string][]byte{}
	}
	buf := append(c.carry[code], r.Payload...)
	cut := incompleteTail(buf)
	c.carry[code] = append([]byte(nil), buf[cut:]...)
	if cut == 0 {
		return nil
	}
	return c.emit(r, code, strings.ToValidUTF8(string(buf[:cut]), "�"))
}

func (c *castWriter) finish() error {
	if !c.headerDone {
		if err := c.header(); err != nil {
			return err
		}
	}
	for _, code := range []string{"o", "i"} {
		if rest := c.carry[code]; len(rest) > 0 {
			c.carry[code] = nil
			last := Record{TS: c.opened}
			if err := c.emit(last, code, strings.ToValidUTF8(string(rest), "�")); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *castWriter) emit(r Record, code, data string) error {
	t := r.TS.Sub(c.opened).Seconds()
	if t < 0 {
		t = 0
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return err
	}
	s := bytes.TrimRight(b.Bytes(), "\n")
	_, err := fmt.Fprintf(c.w, "[%s, %q, %s]\n", strconv.FormatFloat(t, 'f', 6, 64), code, s)
	return err
}

// incompleteTail returns the index where a trailing, not-yet-complete
// UTF-8 sequence starts (len(b) when the buffer ends cleanly).
func incompleteTail(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		c := b[i]
		if c < 0x80 {
			return len(b) // ASCII: nothing pending
		}
		if c >= 0xC0 { // leading byte
			need := 2
			switch {
			case c >= 0xF0:
				need = 4
			case c >= 0xE0:
				need = 3
			}
			if len(b)-i < need && c < 0xF8 {
				return i
			}
			return len(b)
		}
	}
	return len(b)
}

// TermSize reports the terminal size a pty-req or window-change request
// record sets.
func TermSize(r Record) (cols, rows uint32, ok bool) {
	if r.Kind != KindRequest {
		return 0, 0, false
	}
	var req Request
	if err := Decode(r.Payload, &req); err != nil {
		return 0, 0, false
	}
	switch req.Type {
	case "pty-req":
		p, err := sshmsg.ParsePtyReq(req.Payload)
		if err != nil {
			return 0, 0, false
		}
		return p.Cols, p.Rows, true
	case "window-change":
		c, r, err := sshmsg.ParseWindowChange(req.Payload)
		if err != nil {
			return 0, 0, false
		}
		return c, r, true
	}
	return 0, 0, false
}
