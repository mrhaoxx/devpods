// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

const (
	// uploadMaxBytes bounds one request's records (uncompressed JSON);
	// the compressed body stays far below the ingest's 4 MiB limit.
	uploadMaxBytes = 3584 << 10
	uploadMaxDelay = 30 * time.Second
	unauthDelay    = 60 * time.Second
)

// Uploader posts the buffer to the ingest endpoint, one request in
// flight, oldest seconds first.
type Uploader struct {
	URL       func() string // re-evaluated per request (config reload)
	TokenPath string        // re-read per request (token rotation)
	Client    *http.Client
	// Sleep waits d or until ctx ends (false); tests inject a fake.
	Sleep func(ctx context.Context, d time.Duration) bool

	lastUnauthLog time.Time
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Run uploads until ctx ends. header supplies the batch identity; Run
// sets FromSeq/Seq.
func (u *Uploader) Run(ctx context.Context, header func() wire.Header, b *Buffer) {
	if u.Client == nil {
		u.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if u.Sleep == nil {
		u.Sleep = sleepCtx
	}
	delay := time.Second
	for ctx.Err() == nil {
		from, to, recs := b.Pending(uploadMaxBytes)
		if len(recs) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-b.Notify():
			case <-time.After(time.Second):
			}
			continue
		}
		h := header()
		h.FromSeq, h.Seq = from, to
		h.TS = time.Now().UTC()
		ack, status, err := u.post(ctx, h, recs)
		switch {
		case err == nil && status/100 == 2:
			b.Ack(ack.AckedSeq)
			delay = time.Second
			continue
		case status == http.StatusUnauthorized:
			if time.Since(u.lastUnauthLog) > time.Minute {
				fmt.Fprintf(os.Stderr, "telemetry: ingest rejected the token (401); retrying in %v\n", unauthDelay)
				u.lastUnauthLog = time.Now()
			}
			if !u.Sleep(ctx, unauthDelay) {
				return
			}
		default:
			jitter := time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
			if !u.Sleep(ctx, jitter) {
				return
			}
			delay = min(delay*2, uploadMaxDelay)
		}
	}
}

func (u *Uploader) post(ctx context.Context, h wire.Header, recs []wire.Record) (wire.Ack, int, error) {
	var body bytes.Buffer
	if err := wire.Encode(&body, h, recs); err != nil {
		return wire.Ack{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.URL(), &body)
	if err != nil {
		return wire.Ack{}, 0, err
	}
	tok, err := os.ReadFile(u.TokenPath)
	if err != nil {
		return wire.Ack{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := u.Client.Do(req)
	if err != nil {
		return wire.Ack{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return wire.Ack{}, resp.StatusCode, fmt.Errorf("ingest: %s", resp.Status)
	}
	var ack wire.Ack
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return wire.Ack{}, resp.StatusCode, err
	}
	return ack, resp.StatusCode, nil
}
