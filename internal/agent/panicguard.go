// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"fmt"
	"os"
	"runtime/debug"
)

// guardPanic contains a panic to the single goroutine that raised it.
//
// Every per-connection and per-channel handler runs in its own goroutine and
// parses input from the network. In Go an unrecovered panic in ANY goroutine
// terminates the whole process, so without this a single latent nil-deref or
// index-out-of-range reachable from a crafted envelope would take the agent
// down for every connected controller — turning a localized bug into an
// availability incident across the fleet.
//
// The decode paths are individually bounds-checked, so this is containment for
// bugs not yet found rather than a fix for a known crash. Deferred callers
// registered *before* this one (semaphore releases, channel closes) still run,
// because deferred calls unwind LIFO after the recover.
func guardPanic(what string) {
	if r := recover(); r != nil {
		fmt.Fprintf(os.Stderr, "fleet-agent: recovered panic in %s: %v\n%s\n", what, r, debug.Stack())
	}
}
