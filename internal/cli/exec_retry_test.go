// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
)

func addExecRetryServer(t *testing.T, configDir string, port int, pin bool) {
	t.Helper()
	app, err := core.Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.AddServer(core.ServerRecord{Name: "web", Address: "127.0.0.1", Port: port, Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatal(err)
	}
	if pin {
		if err := app.ReconnectServer("web", false); err != nil { // pins the host key
			t.Fatalf("ReconnectServer: %v", err)
		}
	}
}

// `exec --retry` retries a failure that certainly never reached the server
// (here the SSH handshake fails every time), so each retry dials again.
func TestExecRetryRetriesConnectionFailures(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepts atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = c.Close() // not an SSH server: the handshake fails
		}
	}()
	configDir := newServerCommandTestConfig(t)
	addExecRetryServer(t, configDir, ln.Addr().(*net.TCPAddr).Port, false)

	if _, err := runFleetIn(t, configDir, "exec", "web", "true", "--retry", "0"); err == nil {
		t.Fatal("exec against a non-SSH port succeeded")
	}
	once := accepts.Load()
	if once == 0 {
		t.Fatal("exec never connected")
	}
	if _, err := runFleetIn(t, configDir, "exec", "web", "true", "--retry", "2", "--backoff", "10ms"); err == nil {
		t.Fatal("exec against a non-SSH port succeeded")
	}
	if got := accepts.Load() - once; got != 3*once {
		t.Fatalf("--retry 2 connected %d times, want %d (the first try and two retries)", got, 3*once)
	}
}

// trackingListener remembers the connections an agent accepted so the test
// can cut them.
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *trackingListener) cut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		_ = c.Close()
	}
}

// A connection that drops while the command runs may mean it ran: `exec
// --retry` must not run it again, and neither may the connection pool.
func TestExecRetryNeverRerunsACommandThatMayHaveRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test command uses a POSIX shell")
	}
	configDir := newServerCommandTestConfig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tl := &trackingListener{Listener: ln}
	srv := agent.Server{
		Mode: transport.ModeDirect, HostKeyPath: filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, tl) }()
	t.Cleanup(func() { stop(); _ = ln.Close(); <-done })
	addExecRetryServer(t, configDir, ln.Addr().(*net.TCPAddr).Port, true)

	runs := filepath.Join(t.TempDir(), "runs")
	command := "echo run >> '" + runs + "'; sleep 10"
	result := make(chan error, 1)
	go func() {
		_, err := runFleetIn(t, configDir, "exec", "web", command, "--retry", "3", "--backoff", "10ms")
		result <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if data, _ := os.ReadFile(runs); strings.Count(string(data), "run") >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	tl.cut() // the connection drops while the command runs
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("exec reported success after its connection dropped")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("exec did not return after its connection dropped")
	}
	time.Sleep(500 * time.Millisecond) // a re-run would have started by now
	data, _ := os.ReadFile(runs)
	if n := strings.Count(string(data), "run"); n != 1 {
		t.Fatalf("the command ran %d times, want exactly once", n)
	}
}
