// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"time"
)

// RawKey is the S3 key of raw segment seq of a session opened at opened.
func RawKey(opened time.Time, session string, seq uint32) string {
	return fmt.Sprintf("raw/%s/%s/%06d.seg.zst", opened.UTC().Format("2006/01/02"), session, seq)
}

// FileKey is the S3 key of the n-th transfer captured on channel.
func FileKey(opened time.Time, session string, channel uint32, n int) string {
	return fmt.Sprintf("files/%s/%s/%d-%d", opened.UTC().Format("2006/01/02"), session, channel, n)
}
