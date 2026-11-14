// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"
)

var columnCache sync.Map // reflect.Type → string

// columns lists the ClickHouse columns of row type T (its ch tags, in
// field order, embedded structs included). Reads and writes name their
// columns: a table with more columns than a reader or writer knows (a
// later version added them) is read and written all the same.
func columns[T any]() string {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if v, ok := columnCache.Load(t); ok {
		return v.(string)
	}
	var names []string
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			if tag := f.Tag.Get("ch"); tag != "" && tag != "-" {
				names = append(names, tag)
			}
		}
	}
	walk(t)
	s := strings.Join(names, ", ")
	columnCache.Store(t, s)
	return s
}

// tableCols caches the columns each table has.
type tableCols struct {
	mu sync.Mutex
	m  map[string]colSet
}

type colSet struct {
	names map[string]bool
	at    time.Time
}

// colsFresh: how long a table's columns are taken as read. A migration
// that adds one is seen within this; until then the column is left out.
var colsFresh = time.Minute

// known is cols (a columns list) without those table does not have yet:
// a console or gateway newer than the schema reads and writes the
// columns there are, and the rest once a migration adds them. When the
// table's columns cannot be read, cols is used as it is.
func (c *CH) known(ctx context.Context, table, cols string) string {
	if c.tables == nil {
		return cols
	}
	c.tables.mu.Lock()
	e, ok := c.tables.m[table]
	c.tables.mu.Unlock()
	if !ok || time.Since(e.at) > colsFresh {
		rows, err := c.conn.Query(ctx, "SELECT name FROM system.columns WHERE database = ? AND table = ?", c.db, table)
		if err != nil {
			return cols
		}
		names := map[string]bool{}
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				names[n] = true
			}
		}
		rows.Close()
		if len(names) == 0 { // no such table (yet): say all, and let the query tell
			return cols
		}
		e = colSet{names: names, at: time.Now()}
		c.tables.mu.Lock()
		c.tables.m[table] = e
		c.tables.mu.Unlock()
	}
	var keep []string
	for _, n := range strings.Split(cols, ", ") {
		if e.names[n] {
			keep = append(keep, n)
		}
	}
	return strings.Join(keep, ", ")
}
