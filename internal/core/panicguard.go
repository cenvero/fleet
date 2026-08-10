// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"runtime/debug"
)

// guardPanic contains a panic to the single goroutine that raised it.
//
// The controller daemon serves every agent in the fleet from one process, so an
// unrecovered panic in a per-connection goroutine does not just drop that
// agent's session — it terminates the daemon and takes the whole fleet's
// control plane with it. Guarding the handlers that parse agent-supplied input
// converts that worst case into one dropped connection plus a stack trace.
//
// This is defense in depth: the envelope decode paths are size-bounded and use
// checked type assertions, so no reachable panic is known today.
func guardPanic(what string) {
	if r := recover(); r != nil {
		fmt.Fprintf(os.Stderr, "fleet: recovered panic in %s: %v\n%s\n", what, r, debug.Stack())
	}
}
