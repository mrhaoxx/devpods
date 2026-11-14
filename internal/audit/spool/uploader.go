// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Uploader empties the spool into object storage and the event store.
//
// Objects (raw segments, captured files) go several at a time, each on
// its own: one that fails waits out its own backoff, and the others go
// on — a session printing gigabytes, or one object S3 keeps refusing,
// does not hold everything else. A batch of rows goes as soon as the
// objects it names are up (its rows point at them), not after every
// object waiting. A batch that cannot be read is put aside (Quarantine)
// with an alert, instead of stopping the queue for good.
type Uploader struct {
	Spool   *Spool
	Objects store.ObjectStore
	Events  store.EventStore

	Interval time.Duration                   // how often the queue is looked at; default 1s
	Backoff  func(attempt int) time.Duration // per item; default 1s·2^(n-1), max 60s
	OnLag    func(seconds float64)           // age of the oldest ready item
	// Workers: objects uploaded at once; default 4.
	Workers int

	mu       sync.Mutex
	state    map[string]*retry // by path: items that failed
	inflight map[string]bool   // by path
	slots    chan struct{}     // one per object going
}

// retry is an item's failures: how many in a row, and when to try again.
type retry struct {
	attempts int
	next     time.Time
	err      error
}

func defaultBackoff(n int) time.Duration {
	d := time.Second
	for i := 1; i < n && d < time.Minute; i++ {
		d *= 2
	}
	return min(d, time.Minute)
}

// stuckAttempts: an object failing this many times in a row no longer
// holds the batches that name it (their rows are worth more seen now
// than complete later; the object still goes when it can).
const stuckAttempts = 5

func (u *Uploader) init() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state == nil {
		u.state = map[string]*retry{}
		u.inflight = map[string]bool{}
	}
	if u.Workers <= 0 {
		u.Workers = 4
	}
	if u.slots == nil {
		u.slots = make(chan struct{}, u.Workers)
	}
	if u.Backoff == nil {
		u.Backoff = defaultBackoff
	}
	if u.Interval <= 0 {
		u.Interval = time.Second
	}
}

// Run uploads until ctx is done.
func (u *Uploader) Run(ctx context.Context) {
	u.init()
	tick := time.NewTicker(u.Interval)
	defer tick.Stop()
	for {
		_ = u.round(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Flush tries every ready item now, whatever its backoff, and returns
// once each was tried: nil when all went, else the first error.
func (u *Uploader) Flush(ctx context.Context) error {
	u.init()
	return u.round(ctx, true)
}

// round: the ready objects due go (Workers at once, in the background
// under Run), then the ready batches whose objects are up.
func (u *Uploader) round(ctx context.Context, all bool) error {
	items, err := u.Spool.Ready()
	if err != nil {
		return err
	}
	if u.OnLag != nil {
		var oldest time.Time
		for _, it := range items {
			if !it.Created.IsZero() && (oldest.IsZero() || it.Created.Before(oldest)) {
				oldest = it.Created
			}
		}
		lag := 0.0
		if !oldest.IsZero() {
			lag = time.Since(oldest).Seconds()
		}
		u.OnLag(lag)
	}
	var objs, batches []Item
	for _, it := range items {
		if it.Kind == ItemObject {
			objs = append(objs, it)
		} else {
			batches = append(batches, it)
		}
	}

	var errMu sync.Mutex
	var first error
	fail := func(err error) {
		errMu.Lock()
		if first == nil {
			first = err
		}
		errMu.Unlock()
	}

	// objects: each due one not already going, Workers at once. Run
	// starts as many as there are free slots and leaves the rest to the
	// next round (so the batches below are not kept waiting); Flush
	// waits for slots and for every one it started.
	now := time.Now()
	var wg sync.WaitGroup
dispatch:
	for _, it := range objs {
		if all {
			u.slots <- struct{}{}
		} else {
			select {
			case u.slots <- struct{}{}:
			default:
				break dispatch
			}
		}
		if !u.take(it, all, now) {
			<-u.slots
			continue
		}
		wg.Add(1)
		go func(it Item) {
			defer func() { <-u.slots; wg.Done() }()
			if err := u.put(ctx, it); err != nil {
				fail(err)
			}
		}(it)
	}
	if all {
		wg.Wait()
	}

	// batches: those whose objects are up (or stuck), in order
	pending := map[string]bool{}
	u.mu.Lock()
	for _, it := range objs {
		if st := u.state[it.Path]; st == nil || st.attempts < stuckAttempts || u.inflight[it.Path] {
			if _, err := os.Stat(it.Path); err == nil {
				pending[it.ObjectKey] = true
			}
		}
	}
	u.mu.Unlock()
	for _, it := range batches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !u.take(it, all, time.Now()) {
			continue
		}
		if err := u.insert(ctx, it, pending); err != nil {
			fail(err)
		}
	}
	// (under Run, objects may still be going and report into first)
	errMu.Lock()
	defer errMu.Unlock()
	return first
}

// take marks it as going when it is due (always under Flush) and not
// going already.
func (u *Uploader) take(it Item, all bool, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.inflight[it.Path] {
		return false
	}
	if st := u.state[it.Path]; st != nil && !all && now.Before(st.next) {
		return false
	}
	u.inflight[it.Path] = true
	return true
}

// done ends an item's turn: err nil forgets its failures, else counts one
// and sets when it is tried again.
func (u *Uploader) done(it Item, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.inflight, it.Path)
	if err == nil {
		delete(u.state, it.Path)
		return
	}
	st := u.state[it.Path]
	if st == nil {
		st = &retry{}
		u.state[it.Path] = st
	}
	st.attempts++
	st.err = err
	st.next = time.Now().Add(u.Backoff(st.attempts))
	slog.Warn("audit_upload", "item", it.Token, "err", err.Error(), "attempt", st.attempts, "retry_in", time.Until(st.next).Round(time.Second).String())
}

func (u *Uploader) put(ctx context.Context, it Item) (err error) {
	defer func() { u.done(it, err) }()
	f, err := os.Open(it.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // gone already (another round's Done)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := u.Objects.Put(ctx, it.ObjectKey, f, it.Size); err != nil {
		return fmt.Errorf("put object %s: %w", it.ObjectKey, err)
	}
	return u.Spool.Done(it)
}

// insert writes a batch whose objects are up; one that names an object
// still waiting stays for a later round (no failure counted).
func (u *Uploader) insert(ctx context.Context, it Item, pending map[string]bool) (err error) {
	data, err := os.ReadFile(it.Path)
	if errors.Is(err, os.ErrNotExist) {
		u.done(it, nil)
		return nil
	}
	if err != nil {
		u.done(it, err)
		return err
	}
	var b store.Batch
	if err := json.Unmarshal(data, &b); err != nil {
		// it never will: aside, said, and the queue goes on
		u.done(it, nil)
		if qerr := u.Spool.Quarantine(it); qerr != nil {
			return errors.Join(err, qerr)
		}
		slog.Error("audit_batch_quarantined", "batch", it.Token, "err", err.Error())
		_ = u.Spool.WriteBatch(store.Batch{Alerts: []store.AlertRow{{
			TS: time.Now(), Kind: "audit_upload_failed", Source: "gateway",
			Detail: fmt.Sprintf("audit batch %s could not be read and was set aside: %v", it.Token, err),
		}}})
		return nil
	}
	for _, key := range objectKeys(b) {
		if pending[key] {
			u.done(it, nil) // its turn comes once the object is up
			return nil
		}
	}
	if err := u.Events.Insert(ctx, b, it.Token); err != nil {
		err = fmt.Errorf("insert batch %s: %w", it.Token, err)
		u.done(it, err)
		return err
	}
	u.done(it, nil)
	return u.Spool.Done(it)
}

// objectKeys: the objects a batch's rows point at.
func objectKeys(b store.Batch) []string {
	var out []string
	for _, r := range b.RawSegments {
		if r.S3Key != "" {
			out = append(out, r.S3Key)
		}
	}
	for _, f := range b.FileEvents {
		if f.S3Key != "" {
			out = append(out, f.S3Key)
		}
	}
	return out
}
