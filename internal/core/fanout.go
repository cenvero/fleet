// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"sync"
	"sync/atomic"
)

// DefaultFanoutLimit is how many per-server operations one fleet process runs at
// once when it fans out across the fleet (top, health, inventory, exec on all
// servers, ...). It deliberately stays below the daemon's control-socket limit
// (maxConcurrentControlConnections = 32 in reverse.go): in reverse mode every
// in-flight call is one control connection, and connections beyond the limit
// are dropped, which used to make large fleets randomly show servers offline.
const DefaultFanoutLimit = 24

// ForEachLimit calls fn(i) for every i in [0, n) with at most limit calls in
// flight, and returns once every call has finished. Calls are started in index
// order; limit <= 0 means DefaultFanoutLimit, and limit == 1 runs the calls one
// after another, in order, exactly like a plain loop. fn must be safe for
// concurrent use when limit > 1 (writing to its own index of a pre-sized slice
// is the usual pattern).
func ForEachLimit(n, limit int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if limit <= 0 {
		limit = DefaultFanoutLimit
	}
	if limit > n {
		limit = n
	}
	if limit == 1 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(limit)
	for w := 0; w < limit; w++ {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// OrderedFlusher releases per-index results strictly in index order while the
// work itself completes in any order: Done(i) marks index i finished, then
// flushes every consecutive finished index starting at the lowest one not yet
// flushed. Use it to stream a concurrent fan-out's output in the same order a
// sequential loop would have produced it, as soon as each prefix is ready.
// flush is called with an internal lock held, so calls never overlap.
type OrderedFlusher struct {
	mu    sync.Mutex
	next  int
	done  []bool
	flush func(i int)
}

// NewOrderedFlusher returns a flusher for n indexes.
func NewOrderedFlusher(n int, flush func(i int)) *OrderedFlusher {
	return &OrderedFlusher{done: make([]bool, n), flush: flush}
}

// Done marks index i finished and flushes the ready prefix.
func (o *OrderedFlusher) Done(i int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if i < 0 || i >= len(o.done) || o.done[i] {
		return
	}
	o.done[i] = true
	for o.next < len(o.done) && o.done[o.next] {
		o.flush(o.next)
		o.next++
	}
}
