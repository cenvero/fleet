// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestConsoleHelperProcess is not a real test: the console tests launch the
// test binary itself as the "fleet" child, and this function plays its part.
func TestConsoleHelperProcess(t *testing.T) {
	if os.Getenv("FLEET_CONSOLE_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	// args = [command path..., args...]
	switch args[0] {
	case "echo":
		fmt.Println(strings.Join(args[1:], " "))
		fmt.Fprintln(os.Stderr, "to-stderr")
	case "fail":
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	case "stdin":
		data, _ := io.ReadAll(os.Stdin)
		fmt.Printf("got:%s", data)
	case "utf8":
		// Write a multi-byte rune split across two writes.
		os.Stdout.Write([]byte{0xE2, 0x9C})
		os.Stdout.Sync()
		time.Sleep(50 * time.Millisecond)
		os.Stdout.Write([]byte{0x93, '\n'}) // ✓
	case "sleep":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		fmt.Println("waiting")
		select {
		case <-ch:
			fmt.Println("interrupted")
			os.Exit(130)
		case <-time.After(30 * time.Second):
		}
	}
	os.Exit(0)
}

func helperLauncher(t *testing.T, seen *[][]string) CommandLauncher {
	t.Helper()
	return func(ctx context.Context, args []string) (*exec.Cmd, error) {
		if seen != nil {
			*seen = append(*seen, append([]string(nil), args...))
		}
		if args[0] == "refuse" {
			return nil, fmt.Errorf("refused by launcher")
		}
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestConsoleHelperProcess$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "FLEET_CONSOLE_HELPER=1")
		return cmd, nil
	}
}

var testCatalog = []CommandSpec{
	{Path: []string{"echo"}, Use: "echo [words...]"},
	{Path: []string{"fail"}, Use: "fail"},
	{Path: []string{"stdin"}, Use: "stdin", Stdin: true},
	{Path: []string{"utf8"}, Use: "utf8"},
	{Path: []string{"sleep"}, Use: "sleep"},
	{Path: []string{"refuse"}, Use: "refuse"},
	{Path: []string{"dashboard"}, Use: "dashboard", Unavailable: "interactive terminal UI"},
	{Path: []string{"secret", "set"}, Use: "set <name>", Flags: []CommandFlag{{Name: "value", Shorthand: "v", Type: "string"}}},
}

func newConsoleServer(t *testing.T) (*Server, string, *[][]string) {
	t.Helper()
	s, ts := newTestServer(t)
	var seen [][]string
	s.SetConsole(testCatalog, helperLauncher(t, &seen))
	t.Cleanup(func() { s.console.runs.cancelAll(10 * time.Second) })
	return s, ts.URL, &seen
}

func postRun(t *testing.T, s *Server, base string, req runRequest) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(req)
	hr, _ := http.NewRequest(http.MethodPost, base+"/api/runs?t="+s.Token(), bytes.NewReader(body))
	hr.Header.Set("Origin", base)
	hr.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// streamRun reads the SSE stream to completion and returns the output per
// stream plus the final status.
func streamRun(t *testing.T, s *Server, base, id string) (stdout, stderr string, final runSummary) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/runs/stream?id="+id+"&t="+s.Token(), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var out, errb strings.Builder
	sc := bufio.NewScanner(res.Body)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if event == "chunk" {
				var c runChunk
				if err := json.Unmarshal([]byte(data), &c); err != nil {
					t.Fatal(err)
				}
				if c.Stream == "err" {
					errb.WriteString(c.Text)
				} else {
					out.WriteString(c.Text)
				}
			} else if event == "status" {
				if err := json.Unmarshal([]byte(data), &final); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return out.String(), errb.String(), final
}

func runID(t *testing.T, out map[string]any) string {
	t.Helper()
	run, _ := out["run"].(map[string]any)
	id, _ := run["id"].(string)
	if id == "" {
		t.Fatalf("no run id in %v", out)
	}
	return id
}

func TestConsoleRunStreamsOutputAndExitCode(t *testing.T) {
	s, base, seen := newConsoleServer(t)
	status, out := postRun(t, s, base, runRequest{Path: []string{"echo"}, Args: []string{"hello", "world"}})
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	stdout, stderr, final := streamRun(t, s, base, runID(t, out))
	if stdout != "hello world\n" || stderr != "to-stderr\n" {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
	}
	if final.State != "ok" || final.ExitCode != 0 {
		t.Fatalf("final %+v", final)
	}
	if want := [][]string{{"echo", "hello", "world"}}; !reflect.DeepEqual(*seen, want) {
		t.Fatalf("launcher saw %v, want %v", *seen, want)
	}

	status, out = postRun(t, s, base, runRequest{Path: []string{"fail"}})
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	_, stderr, final = streamRun(t, s, base, runID(t, out))
	if final.State != "failed" || final.ExitCode != 3 || stderr != "boom\n" {
		t.Fatalf("final %+v stderr %q", final, stderr)
	}
}

func TestConsoleStdinAndUTF8(t *testing.T) {
	s, base, _ := newConsoleServer(t)
	_, out := postRun(t, s, base, runRequest{Path: []string{"stdin"}, Stdin: "s3cret"})
	stdout, _, _ := streamRun(t, s, base, runID(t, out))
	if stdout != "got:s3cret" {
		t.Fatalf("stdout %q", stdout)
	}
	_, out = postRun(t, s, base, runRequest{Path: []string{"utf8"}})
	stdout, _, _ = streamRun(t, s, base, runID(t, out))
	if stdout != "✓\n" {
		t.Fatalf("split rune mangled: %q", stdout)
	}
	// Without stdin text the child reads EOF immediately (never blocks).
	_, out = postRun(t, s, base, runRequest{Path: []string{"stdin"}})
	stdout, _, _ = streamRun(t, s, base, runID(t, out))
	if stdout != "got:" {
		t.Fatalf("stdout %q", stdout)
	}
}

func TestConsoleCancelInterruptsRun(t *testing.T) {
	s, base, _ := newConsoleServer(t)
	_, out := postRun(t, s, base, runRequest{Path: []string{"sleep"}})
	id := runID(t, out)
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, _ := s.console.runs.get(id)
		s.console.runs.mu.Lock()
		started := run.bytes > 0
		s.console.runs.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, body := postAPI(t, s, base, "/api/runs/cancel", url.Values{"id": {id}}, base)
	if code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, body)
	}
	stdout, _, final := streamRun(t, s, base, id)
	if final.State != "cancelled" {
		t.Fatalf("final %+v", final)
	}
	if !strings.Contains(stdout, "interrupted") && final.ExitCode != -1 {
		t.Fatalf("expected a graceful interrupt, stdout %q final %+v", stdout, final)
	}
	if code, _ := postAPI(t, s, base, "/api/runs/cancel", url.Values{"id": {id}}, base); code != http.StatusConflict {
		t.Fatalf("second cancel: %d, want 409", code)
	}
}

func TestConsoleRefusesBadRequests(t *testing.T) {
	s, base, seen := newConsoleServer(t)
	cases := []struct {
		name string
		req  runRequest
		want int
	}{
		{"unknown command", runRequest{Path: []string{"nope"}}, http.StatusBadRequest},
		{"unavailable command", runRequest{Path: []string{"dashboard"}}, http.StatusBadRequest},
		{"token override", runRequest{Path: []string{"echo"}, Args: []string{"--token", "x"}}, http.StatusBadRequest},
		{"token override =", runRequest{Path: []string{"echo"}, Args: []string{"--token=x"}}, http.StatusBadRequest},
		{"config-dir override", runRequest{Path: []string{"echo"}, Args: []string{"--config-dir=/tmp"}}, http.StatusBadRequest},
		{"NUL byte", runRequest{Path: []string{"echo"}, Args: []string{"a\x00b"}}, http.StatusBadRequest},
		{"launcher refusal", runRequest{Path: []string{"refuse"}}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if status, out := postRun(t, s, base, c.req); status != c.want {
			t.Fatalf("%s: status %d (%v), want %d", c.name, status, out, c.want)
		}
	}
	for _, args := range *seen {
		if args[0] != "refuse" {
			t.Fatalf("launcher reached for a refused request: %v", args)
		}
	}
	// After "--" the reserved names are plain positional text.
	if status, out := postRun(t, s, base, runRequest{Path: []string{"echo"}, Args: []string{"--", "--token"}}); status != http.StatusOK {
		t.Fatalf("positional --token refused: %d %v", status, out)
	} else {
		streamRun(t, s, base, runID(t, out))
	}
	// Unknown JSON fields are rejected, so a client can't think it set an env.
	hr, _ := http.NewRequest(http.MethodPost, base+"/api/runs?t="+s.Token(), strings.NewReader(`{"path":["echo"],"env":{"A":"b"}}`))
	hr.Header.Set("Origin", base)
	res, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", res.StatusCode)
	}
}

func TestConsoleRunPostRequiresSameOrigin(t *testing.T) {
	s, base, seen := newConsoleServer(t)
	body := `{"path":["echo"],"args":["x"]}`
	for _, origin := range []string{"", "https://evil.example", "null"} {
		hr, _ := http.NewRequest(http.MethodPost, base+"/api/runs?t="+s.Token(), strings.NewReader(body))
		if origin != "" {
			hr.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: status %d, want 403", origin, res.StatusCode)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("a cross-origin POST reached the launcher: %v", *seen)
	}
	// Without the token nothing is listed or run.
	res, err := http.Get(base + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.StatusCode)
	}
}

func TestConsoleHistoryMasksSensitiveFlags(t *testing.T) {
	spec := testCatalog[len(testCatalog)-1]
	cases := map[string][]string{
		"--value hunter2": {"db", "--value", "••••"},
		"--value=hunter2": {"db", "--value=••••"},
		"-v hunter2":      {"db", "-v", "••••"},
		"-vhunter2":       {"db", "-v••••"},
	}
	for name, want := range cases {
		args := append([]string{"db"}, strings.Fields(name)...)
		if got := displayArgs(spec, args); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
	}
	// Flags marked sensitive by the catalog are masked too.
	custom := CommandSpec{Flags: []CommandFlag{{Name: "api-key", Type: "string", Sensitive: true}}}
	if got := displayArgs(custom, []string{"--api-key", "k"}); got[1] != "••••" {
		t.Fatalf("custom sensitive flag not masked: %v", got)
	}
}

func TestConsoleCatalogAndHistoryEndpoints(t *testing.T) {
	s, base, _ := newConsoleServer(t)
	res, err := http.Get(base + "/api/commands?t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Commands []CommandSpec `json:"commands"`
	}
	_ = json.NewDecoder(res.Body).Decode(&cat)
	res.Body.Close()
	if len(cat.Commands) != len(testCatalog) {
		t.Fatalf("catalog has %d commands, want %d", len(cat.Commands), len(testCatalog))
	}
	for _, c := range cat.Commands {
		if c.Args == nil || c.Flags == nil {
			t.Fatalf("catalog entry %v has null args/flags", c.Path)
		}
	}

	_, out := postRun(t, s, base, runRequest{Path: []string{"secret", "set"}, Args: []string{"db", "--value", "hunter2"}})
	id := runID(t, out)
	streamRun(t, s, base, id)
	res, err = http.Get(base + "/api/runs?t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("history leaked a sensitive value: %s", raw)
	}
	res, err = http.Get(base + "/api/runs?id=" + id + "&t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	var one struct {
		Run    runSummary `json:"run"`
		Output []runChunk `json:"output"`
	}
	_ = json.NewDecoder(res.Body).Decode(&one)
	res.Body.Close()
	if one.Run.ID != id || one.Run.State != "ok" {
		t.Fatalf("run lookup: %+v", one.Run)
	}
	if code, body := postAPI(t, s, base, "/api/runs/clear", url.Values{}, base); code != http.StatusOK || !strings.Contains(body, `"removed":1`) {
		t.Fatalf("clear: %d %s", code, body)
	}
}

func TestConsoleOutputIsBounded(t *testing.T) {
	h := newRunHub()
	r := &commandRun{changed: make(chan struct{})}
	big := strings.Repeat("x", 1<<20)
	for i := 0; i < 6; i++ {
		h.appendOutput(r, "out", big)
	}
	if r.bytes > maxRunOutputBytes || !r.truncated {
		t.Fatalf("bytes %d truncated %v", r.bytes, r.truncated)
	}
	if r.chunks[len(r.chunks)-1].Seq != 6 {
		t.Fatalf("sequence numbers must keep counting after truncation")
	}
}

func TestConsoleDisabledByDefault(t *testing.T) {
	s, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/commands?t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 without a console", res.StatusCode)
	}
}
