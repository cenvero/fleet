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

// guardPanicThen is guardPanic for a goroutine that owes someone an answer.
// After containing a panic it calls answer, to reply to the controller that is
// waiting or to report the failure to the caller, so the panic does not turn
// into silence. A second panic raised by answer is contained too.
//
// Like guardPanic it must be deferred directly: recover only stops a panic when
// called by the deferred function itself.
func guardPanicThen(what string, answer func()) {
	if r := recover(); r != nil {
		fmt.Fprintf(os.Stderr, "fleet-agent: recovered panic in %s: %v\n%s\n", what, r, debug.Stack())
		defer guardPanic(what + " (answering after a panic)")
		answer()
	}
}
