// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"sync"
	"time"
)

// LinkStats is a connected peer's link as an instance sees it: since when,
// its round trip, and what its streams carry — in all and per second.
type LinkStats struct {
	Since time.Time `json:"since"`
	// RTTMillis: the latest ping's round trip; 0 when the peer has not
	// answered one (an initiator of before pings both ways never does).
	RTTMillis float64 `json:"rttMs,omitempty"`
	// RxRate and TxRate: bytes per second read from and written to the
	// peer's streams, over the time since the previous reading.
	RxRate float64 `json:"rxRate"`
	TxRate float64 `json:"txRate"`
	Rx     int64   `json:"rx"`
	Tx     int64   `json:"tx"`
	// Streams open now, and idle connections waiting to become one.
	Streams int `json:"streams"`
	Idle    int `json:"idle"`
}

// minSample: readings closer together than this keep the last rate.
const minSample = time.Second

type sample struct {
	since  time.Time // which presence of the peer it is of
	at     time.Time
	rx, tx int64
	rxRate float64
	txRate float64
}

type meter struct {
	mu   sync.Mutex
	last map[string]sample
}

// Stats is Peers with rates: each over the time since the previous call
// (at least a second; the first, since the peer connected).
func (s *Server) Stats() map[string]LinkStats {
	peers := s.Peers()
	now := time.Now()
	s.meter.mu.Lock()
	defer s.meter.mu.Unlock()
	if s.meter.last == nil {
		s.meter.last = map[string]sample{}
	}
	out := make(map[string]LinkStats, len(peers))
	for name, p := range peers {
		prev, ok := s.meter.last[name]
		first := !ok || !prev.since.Equal(p.Since) || p.Rx < prev.rx || p.Tx < prev.tx // new, or connected again
		if first {
			prev = sample{since: p.Since, at: p.Since}
		}
		cur := prev
		// the first reading is over the whole time since it connected
		if dt := now.Sub(prev.at); dt > 0 && (first || dt >= minSample) {
			sec := dt.Seconds()
			cur = sample{since: p.Since, at: now, rx: p.Rx, tx: p.Tx, rxRate: float64(p.Rx-prev.rx) / sec, txRate: float64(p.Tx-prev.tx) / sec}
		}
		s.meter.last[name] = cur
		out[name] = LinkStats{
			Since: p.Since, RTTMillis: float64(p.RTT.Microseconds()) / 1000,
			RxRate: cur.rxRate, TxRate: cur.txRate, Rx: p.Rx, Tx: p.Tx, Streams: p.Streams, Idle: p.Idle,
		}
	}
	for name := range s.meter.last {
		if _, ok := peers[name]; !ok {
			delete(s.meter.last, name)
		}
	}
	return out
}
