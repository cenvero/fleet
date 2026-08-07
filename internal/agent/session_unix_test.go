// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestReplayBufferSnapshotAndClear proves the scrollback is replayed at most
// once: snapshotAndClear hands back the accumulated bytes and empties the
// buffer, so a later (possibly unrelated) channel attaching to the same
// fingerprint-keyed session does not auto-receive the prior session's
// scrollback. Output produced after the clear is still captured for the next
// reconnect, preserving resume-after-drop.
func TestReplayBufferSnapshotAndClear(t *testing.T) {
	t.Parallel()
	r := &replayBuffer{}
	r.write([]byte("secret command output"))

	// First attach drains and clears.
	if got := string(r.snapshotAndClear()); got != "secret command output" {
		t.Fatalf("first replay = %q, want the buffered scrollback", got)
	}
	// A second attach with no intervening output sees nothing — the prior
	// scrollback is NOT re-served to a new connection.
	if got := r.snapshotAndClear(); len(got) != 0 {
		t.Fatalf("second replay must be empty (no cross-connection leak), got %q", got)
	}
	// Output produced after the clear is still available for the next reconnect.
	r.write([]byte("new output"))
	if got := string(r.snapshotAndClear()); got != "new output" {
		t.Fatalf("post-clear replay = %q, want only the new output", got)
	}
}

// TestReplayBufferCapAfterClear confirms the ring still enforces its size cap
// independently of the clear, so the buffer can't grow without bound.
func TestReplayBufferCapAfterClear(t *testing.T) {
	t.Parallel()
	r := &replayBuffer{}
	r.write(make([]byte, replayBufCap+4096))
	if got := r.snapshot(); len(got) != replayBufCap {
		t.Fatalf("buffer should be capped at %d, got %d", replayBufCap, len(got))
	}
}

type attachmentTestChannel struct{}

func (*attachmentTestChannel) Read([]byte) (int, error)                       { return 0, io.EOF }
func (*attachmentTestChannel) Write(p []byte) (int, error)                    { return len(p), nil }
func (*attachmentTestChannel) Close() error                                   { return nil }
func (*attachmentTestChannel) CloseWrite() error                              { return nil }
func (*attachmentTestChannel) SendRequest(string, bool, []byte) (bool, error) { return true, nil }
func (*attachmentTestChannel) Stderr() io.ReadWriter                          { return &bytes.Buffer{} }

func TestPersistentSessionRejectsSecondActiveAttachment(t *testing.T) {
	first := &attachmentTestChannel{}
	second := &attachmentTestChannel{}
	s := &persistentSession{activeConn: first}
	if _, err := s.attach(second, 80, 24, &sessionStore{}, "resume-id"); !errors.Is(err, errSessionAlreadyAttached) {
		t.Fatalf("second active attachment error = %v, want %v", err, errSessionAlreadyAttached)
	}
	if s.activeConn != first {
		t.Fatal("second attachment must not replace the active channel")
	}
}
