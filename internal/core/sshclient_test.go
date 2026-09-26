// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func sshTestApp(t *testing.T) *App {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatal(err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

// B9: `fleet ssh` to a reverse-mode server dialed the placeholder address in
// its record straight from the controller, would pin and talk to whatever
// listened there, and printed "Connection lost. Reconnecting" for a
// connection that never existed. It must refuse before dialing anything.
func TestSSHRefusesReverseServerWithoutDialing(t *testing.T) {
	app := sshTestApp(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	// Point the placeholder at a listener so any dial would be seen.
	if err := app.AddServer(ServerRecord{Name: "rev", Address: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Mode: transport.ModeReverse, User: "cenvero-agent"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = app.RunSSHSession("rev", &out)
	if !errors.Is(err, ErrSSHReverseUnsupported) {
		t.Fatalf("RunSSHSession() error = %v, want %v", err, ErrSSHReverseUnsupported)
	}
	if want := "interactive ssh to reverse-mode servers is not supported; use fleet exec"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	time.Sleep(50 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("placeholder address was dialed %d times", n)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

// A first connection that fails has nothing to reconnect to: report it at
// once, without "Connection lost" / "Reconnecting" noise or retry delays.
func TestSSHFirstConnectFailureDoesNotRetry(t *testing.T) {
	app := sshTestApp(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // nothing listens there now
	if err := app.AddServer(ServerRecord{Name: "web", Address: "127.0.0.1", Port: port,
		Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	start := time.Now()
	err = app.RunSSHSession("web", &out)
	if err == nil {
		t.Fatal("RunSSHSession() succeeded against a closed port")
	}
	if elapsed := time.Since(start); elapsed >= sshReconnectDelay {
		t.Fatalf("first-connect failure took %s; it must not wait to retry", elapsed)
	}
	if s := out.String(); strings.Contains(s, "Reconnect") || strings.Contains(s, "Connection lost") {
		t.Fatalf("printed reconnect messages although it never connected: %q", s)
	}
	if !strings.Contains(err.Error(), "connect to web") {
		t.Fatalf("error = %v, want it to name the failed connection", err)
	}
}

// With piped stdin, `exit 3` on the remote shell must make `fleet ssh` exit
// 3: the agent reports the shell's real status and the client returns it.
func TestSSHPropagatesRemoteExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent's interactive shell is unix-only")
	}
	app := sshTestApp(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := agent.Server{
		Mode: transport.ModeDirect, HostKeyPath: filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(app.ConfigDir, "keys", "id_ed25519.pub"),
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() { stop(); <-done })
	if err := app.AddServer(ServerRecord{Name: "web", Address: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatal(err)
	}
	if err := app.ReconnectServer("web", true); err != nil { // pin the host key
		t.Fatalf("ReconnectServer: %v", err)
	}

	for _, tc := range []struct {
		script string
		want   int
	}{{"exit 3\n", 3}, {"exit 0\n", 0}} {
		err := runSSHWithPipedStdin(t, app, "web", tc.script)
		var exitErr *RemoteExitError
		switch {
		case tc.want == 0 && err != nil:
			t.Fatalf("%q: RunSSHSession() error = %v, want nil", tc.script, err)
		case tc.want != 0 && (!errors.As(err, &exitErr) || exitErr.Code != tc.want):
			t.Fatalf("%q: RunSSHSession() error = %v, want remote exit status %d", tc.script, err, tc.want)
		}
	}
}

// runSSHWithPipedStdin runs RunSSHSession with stdin a pipe carrying script
// and the shell's output discarded, as `echo ... | fleet ssh` would.
func runSSHWithPipedStdin(t *testing.T, app *App, server, script string) error {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	sink, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinR, sink
	defer func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		_ = stdinR.Close()
		_ = sink.Close()
	}()
	if _, err := stdinW.WriteString(script); err != nil {
		t.Fatal(err)
	}
	_ = stdinW.Close()

	result := make(chan error, 1)
	go func() { result <- app.RunSSHSession(server, &bytes.Buffer{}) }()
	select {
	case err := <-result:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("remote shell did not exit")
		return nil
	}
}
