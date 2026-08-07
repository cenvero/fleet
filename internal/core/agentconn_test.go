// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"testing"
	"time"
)

// drainAgentServeErrs waits for the in-test agent's ServeConn goroutines to
// unwind and fails if any returned an error.
//
// Tests used to receive a fixed number of times from errCh, one per RPC, because
// every control call opened and closed its own SSH connection. Connections are
// now pooled and reused, so the number of connections is an implementation
// detail — and reducing it is the point. What still matters is that every
// connection the agent did serve exited cleanly, so this drains whatever was
// produced instead of asserting a count.
//
// Call App.DisconnectPooledSessions first: ServeConn only returns once the
// controller closes the connection.
func drainAgentServeErrs(t *testing.T, errCh <-chan error) {
	t.Helper()
	// At least one connection is always established, so wait properly for the
	// first rather than racing the goroutine's teardown.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("agent server exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent server did not exit after pooled connections were released")
	}
	// Any further connections (redials after a failure, extra channels) unwind
	// immediately behind the first.
	for {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("agent server exited with error: %v", err)
			}
		case <-time.After(250 * time.Millisecond):
			return
		}
	}
}
