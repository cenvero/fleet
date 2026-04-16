// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

const (
	// replayBufCap is the amount of recent PTY output kept in memory so the
	// user sees context when they reconnect after a network drop.
	replayBufCap = 64 * 1024 // 64 KB

	// sessionIdleTimeout is how long a disconnected session is kept alive.
	// After this with no reconnect, the shell receives SIGHUP and is cleaned up.
	sessionIdleTimeout = 10 * time.Minute
)

// globalStore is the single in-process session store.
// Each fleet-agent process has exactly one store; sessions are keyed by a
// session ID (currently always "default" — one session per agent).
var globalStore = &sessionStore{sessions: make(map[string]*persistentSession)}

// ────────────────────────────────────────────────────────────────────────────
// replayBuffer — a size-capped byte ring used as PTY scrollback.
// ────────────────────────────────────────────────────────────────────────────

type replayBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (r *replayBuffer) write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
	if len(r.data) > replayBufCap {
		r.data = r.data[len(r.data)-replayBufCap:]
	}
}

func (r *replayBuffer) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.data))
	copy(out, r.data)
	return out
}

// ────────────────────────────────────────────────────────────────────────────
// persistentSession — one live shell kept alive across network drops.
// ────────────────────────────────────────────────────────────────────────────

type persistentSession struct {
	ptm    *os.File
	cmd    *exec.Cmd
	replay *replayBuffer
	done   chan struct{} // closed when the shell process exits

	mu          sync.Mutex
	activeConn  ssh.Channel // nil when no client is connected
	idleTimer   *time.Timer
}

// attach wires a new SSH channel to this session.
// It cancels any pending idle timer, sends the replay buffer so the user
// sees recent output, then starts proxying live I/O.
func (s *persistentSession) attach(channel ssh.Channel, cols, rows uint32, store *sessionStore, id string) <-chan struct{} {
	s.mu.Lock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.activeConn = channel
	snapshot := s.replay.snapshot()
	s.mu.Unlock()

	// Resize PTY to the new terminal dimensions.
	_ = pty.Setsize(s.ptm, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})

	// Send scrollback so the user can see what happened while disconnected.
	if len(snapshot) > 0 {
		_, _ = channel.Write(snapshot)
	}

	// Proxy stdin: channel → PTY master.
	// When this returns (channel closed / network drop), detach.
	// Returns a channel that is closed once the input goroutine exits —
	// the caller uses this to know when the connection dropped without
	// creating a second competing reader on the same channel.
	detached := make(chan struct{})
	go func() {
		defer close(detached)
		_, _ = io.Copy(s.ptm, channel)
		s.detach(channel, store, id)
	}()
	return detached
}

// detach is called when the channel closes (user disconnected / network drop).
// The shell keeps running; an idle timer is started.
func (s *persistentSession) detach(channel ssh.Channel, store *sessionStore, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeConn != channel {
		return // already replaced by a newer connection
	}
	s.activeConn = nil

	// Start idle countdown. If nobody reconnects within the timeout,
	// send SIGHUP to the shell and remove the session.
	s.idleTimer = time.AfterFunc(sessionIdleTimeout, func() {
		store.kill(id)
	})
}

// outputLoop runs for the lifetime of the session. It reads PTY output and:
//   - writes to the active channel when one is connected, AND
//   - always writes to the replay buffer (for scrollback on reconnect).
//
// When the shell process exits, done is closed and the session is removed
// from the store automatically.
func (s *persistentSession) outputLoop(store *sessionStore, id string) {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptm.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			s.replay.write(data)
			s.mu.Lock()
			conn := s.activeConn
			s.mu.Unlock()
			if conn != nil {
				_, _ = conn.Write(data)
			}
		}
		if err != nil {
			break
		}
	}
	close(s.done)
	store.remove(id)
}

// ────────────────────────────────────────────────────────────────────────────
// sessionStore — the in-process registry of live sessions.
// ────────────────────────────────────────────────────────────────────────────

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*persistentSession
}

// getOrCreate returns an existing live session or creates a new one.
// newFn is called (without the lock held) only when no session exists.
func (st *sessionStore) getOrCreate(id string, newFn func() (*persistentSession, error)) (*persistentSession, bool, error) {
	st.mu.Lock()
	if s, ok := st.sessions[id]; ok {
		st.mu.Unlock()
		return s, false, nil // false = existing session
	}
	st.mu.Unlock()

	s, err := newFn()
	if err != nil {
		return nil, false, err
	}

	st.mu.Lock()
	// Double-check: another goroutine may have created it while we were in newFn.
	if existing, ok := st.sessions[id]; ok {
		st.mu.Unlock()
		// Discard the one we just made — kill its shell.
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.ptm.Close()
		return existing, false, nil
	}
	st.sessions[id] = s
	st.mu.Unlock()

	go s.outputLoop(st, id)
	return s, true, nil // true = newly created
}

// remove deletes the session entry. Called when the shell exits.
func (st *sessionStore) remove(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, id)
}

// kill sends SIGHUP to the shell and removes the session (idle timeout path).
func (st *sessionStore) kill(id string) {
	st.mu.Lock()
	s, ok := st.sessions[id]
	if ok {
		delete(st.sessions, id)
	}
	st.mu.Unlock()
	if !ok {
		return
	}
	s.mu.Lock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.mu.Unlock()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGHUP)
	}
	_ = s.ptm.Close()
}
