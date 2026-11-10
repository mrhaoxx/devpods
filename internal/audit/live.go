// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/mrhaoxx/devpod/internal/audit/rawlog"
	"github.com/mrhaoxx/devpod/internal/audit/spool"
	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Live session viewing (audit spec §16): persistence + memory relay.
// A viewer gets the segments still on this gateway's disk (sealed ones
// waiting for upload and the open one, flushed at the hand-over) and,
// from the hand-over on, the channel's new records in memory. Older
// segments are in object storage. Nothing is kept for unwatched sessions.

var (
	// ErrNoSession: no open session with that ID on this gateway.
	ErrNoSession = errors.New("audit: no such open session")
	// ErrWatchOverflow: the viewer fell too far behind and was dropped;
	// it reconnects and relays again.
	ErrWatchOverflow = errors.New("audit: live viewer fell behind")
)

// watchQueueBytes bounds the payload one viewer may have queued.
var watchQueueBytes int64 = 4 << 20

const watchQueueRecords = 4096

type watcher struct {
	ch       uint32
	q        chan rawlog.Record
	queued   atomic.Int64
	overflow atomic.Bool
}

// LiveSegment is a raw segment read from local disk; Limit < 0 reads the
// whole (sealed) file, else the open segment's bytes at the hand-over.
type LiveSegment struct {
	Seq   uint32
	f     *os.File
	limit int64
}

// Watch is one live viewer of a channel.
type Watch struct {
	// From is the first segment served from local disk; segments before
	// it are in object storage. Cur is the segment open at the hand-over
	// (or the next one when none was open).
	From, Cur uint32
	Local     []LiveSegment

	s  *Session
	w  *watcher
	ch uint32
}

// Watch starts a live view of channel ch of the open session id.
func (r *Recorder) Watch(id string, ch uint32) (*Watch, error) {
	var s *Session
	r.mu.Lock()
	for x := range r.sessions {
		if x.meta.ID == id {
			s = x
			break
		}
	}
	r.mu.Unlock()
	if s == nil {
		return nil, ErrNoSession
	}
	w, err := s.watch(ch)
	if err != nil {
		return nil, err
	}
	if err := w.addSealed(r.spool, s); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

// watch registers the viewer at the hand-over point: under s.mu the open
// segment is flushed, so the file holds every earlier record and every
// later one goes to the viewer.
func (s *Session) watch(ch uint32) (*Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrNoSession
	}
	w := &Watch{s: s, ch: ch, w: &watcher{ch: ch, q: make(chan rawlog.Record, watchQueueRecords)}}
	if s.seg != nil && s.cur != nil {
		if err := s.seg.Flush(); err != nil {
			return nil, err
		}
		f, err := os.Open(s.cur.f.Path())
		if err != nil {
			return nil, err
		}
		w.Cur = s.cur.seq
		w.Local = []LiveSegment{{Seq: s.cur.seq, f: f, limit: s.cur.f.Size()}}
	} else {
		w.Cur = s.segSeq + 1
	}
	s.watchers = append(s.watchers, w.w)
	return w, nil
}

// addSealed opens the session's sealed segments still in the spool: the
// contiguous run just below Cur (uploads go oldest first, so everything
// below the run is in object storage). An open file stays readable when
// the uploader removes it.
func (w *Watch) addSealed(sp *spool.Spool, s *Session) error {
	prefix := strings.TrimSuffix(store.RawKey(s.meta.OpenedAt, s.meta.ID, 0), "000000.seg.zst")
	for attempt := 0; ; attempt++ {
		items, err := sp.Ready()
		if err != nil {
			return err
		}
		type cand struct {
			seq  uint32
			path string
		}
		var cs []cand
		for _, it := range items {
			if it.Kind != spool.ItemObject || !strings.HasPrefix(it.ObjectKey, prefix) {
				continue
			}
			n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(it.ObjectKey, prefix), ".seg.zst"), 10, 32)
			if err != nil || uint32(n) >= w.Cur {
				continue
			}
			cs = append(cs, cand{uint32(n), it.Path})
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].seq > cs[j].seq })
		var opened []LiveSegment
		want, failed := w.Cur-1, false
		for _, c := range cs {
			if c.seq != want {
				break
			}
			f, err := os.Open(c.path)
			if err != nil {
				failed = true // uploaded meanwhile: list again
				break
			}
			opened = append(opened, LiveSegment{Seq: c.seq, f: f, limit: -1})
			want--
		}
		if failed && attempt < 3 {
			for _, l := range opened {
				_ = l.f.Close()
			}
			continue
		}
		for i := len(opened) - 1; i >= 0; i-- {
			w.Local = append([]LiveSegment{opened[i]}, w.Local...)
		}
		break
	}
	w.From = w.Cur
	if len(w.Local) > 0 {
		w.From = w.Local[0].Seq
	}
	return nil
}

// Backlog calls fn with the channel's records from the local segments,
// oldest first.
func (w *Watch) Backlog(fn func(rawlog.Record) error) error {
	for _, seg := range w.Local {
		var src io.Reader = seg.f
		if seg.limit >= 0 {
			src = io.LimitReader(seg.f, seg.limit)
		}
		rd, err := rawlog.OpenSegment(src)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				continue // nothing flushed yet
			}
			return err
		}
		for {
			rec, err := rd.Next()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					break // the open segment ends mid-frame after the flush point
				}
				return err
			}
			if rec.Channel != w.ch {
				continue
			}
			if err := fn(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// Next returns the channel's next live record: io.EOF once the channel
// or the session has ended, ErrWatchOverflow when the viewer was dropped.
func (w *Watch) Next(ctx context.Context) (rawlog.Record, error) {
	select {
	case rec, ok := <-w.w.q:
		if !ok {
			if w.w.overflow.Load() {
				return rawlog.Record{}, ErrWatchOverflow
			}
			return rawlog.Record{}, io.EOF
		}
		w.w.queued.Add(-int64(len(rec.Payload)))
		return rec, nil
	case <-ctx.Done():
		return rawlog.Record{}, ctx.Err()
	}
}

// Close stops the view and releases its files.
func (w *Watch) Close() {
	w.s.unwatch(w.w)
	for _, l := range w.Local {
		_ = l.f.Close()
	}
}

func (s *Session) unwatch(wt *watcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.watchers {
		if x == wt {
			s.watchers = append(s.watchers[:i], s.watchers[i+1:]...)
			return
		}
	}
}

// deliverLocked hands rec to the viewers of its channel; s.mu held. A
// viewer that cannot take it is dropped, never waited for.
func (s *Session) deliverLocked(rec rawlog.Record) {
	if len(s.watchers) == 0 {
		return
	}
	kept := s.watchers[:0]
	for _, w := range s.watchers {
		if w.ch != rec.Channel {
			kept = append(kept, w)
			continue
		}
		n := int64(len(rec.Payload))
		if w.queued.Load()+n > watchQueueBytes {
			w.overflow.Store(true)
			close(w.q)
			continue
		}
		cp := rec
		cp.Payload = append([]byte(nil), rec.Payload...)
		select {
		case w.q <- cp:
			w.queued.Add(n)
		default:
			w.overflow.Store(true)
			close(w.q)
			continue
		}
		if rec.Kind == rawlog.KindChannelClose {
			close(w.q)
			continue
		}
		kept = append(kept, w)
	}
	for i := len(kept); i < len(s.watchers); i++ {
		s.watchers[i] = nil
	}
	s.watchers = kept
}

// endWatchersLocked ends every view when the session closes; s.mu held.
func (s *Session) endWatchersLocked() {
	for _, w := range s.watchers {
		close(w.q)
	}
	s.watchers = nil
}
