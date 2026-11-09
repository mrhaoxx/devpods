// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package spool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Uploader drains a Spool into S3 and ClickHouse.
type Uploader struct {
	Spool   *Spool
	Objects store.ObjectStore
	Events  store.EventStore

	Interval time.Duration                   // poll period; default 1s
	Backoff  func(attempt int) time.Duration // default 1s·2^(n-1), max 60s
	OnLag    func(seconds float64)           // age of the oldest ready item
}

func defaultBackoff(n int) time.Duration {
	d := time.Second
	for i := 1; i < n && d < time.Minute; i++ {
		d *= 2
	}
	return min(d, time.Minute)
}

// Run uploads until ctx is done, backing off while uploads fail.
func (u *Uploader) Run(ctx context.Context) {
	interval := u.Interval
	if interval <= 0 {
		interval = time.Second
	}
	backoff := u.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	attempt := 0
	for {
		wait := interval
		if err := u.Flush(ctx); err != nil {
			attempt++
			wait = backoff(attempt)
		} else {
			attempt = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Flush uploads every ready item, stopping at the first failure (later
// items keep their order for the next attempt).
func (u *Uploader) Flush(ctx context.Context) error {
	items, err := u.Spool.Ready()
	if err != nil {
		return err
	}
	if u.OnLag != nil {
		lag := 0.0
		if len(items) > 0 && !items[0].Created.IsZero() {
			lag = time.Since(items[0].Created).Seconds()
		}
		u.OnLag(lag)
	}
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := u.upload(ctx, it); err != nil {
			return err
		}
		if err := u.Spool.Done(it); err != nil {
			return err
		}
	}
	return nil
}

func (u *Uploader) upload(ctx context.Context, it Item) error {
	switch it.Kind {
	case ItemObject:
		f, err := os.Open(it.Path)
		if err != nil {
			return err
		}
		defer f.Close()
		return u.Objects.Put(ctx, it.ObjectKey, f, it.Size)
	case ItemBatch:
		data, err := os.ReadFile(it.Path)
		if err != nil {
			return err
		}
		var b store.Batch
		if err := json.Unmarshal(data, &b); err != nil {
			return fmt.Errorf("spool batch %s: %w", it.Token, err)
		}
		return u.Events.Insert(ctx, b, it.Token)
	}
	return fmt.Errorf("spool: unknown item kind %d", it.Kind)
}
