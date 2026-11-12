// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"reflect"
	"strings"
	"sync"
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
