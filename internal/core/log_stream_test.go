// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

// legacyAgentLogReader behaves like an agent from before log cursors: it
// ignores the request cursor, scans the whole file and returns the last lines
// with no cursor in the result.
type legacyAgentLogReader struct{}

func (legacyAgentLogReader) Read(_ context.Context, payload proto.LogReadPayload) (proto.LogReadResult, error) {
	data, err := os.ReadFile(payload.Path)
	if err != nil {
		return proto.LogReadResult{}, &agent.RPCError{Code: "log_open_failed", Message: err.Error()}
	}
	tail := payload.TailLines
	if tail <= 0 {
		tail = 200
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var lines []proto.LogLine
	for n := 1; scanner.Scan(); n++ {
		if payload.Search == "" || strings.Contains(strings.ToLower(scanner.Text()), strings.ToLower(payload.Search)) {
			lines = append(lines, proto.LogLine{Number: n, Text: scanner.Text()})
		}
	}
	result := proto.LogReadResult{Path: payload.Path, Lines: lines}
	if len(lines) > tail {
		result.Lines, result.Truncated = lines[len(lines)-tail:], true
	}
	return result, nil
}

// followHarness runs FollowServiceLogs against an in-process agent serving
// logPath and collects what it prints.
type followHarness struct {
	t      *testing.T
	app    *App
	errCh  chan error
	cancel context.CancelFunc
	done   chan error

	mu    sync.Mutex
	lines []string
}

func startFollow(t *testing.T, reader agent.LogReader, logPath, search string, tailLines int) *followHarness {
	t.Helper()
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
	h := &followHarness{t: t, app: app, errCh: make(chan error, 8), done: make(chan error, 1)}
	server := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
		LogReader:          reader,
	}
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40021", "127.0.0.1:2222")
		go func() { h.errCh <- server.ServeConn(serverConn) }()
		return clientConn, nil
	}
	if err := app.AddServer(ServerRecord{
		Name: "loopback", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent",
		Services: []ServiceRecord{{Name: "app.service", LogPath: logPath}},
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.done <- app.FollowServiceLogs(ctx, "loopback", "app.service", search, tailLines, 5*time.Millisecond, func(line proto.LogLine) error {
			h.mu.Lock()
			h.lines = append(h.lines, fmt.Sprintf("%d:%s", line.Number, line.Text))
			h.mu.Unlock()
			return nil
		})
	}()
	return h
}

// waitFor waits until n lines have been printed.
func (h *followHarness) waitFor(n int) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		got := len(h.lines)
		h.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.t.Fatalf("timed out waiting for %d lines, have %d: %v", n, len(h.lines), h.lines)
}

// stop ends the follow and returns everything printed.
func (h *followHarness) stop() []string {
	h.t.Helper()
	time.Sleep(30 * time.Millisecond) // a few more polls: nothing further may appear
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Fatalf("FollowServiceLogs() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("FollowServiceLogs() did not stop")
	}
	h.app.DisconnectPooledSessions()
	drainAgentServeErrs(h.t, h.errCh)
	h.app.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.lines...)
}

func writeTestLog(t *testing.T, path string, from, to int, prefix string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%s%d\n", prefix, i)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func expectLines(numFrom, numTo int, prefix string, textFrom int) []string {
	var out []string
	for n := numFrom; n <= numTo; n++ {
		out = append(out, fmt.Sprintf("%d:%s%d", n, prefix, textFrom+n-numFrom))
	}
	return out
}

// TestFollowServiceLogsWithCursorLosesNothing drives a real agent log reader:
// bursts larger than the tail window, truncation and rotation must neither
// drop nor repeat a line.
func TestFollowServiceLogsWithCursorLosesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	writeTestLog(t, logPath, 1, 5, "line-")

	h := startFollow(t, nil, logPath, "", 3)
	want := expectLines(3, 5, "line-", 3)
	h.waitFor(len(want))

	// A burst far bigger than the 3-line tail window.
	writeTestLog(t, logPath, 6, 2005, "line-")
	want = append(want, expectLines(6, 2005, "line-", 6)...)
	h.waitFor(len(want))

	// copytruncate-style truncation, then new lines.
	if err := os.Truncate(logPath, 0); err != nil {
		t.Fatal(err)
	}
	writeTestLog(t, logPath, 1, 2, "trunc-")
	want = append(want, expectLines(1, 2, "trunc-", 1)...)
	h.waitFor(len(want))
	writeTestLog(t, logPath, 3, 4, "trunc-")
	want = append(want, expectLines(3, 4, "trunc-", 3)...)
	h.waitFor(len(want))

	// Rotation: the path atomically becomes a new file.
	tmp := filepath.Join(dir, "app.log.new")
	writeTestLog(t, tmp, 1, 3, "rot-")
	if err := os.Link(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, logPath); err != nil {
		t.Fatal(err)
	}
	want = append(want, expectLines(1, 3, "rot-", 1)...)
	h.waitFor(len(want))
	writeTestLog(t, logPath, 4, 500, "rot-")
	want = append(want, expectLines(4, 500, "rot-", 4)...)
	h.waitFor(len(want))

	if got := h.stop(); !reflect.DeepEqual(got, want) {
		t.Fatalf("followed lines differ (%d vs %d)\n got tail %v\nwant tail %v", len(got), len(want), got[max(0, len(got)-5):], want[max(0, len(want)-5):])
	}
}

func TestFollowServiceLogsWithCursorAndSearch(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "app.log")
	writeTestLog(t, logPath, 1, 4, "ok-")
	h := startFollow(t, nil, logPath, "ERROR", 10)
	time.Sleep(20 * time.Millisecond)
	writeTestLog(t, logPath, 5, 6, "ok-")
	writeTestLog(t, logPath, 7, 8, "error-")
	writeTestLog(t, logPath, 9, 9, "ok-")
	want := []string{"7:error-7", "8:error-8"}
	h.waitFor(len(want))
	if got := h.stop(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestFollowServiceLogsLegacyAgent: an agent without cursor support is
// followed by re-reading its tail, exactly as before.
func TestFollowServiceLogsLegacyAgent(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "app.log")
	writeTestLog(t, logPath, 1, 5, "line-")
	h := startFollow(t, legacyAgentLogReader{}, logPath, "", 50)
	want := expectLines(1, 5, "line-", 1)
	h.waitFor(len(want))
	writeTestLog(t, logPath, 6, 30, "line-")
	want = append(want, expectLines(6, 30, "line-", 6)...)
	h.waitFor(len(want))
	// Truncation below the last printed number restarts numbering once.
	if err := os.WriteFile(logPath, []byte("new-1\nnew-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want = append(want, expectLines(1, 2, "new-", 1)...)
	h.waitFor(len(want))
	if got := h.stop(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

// TestLogFollowerTailResetDoesNotRepeat is the regression test for the legacy
// dedupe: after a truncation the old high-water mark was kept, so every later
// poll re-printed the whole new tail.
func TestLogFollowerTailResetDoesNotRepeat(t *testing.T) {
	var got []string
	f := logFollower{emit: func(l proto.LogLine) error { got = append(got, fmt.Sprintf("%d:%s", l.Number, l.Text)); return nil }}
	polls := [][]proto.LogLine{
		{{Number: 1, Text: "a"}, {Number: 2, Text: "b"}, {Number: 3, Text: "c"}},
		{{Number: 1, Text: "x"}, {Number: 2, Text: "y"}}, // truncated
		{{Number: 1, Text: "x"}, {Number: 2, Text: "y"}}, // nothing new
		{{Number: 1, Text: "x"}, {Number: 2, Text: "y"}, {Number: 3, Text: "z"}},
	}
	for _, lines := range polls {
		if err := f.applyTail(proto.LogReadResult{Lines: lines}); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"1:a", "2:b", "3:c", "1:x", "2:y", "3:z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLogFollowerHoldsBackUnterminatedLine(t *testing.T) {
	var got []string
	f := logFollower{emit: func(l proto.LogLine) error { got = append(got, fmt.Sprintf("%d:%s", l.Number, l.Text)); return nil }}
	apply := func(cursorLine int, lines ...proto.LogLine) {
		t.Helper()
		if _, err := f.applyCursor(proto.LogReadResult{Lines: lines, Cursor: &proto.LogCursor{Line: cursorLine}}); err != nil {
			t.Fatal(err)
		}
	}
	apply(2, proto.LogLine{Number: 1, Text: "a"}, proto.LogLine{Number: 2, Text: "b"}, proto.LogLine{Number: 3, Text: "par"})
	apply(3, proto.LogLine{Number: 3, Text: "partial"}) // completed before the next poll
	apply(3, proto.LogLine{Number: 4, Text: "stuck"})   // unterminated, then unchanged for a poll
	apply(3, proto.LogLine{Number: 4, Text: "stuck"})
	apply(5, proto.LogLine{Number: 4, Text: "stuck!"}, proto.LogLine{Number: 5, Text: "e"}) // completion of a printed line is not repeated
	if want := []string{"1:a", "2:b", "3:partial", "4:stuck", "5:e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
