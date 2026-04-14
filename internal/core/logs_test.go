// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestReadServiceLogs(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		LogReader: fakeLogReader{
			Results: map[string]proto.LogReadResult{
				"/var/log/nginx/access.log": {
					Path: "/var/log/nginx/access.log",
					Lines: []proto.LogLine{
						{Number: 18, Text: "127.0.0.1 GET /health"},
						{Number: 19, Text: "127.0.0.1 GET /metrics"},
					},
				},
			},
		},
	}
	errCh := make(chan error, 1)

	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40001", "127.0.0.1:2222")
		go func() {
			errCh <- server.ServeConn(serverConn)
		}()
		return clientConn, nil
	}

	if err := app.AddServer(ServerRecord{
		Name:    "loopback",
		Address: "127.0.0.1",
		Port:    2222,
		Mode:    transport.ModeDirect,
		User:    "cenvero-agent",
		Services: []ServiceRecord{
			{Name: "nginx.service", LogPath: "/var/log/nginx/access.log"},
		},
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	result, err := app.ReadServiceLogs("loopback", "nginx.service", "metrics", 50, false)
	if err != nil {
		t.Fatalf("ReadServiceLogs() error = %v", err)
	}
	if result.Path != "/var/log/nginx/access.log" {
		t.Fatalf("unexpected log path %q", result.Path)
	}
	if len(result.Lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(result.Lines))
	}

	if err := <-errCh; err != nil {
		t.Fatalf("agent server exited with error: %v", err)
	}
}

type fakeLogReader struct {
	mu       sync.Mutex
	Results  map[string]proto.LogReadResult
	Sequence map[string][]proto.LogReadResult
}

func (f fakeLogReader) Read(_ context.Context, payload proto.LogReadPayload) (proto.LogReadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sequence := f.Sequence[payload.Path]; len(sequence) > 0 {
		result := sequence[0]
		f.Sequence[payload.Path] = sequence[1:]
		return result, nil
	}
	result, ok := f.Results[payload.Path]
	if !ok {
		return proto.LogReadResult{}, &agent.RPCError{
			Code:    "missing_log_path",
			Message: "log path was not registered in the test reader",
		}
	}
	return result, nil
}

func TestFollowServiceLogsEmitsOnlyNewLines(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	reader := fakeLogReader{
		Sequence: map[string][]proto.LogReadResult{
			"/var/log/nginx/access.log": {
				{
					Path: "/var/log/nginx/access.log",
					Lines: []proto.LogLine{
						{Number: 10, Text: "first"},
						{Number: 11, Text: "second"},
					},
				},
				{
					Path: "/var/log/nginx/access.log",
					Lines: []proto.LogLine{
						{Number: 10, Text: "first"},
						{Number: 11, Text: "second"},
						{Number: 12, Text: "third"},
					},
				},
			},
		},
	}

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		LogReader:          reader,
	}
	errCh := make(chan error, 1)

	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40009", "127.0.0.1:2222")
		go func() {
			errCh <- server.ServeConn(serverConn)
		}()
		return clientConn, nil
	}

	if err := app.AddServer(ServerRecord{
		Name:    "loopback",
		Address: "127.0.0.1",
		Port:    2222,
		Mode:    transport.ModeDirect,
		User:    "cenvero-agent",
		Services: []ServiceRecord{
			{Name: "nginx.service", LogPath: "/var/log/nginx/access.log"},
		},
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var lines []string
	done := make(chan error, 1)
	go func() {
		done <- app.FollowServiceLogs(ctx, "loopback", "nginx.service", "", 50, 10*time.Millisecond, func(line proto.LogLine) error {
			lines = append(lines, line.Text)
			if len(lines) == 3 {
				cancel()
			}
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FollowServiceLogs() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("FollowServiceLogs() did not finish in time")
	}

	if got := len(lines); got != 3 {
		t.Fatalf("expected 3 emitted lines, got %d (%v)", got, lines)
	}
	if lines[0] != "first" || lines[1] != "second" || lines[2] != "third" {
		t.Fatalf("unexpected followed lines: %v", lines)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("agent server exited with error: %v", err)
	}
}

func TestReadCachedServiceLogsUsesAggregatedCopy(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	reader := fakeLogReader{
		Sequence: map[string][]proto.LogReadResult{
			"/var/log/nginx/access.log": {
				{
					Path: "/var/log/nginx/access.log",
					Lines: []proto.LogLine{
						{Number: 1, Text: "first"},
						{Number: 2, Text: "second"},
					},
				},
				{
					Path: "/var/log/nginx/access.log",
					Lines: []proto.LogLine{
						{Number: 1, Text: "first"},
						{Number: 2, Text: "second"},
						{Number: 3, Text: "third"},
					},
				},
			},
		},
	}

	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		LogReader:          reader,
	}
	errCh := make(chan error, 1)

	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40011", "127.0.0.1:2222")
		go func() {
			errCh <- server.ServeConn(serverConn)
		}()
		return clientConn, nil
	}

	if err := app.AddServer(ServerRecord{
		Name:    "loopback",
		Address: "127.0.0.1",
		Port:    2222,
		Mode:    transport.ModeDirect,
		User:    "cenvero-agent",
		Services: []ServiceRecord{
			{Name: "nginx.service", LogPath: "/var/log/nginx/access.log"},
		},
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	if _, err := app.ReadServiceLogs("loopback", "nginx.service", "", 50, false); err != nil {
		t.Fatalf("ReadServiceLogs(first) error = %v", err)
	}
	if _, err := app.ReadServiceLogs("loopback", "nginx.service", "", 50, false); err != nil {
		t.Fatalf("ReadServiceLogs(second) error = %v", err)
	}

	cached, err := app.ReadCachedServiceLogs("loopback", "nginx.service", "", 50)
	if err != nil {
		t.Fatalf("ReadCachedServiceLogs() error = %v", err)
	}
	if len(cached.Lines) != 3 {
		t.Fatalf("expected 3 cached log lines, got %d", len(cached.Lines))
	}
	if cached.Lines[2].Text != "third" {
		t.Fatalf("unexpected cached lines: %#v", cached.Lines)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("agent server exited with error: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("agent server exited with error: %v", err)
	}
}
