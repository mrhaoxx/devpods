// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package spool is the gateway's local disk buffer for audit data:
// sealed objects (raw segments, file contents) and row batches wait here
// until the uploader has pushed them to S3 / ClickHouse.
//
// Layout under the spool directory:
//
//	tmp/<ulid>.part             content being written (discarded on restart)
//	ready/obj/<ulid>.bin        sealed object; <ulid>.key holds its S3 key
//	ready/batch/<ulid>.json     a store.Batch; the ULID is its dedup token
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mrhaoxx/devpod/internal/audit/store"
)

// Level is how full the spool is.
type Level int

const (
	LevelOK   Level = iota
	LevelWarn       // ≥ 80 %
	LevelHigh       // ≥ 90 %
	LevelFull       // the content budget (max − max/20) is exhausted
)

func (l Level) String() string {
	return [...]string{"ok", "warn", "high", "full"}[l]
}

// ErrFull is returned when content would eat into the event reserve, or
// when even a batch no longer fits.
var ErrFull = errors.New("spool full")

// Spool accounts disk usage of the spool directory.
type Spool struct {
	dir     string
	max     int64
	reserve int64
	usage   atomic.Int64
}

// Open prepares dir, discards unsealed leftovers and counts what is
// already waiting (crash recovery).
func Open(dir string, maxBytes int64) (*Spool, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("spool: max bytes must be positive")
	}
	s := &Spool{dir: dir, max: maxBytes, reserve: maxBytes / 20}
	if err := os.RemoveAll(filepath.Join(dir, "tmp")); err != nil {
		return nil, err
	}
	for _, d := range []string{"tmp", "ready/obj", "ready/batch"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	var total int64
	err := filepath.WalkDir(filepath.Join(dir, "ready"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.usage.Store(total)
	return s, nil
}

// Usage is the number of bytes currently on disk.
func (s *Spool) Usage() int64 { return s.usage.Load() }

// Level classifies Usage.
func (s *Spool) Level() Level {
	u := s.usage.Load()
	switch {
	case u >= s.max-s.reserve:
		return LevelFull
	case u*10 >= s.max*9:
		return LevelHigh
	case u*10 >= s.max*8:
		return LevelWarn
	}
	return LevelOK
}

// grow reserves n more bytes of content budget.
func (s *Spool) grow(n int64, limit int64) error {
	for {
		u := s.usage.Load()
		if u+n > limit {
			return ErrFull
		}
		if s.usage.CompareAndSwap(u, u+n) {
			return nil
		}
	}
}

// Create opens a content file that will become the object objectKey.
func (s *Spool) Create(objectKey string) (*File, error) {
	if s.Level() == LevelFull {
		return nil, ErrFull
	}
	id := ulid.Make().String()
	path := filepath.Join(s.dir, "tmp", id+".part")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &File{s: s, f: f, id: id, key: objectKey, path: path}, nil
}

// File is a content file in the spool. Writes are accounted against the
// content budget; a write that does not fit fails with ErrFull.
type File struct {
	s    *Spool
	f    *os.File
	id   string
	key  string
	path string

	mu   sync.Mutex
	size int64
	done bool
}

func (f *File) account(end int64) error {
	if end <= f.size {
		return nil
	}
	if err := f.s.grow(end-f.size, f.s.max-f.s.reserve); err != nil {
		return err
	}
	f.size = end
	return nil
}

// Write appends p.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.account(f.size + int64(len(p))); err != nil {
		return 0, err
	}
	return f.f.WriteAt(p, f.size-int64(len(p)))
}

// WriteAt writes p at off (random access, for SFTP reassembly).
func (f *File) WriteAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.account(off + int64(len(p))); err != nil {
		return 0, err
	}
	return f.f.WriteAt(p, off)
}

// Size is the current file size.
func (f *File) Size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.size
}

// Path is the on-disk path while the file is open (for hashing).
func (f *File) Path() string { return f.path }

// Seal makes the file ready for upload.
func (f *File) Seal() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return nil
	}
	f.done = true
	if err := f.f.Close(); err != nil {
		return err
	}
	keyPath := filepath.Join(f.s.dir, "ready/obj", f.id+".key")
	if err := os.WriteFile(keyPath, []byte(f.key), 0o600); err != nil {
		return err
	}
	f.s.usage.Add(int64(len(f.key)))
	return os.Rename(f.path, filepath.Join(f.s.dir, "ready/obj", f.id+".bin"))
}

// Discard drops the file and returns its budget.
func (f *File) Discard() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return nil
	}
	f.done = true
	_ = f.f.Close()
	f.s.usage.Add(-f.size)
	return os.Remove(f.path)
}

// WriteBatch spools a row batch; batches may use the event reserve.
func (s *Spool) WriteBatch(b store.Batch) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if err := s.grow(int64(len(data)), s.max); err != nil {
		return err
	}
	id := ulid.Make().String()
	tmp := filepath.Join(s.dir, "tmp", id+".json")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.usage.Add(-int64(len(data)))
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "ready/batch", id+".json"))
}

// ItemKind tells objects from batches.
type ItemKind uint8

const (
	ItemObject ItemKind = iota + 1
	ItemBatch
)

// Item is one ready upload.
type Item struct {
	Kind      ItemKind
	Path      string
	ObjectKey string // ItemObject
	Token     string // ItemBatch: the dedup token (batch ULID)
	Size      int64
	Created   time.Time
}

// Ready lists sealed items, oldest first.
func (s *Spool) Ready() ([]Item, error) {
	var items []Item
	objs, err := os.ReadDir(filepath.Join(s.dir, "ready/obj"))
	if err != nil {
		return nil, err
	}
	for _, e := range objs {
		id, ok := strings.CutSuffix(e.Name(), ".bin")
		if !ok {
			continue
		}
		key, err := os.ReadFile(filepath.Join(s.dir, "ready/obj", id+".key"))
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, Item{Kind: ItemObject, Path: filepath.Join(s.dir, "ready/obj", e.Name()), ObjectKey: string(key), Token: id, Size: info.Size(), Created: ulidTime(id)})
	}
	batches, err := os.ReadDir(filepath.Join(s.dir, "ready/batch"))
	if err != nil {
		return nil, err
	}
	for _, e := range batches {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, Item{Kind: ItemBatch, Path: filepath.Join(s.dir, "ready/batch", e.Name()), Token: id, Size: info.Size(), Created: ulidTime(id)})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Token < items[j].Token })
	return items, nil
}

// Done removes an uploaded item and returns its budget.
func (s *Spool) Done(it Item) error {
	freed := it.Size
	if err := os.Remove(it.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if it.Kind == ItemObject {
		keyPath := strings.TrimSuffix(it.Path, ".bin") + ".key"
		freed += int64(len(it.ObjectKey))
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	s.usage.Add(-freed)
	return nil
}

// Quarantine moves an item that can never be uploaded (a batch that does
// not parse) out of the queue, into failed/ beside it, for whoever looks
// into it; it no longer counts against the budget (failed/ is not read
// again).
func (s *Spool) Quarantine(it Item) error {
	dir := filepath.Join(s.dir, "failed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(it.Path, filepath.Join(dir, filepath.Base(it.Path))); err != nil {
		return err
	}
	freed := it.Size
	if it.Kind == ItemObject {
		keyPath := strings.TrimSuffix(it.Path, ".bin") + ".key"
		freed += int64(len(it.ObjectKey))
		_ = os.Rename(keyPath, filepath.Join(dir, filepath.Base(keyPath)))
	}
	s.usage.Add(-freed)
	return nil
}

func ulidTime(id string) time.Time {
	u, err := ulid.Parse(id)
	if err != nil {
		return time.Time{}
	}
	return ulid.Time(u.Time())
}
