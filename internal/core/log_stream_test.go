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

	cached []string // aggregated cache contents, filled by stop()
}

func startFollow(t *testing.T, reader agent.LogReader, logPath, search string, tailLines int) *followHarness {
	t.Helper()
	h := newLogHarness(t, reader, logPath)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.done <- h.app.FollowServiceLogs(ctx, "loopback", "app.service", search, tailLines, 5*time.Millisecond, func(line proto.LogLine) error {
			h.mu.Lock()
			h.lines = append(h.lines, fmt.Sprintf("%d:%s", line.Number, line.Text))
			h.mu.Unlock()
			return nil
		})
	}()
	return h
}

// newLogHarness sets up an App whose tracked service "loopback/app.service"
// logs to logPath on an in-process agent (reader nil = the real reader).
func newLogHarness(t *testing.T, reader agent.LogReader, logPath string) *followHarness {
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
	cached, err := h.app.aggregatedLogs().Read("loopback", "app.service", "", 10_000_000)
	if err != nil {
		h.t.Fatalf("reading the aggregated cache: %v", err)
	}
	h.cached = h.cached[:0]
	for _, line := range cached.Lines {
		h.cached = append(h.cached, line.Text)
	}
	h.app.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.lines...)
}

// expectCachedAsPrinted checks that the controller's aggregated cache holds
// exactly the printed lines (texts), in order: nothing dropped or repeated.
func (h *followHarness) expectCachedAsPrinted(printed []string) {
	h.t.Helper()
	want := make([]string, len(printed))
	for i, p := range printed {
		_, want[i], _ = strings.Cut(p, ":")
	}
	if strings.Join(h.cached, "\n") != strings.Join(want, "\n") {
		h.t.Fatalf("aggregated cache holds %d lines, printed %d\n cache tail %v\nprinted tail %v",
			len(h.cached), len(want), h.cached[max(0, len(h.cached)-5):], want[max(0, len(want)-5):])
	}
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

	// copytruncate-style truncation, then (more than a tail window of) new
	// lines before the next poll can see them.
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "trunc-%d\n", i)
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	want = append(want, expectLines(1, 40, "trunc-", 1)...)
	h.waitFor(len(want))
	writeTestLog(t, logPath, 41, 44, "trunc-")
	want = append(want, expectLines(41, 44, "trunc-", 41)...)
	h.waitFor(len(want))

	// Rotation: the path atomically becomes a new file that already holds
	// more lines than the tail window.
	tmp := filepath.Join(dir, "app.log.new")
	writeTestLog(t, tmp, 1, 50, "rot-")
	if err := os.Link(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, logPath); err != nil {
		t.Fatal(err)
	}
	want = append(want, expectLines(1, 50, "rot-", 1)...)
	h.waitFor(len(want))
	writeTestLog(t, logPath, 51, 500, "rot-")
	want = append(want, expectLines(51, 500, "rot-", 51)...)
	h.waitFor(len(want))

	// Rotation by rename, briefly leaving no file at the path.
	if err := os.Rename(logPath, logPath+".2"); err != nil {
		t.Fatal(err)
	}
	writeTestLog(t, logPath, 1, 7, "gap-")
	want = append(want, expectLines(1, 7, "gap-", 1)...)
	h.waitFor(len(want))

	if got := h.stop(); !reflect.DeepEqual(got, want) {
		t.Fatalf("followed lines differ (%d vs %d)\n got tail %v\nwant tail %v", len(got), len(want), got[max(0, len(got)-5):], want[max(0, len(want)-5):])
	}
	// The rotation above replaced a file cached up to line 44 with one that
	// already had 50 lines: all 50 must be cached too (B10).
	h.expectCachedAsPrinted(want)
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
	h.expectCachedAsPrinted(want)
}

// scriptedLogReader answers log.read from a fixed script, then repeats the
// last step.
type scriptedLogReader struct {
	mu    sync.Mutex
	steps []func() (proto.LogReadResult, error)
	calls int
}

func (r *scriptedLogReader) Read(context.Context, proto.LogReadPayload) (proto.LogReadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	step := r.steps[min(r.calls, len(r.steps)-1)]
	r.calls++
	return step()
}

func logResult(cursorLine int, reset bool, lines ...string) func() (proto.LogReadResult, error) {
	return func() (proto.LogReadResult, error) {
		res := proto.LogReadResult{Path: "/x", Lines: []proto.LogLine{}, Reset: reset, Cursor: &proto.LogCursor{Line: cursorLine}}
		for _, l := range lines {
			n, text, _ := strings.Cut(l, ":")
			var num int
			fmt.Sscan(n, &num)
			res.Lines = append(res.Lines, proto.LogLine{Number: num, Text: text})
		}
		return res, nil
	}
}

func logMissing() (proto.LogReadResult, error) {
	return proto.LogReadResult{}, &agent.RPCError{Code: "log_open_failed", Message: "open /x: no such file or directory"}
}

// TestFollowServiceLogsRidesOutMissingFile: a file briefly missing mid-
// rotation is retried; one missing for too long ends the follow with the error.
func TestFollowServiceLogsRidesOutMissingFile(t *testing.T) {
	t.Parallel()
	reader := &scriptedLogReader{steps: []func() (proto.LogReadResult, error){
		logResult(2, false, "1:a", "2:b"),
		logMissing, logMissing, logMissing,
		logResult(1, true, "1:x"),
		logResult(1, false),
	}}
	h := startFollow(t, reader, "/x", "", 10)
	h.waitFor(3)
	if got := h.stop(); strings.Join(got, "|") != "1:a|2:b|1:x" {
		t.Fatalf("got %v", got)
	}
	h.expectCachedAsPrinted([]string{"1:a", "2:b", "1:x"})

	reader = &scriptedLogReader{steps: []func() (proto.LogReadResult, error){logResult(1, false, "1:a"), logMissing}}
	h = startFollow(t, reader, "/x", "", 10)
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "log_open_failed") {
			t.Fatalf("follow of a vanished log: err = %v", err)
		}
		h.done <- nil // let stop() see a clean exit
	case <-time.After(10 * time.Second):
		t.Fatal("follow of a vanished log did not give up")
	}
	if got := h.stop(); strings.Join(got, "|") != "1:a" {
		t.Fatalf("got %v", got)
	}
	if reader.calls != 2+maxLogFollowOpenRetries {
		t.Fatalf("reads = %d, want %d", reader.calls, 2+maxLogFollowOpenRetries)
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
