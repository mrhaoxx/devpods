// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"sync"
	"time"
)

// Guessing a password is made slow, per user name: the first
// freeLoginFailures failures in a row cost nothing more than the usual
// delay; each one after makes the next try wait twice as long as the one
// before, from a second up to maxLoginWait, and a try during the wait is
// refused unheard (the right password too). A sign-in clears it.
//
// Per name, not per address: the console sits behind proxies, so every
// request may come from the same one. Kept in memory, per replica: with
// two, a guesser gets twice the tries; a restart forgets.
const (
	freeLoginFailures = 5
	maxLoginWait      = 10 * time.Minute
	// a name not failed for this long is forgotten
	loginFailureMemory = 24 * time.Hour
)

// loginWait is the wait after n failures in a row.
func loginWait(n int) time.Duration {
	if n <= freeLoginFailures {
		return 0
	}
	w := time.Second
	for i := freeLoginFailures + 1; i < n && w < maxLoginWait; i++ {
		w *= 2
	}
	return min(w, maxLoginWait)
}

type loginGuard struct {
	mu    sync.Mutex
	names map[string]*loginFailures
}

type loginFailures struct {
	n     int       // failures in a row
	until time.Time // no try is heard before
	last  time.Time
}

// wait: how long before name may try again (0: now).
func (g *loginGuard) wait(name string, now time.Time) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f := g.names[name]; f != nil && now.Before(f.until) {
		return f.until.Sub(now)
	}
	return 0
}

// failed counts a failure of name's.
func (g *loginGuard) failed(name string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.names == nil {
		g.names = map[string]*loginFailures{}
	}
	for k, f := range g.names { // forget the long quiet
		if now.Sub(f.last) > loginFailureMemory {
			delete(g.names, k)
		}
	}
	f := g.names[name]
	if f == nil {
		f = &loginFailures{}
		g.names[name] = f
	}
	f.n++
	f.last = now
	f.until = now.Add(loginWait(f.n))
}

// succeeded clears name's failures.
func (g *loginGuard) succeeded(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.names, name)
}
