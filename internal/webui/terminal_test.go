// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package webui

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"golang.org/x/term"
)

// TestTermHelperProcess plays the `fleet ssh` child in the terminal tests: a
// tiny line-based shell running under the pty the web UI allocates.
func TestTermHelperProcess(t *testing.T) {
	if os.Getenv("FLEET_TERM_HELPER") != "1" {
		return
	}
	fmt.Printf("ready on %s (tty=%v)\r\n", os.Args[len(os.Args)-1], term.IsTerminal(int(os.Stdin.Fd())))
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch line {
		case "size":
			w, h, _ := term.GetSize(int(os.Stdin.Fd()))
			fmt.Printf("size=%dx%d\r\n", w, h)
		case "exit":
			os.Exit(7)
		default:
			fmt.Printf("got:%s\r\n", line)
		}
	}
	os.Exit(0)
}

func newTermServer(t *testing.T) (*Server, string, *[]string) {
	t.Helper()
	s, ts := newTestServer(t)
	for _, rec := range []core.ServerRecord{
		{Name: "web", Address: "127.0.0.1", Mode: transport.ModeDirect, Port: 2222},
		{Name: "edge", Address: "unknown", Mode: transport.ModeReverse, Port: 2222},
	} {
		if err := s.app.AddServer(rec); err != nil {
			t.Fatalf("AddServer: %v", err)
		}
	}
	var seen []string
	var seenMu sync.Mutex // opens can run concurrently (see the session-limit test)
	s.SetTerminal(func(ctx context.Context, server string) (*exec.Cmd, error) {
		seenMu.Lock()
		seen = append(seen, server)
		seenMu.Unlock()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTermHelperProcess$", "--", server)
		cmd.Env = append(os.Environ(), "FLEET_TERM_HELPER=1")
		return cmd, nil
	})
	t.Cleanup(func() { s.terminal.closeAll(5 * time.Second) })
	return s, ts.URL, &seen
}

func termPost(t *testing.T, s *Server, base, path string, q url.Values, body string) (int, string) {
	t.Helper()
	q.Set("t", s.Token())
	req, _ := http.NewRequest(http.MethodPost, base+path+"?"+q.Encode(), strings.NewReader(body))
	req.Header.Set("Origin", base)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		b.WriteString(sc.Text())
	}
	return res.StatusCode, b.String()
}

// termReader follows a session's SSE stream and collects decoded output.
type termReader struct {
	out    chan string
	exit   chan termSummary
	cancel context.CancelFunc
}

func followTerm(t *testing.T, s *Server, base, id string) *termReader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/term/stream?id="+id+"&t="+s.Token(), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	tr := &termReader{out: make(chan string, 100), exit: make(chan termSummary, 1), cancel: cancel}
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data := strings.TrimPrefix(line, "data: ")
				if event == "data" {
					raw, _ := base64.StdEncoding.DecodeString(data)
					tr.out <- string(raw)
				} else if event == "exit" {
					var sum termSummary
					_ = json.Unmarshal([]byte(data), &sum)
					tr.exit <- sum
				}
			}
		}
	}()
	t.Cleanup(cancel)
	return tr
}

func (tr *termReader) waitFor(t *testing.T, want string) string {
	t.Helper()
	var all strings.Builder
	deadline := time.After(15 * time.Second)
	for {
		select {
		case s := <-tr.out:
			all.WriteString(s)
			if strings.Contains(all.String(), want) {
				return all.String()
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %q", want, all.String())
		}
	}
}

func openTerm(t *testing.T, s *Server, base, server string) string {
	t.Helper()
	code, body := termPost(t, s, base, "/api/term/open", url.Values{"server": {server}, "cols": {"90"}, "rows": {"30"}}, "")
	if code != http.StatusOK {
		t.Fatalf("open %s: %d %s", server, code, body)
	}
	var out struct {
		Session termSummary `json:"session"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Session.ID == "" {
		t.Fatalf("open response %s: %v", body, err)
	}
	return out.Session.ID
}

func TestTerminalSessionRoundTrip(t *testing.T) {
	s, base, seen := newTermServer(t)
	id := openTerm(t, s, base, "web")
	tr := followTerm(t, s, base, id)
	tr.waitFor(t, "ready on web (tty=true)")

	if code, body := termPost(t, s, base, "/api/term/input", url.Values{"id": {id}}, "hello fleet\r"); code != http.StatusNoContent {
		t.Fatalf("input: %d %s", code, body)
	}
	tr.waitFor(t, "got:hello fleet")

	termPost(t, s, base, "/api/term/input", url.Values{"id": {id}}, "size\r")
	tr.waitFor(t, "size=90x30")
	if code, _ := termPost(t, s, base, "/api/term/resize", url.Values{"id": {id}, "cols": {"132"}, "rows": {"43"}}, ""); code != http.StatusNoContent {
		t.Fatalf("resize: %d", code)
	}
	termPost(t, s, base, "/api/term/input", url.Values{"id": {id}}, "size\r")
	tr.waitFor(t, "size=132x43")

	termPost(t, s, base, "/api/term/input", url.Values{"id": {id}}, "exit\r")
	select {
	case sum := <-tr.exit:
		if sum.State != "exited" || sum.ExitCode != 7 {
			t.Fatalf("exit summary %+v", sum)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no exit event")
	}
	if code, _ := termPost(t, s, base, "/api/term/input", url.Values{"id": {id}}, "x"); code != http.StatusConflict {
		t.Fatalf("input after exit: %d, want 409", code)
	}
	if len(*seen) != 1 || (*seen)[0] != "web" {
		t.Fatalf("launcher saw %v", *seen)
	}
}

func TestTerminalCloseEndsSession(t *testing.T) {
	s, base, _ := newTermServer(t)
	id := openTerm(t, s, base, "web")
	tr := followTerm(t, s, base, id)
	tr.waitFor(t, "ready on web")
	if code, body := termPost(t, s, base, "/api/term/close", url.Values{"id": {id}}, ""); code != http.StatusOK {
		t.Fatalf("close: %d %s", code, body)
	}
	select {
	case sum := <-tr.exit:
		if sum.State != "exited" {
			t.Fatalf("summary %+v", sum)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("session did not end")
	}
	// Closing an ended session forgets it.
	termPost(t, s, base, "/api/term/close", url.Values{"id": {id}}, "")
	if _, ok := s.terminal.get(id); ok {
		t.Fatal("ended session still listed after a second close")
	}
}

func TestTerminalRefusesBadServers(t *testing.T) {
	s, base, seen := newTermServer(t)
	cases := map[string]int{
		"":              http.StatusBadRequest,
		"nope":          http.StatusNotFound,
		"edge":          http.StatusBadRequest, // reverse mode
		"--config-dir":  http.StatusNotFound,
		"web; rm -rf /": http.StatusNotFound,
	}
	for server, want := range cases {
		if code, body := termPost(t, s, base, "/api/term/open", url.Values{"server": {server}}, ""); code != want {
			t.Fatalf("server %q: %d %s, want %d", server, code, body, want)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("launcher reached for a refused server: %v", *seen)
	}
	// Cross-origin and token-less requests never reach a session.
	req, _ := http.NewRequest(http.MethodPost, base+"/api/term/open?server=web&t="+s.Token(), nil)
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin open: %d", res.StatusCode)
	}
	res, err = http.Get(base + "/api/term")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.StatusCode)
	}
}

// listTerms returns the hub's sessions by id, as the browser sees them.
func listTerms(t *testing.T, s *Server, base string) map[string]termSummary {
	t.Helper()
	res, err := http.Get(base + "/api/term?t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Sessions []termSummary `json:"sessions"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	m := map[string]termSummary{}
	for _, sum := range out.Sessions {
		m[sum.ID] = sum
	}
	return m
}

// Opens that race each other must not together exceed maxTermSessions: the
// slot is taken under the same lock that counts the running sessions.
func TestTerminalSessionLimitHoldsUnderConcurrency(t *testing.T) {
	s, base, _ := newTermServer(t)
	const tries = maxTermSessions * 2
	codes := make(chan int, tries)
	start := make(chan struct{})
	openURL := base + "/api/term/open?" + url.Values{"server": {"web"}, "t": {s.Token()}}.Encode()
	for i := 0; i < tries; i++ {
		go func() {
			<-start
			req, _ := http.NewRequest(http.MethodPost, openURL, nil)
			req.Header.Set("Origin", base)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- -1
				return
			}
			res.Body.Close()
			codes <- res.StatusCode
		}()
	}
	close(start)
	opened, refused := 0, 0
	for i := 0; i < tries; i++ {
		switch code := <-codes; code {
		case http.StatusOK:
			opened++
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if opened != maxTermSessions || refused != tries-maxTermSessions {
		t.Fatalf("opened %d, refused %d; want %d and %d", opened, refused, maxTermSessions, tries-maxTermSessions)
	}
	s.terminal.mu.Lock()
	reserved := s.terminal.reserved
	s.terminal.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("%d slots still reserved after every open returned", reserved)
	}
}

// A session nobody watches (a closed tab) is ended after the idle timeout; a
// watched one is left alone.
func TestTerminalIdleSessionIsReaped(t *testing.T) {
	s, base, _ := newTermServer(t)
	// Long enough for the watched session's stream to attach first, even on a
	// slow machine under the race detector.
	s.terminal.idleTimeout = time.Second
	s.terminal.reapEvery = 25 * time.Millisecond

	idle := openTerm(t, s, base, "web")
	watched := openTerm(t, s, base, "web")
	tr := followTerm(t, s, base, watched)
	tr.waitFor(t, "ready on web")

	deadline := time.Now().Add(10 * time.Second)
	for {
		sum := listTerms(t, s, base)[idle]
		if sum.State == "exited" {
			if !strings.Contains(sum.Error, "no browser attached") {
				t.Fatalf("reaped session error %q", sum.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle session still %q after the timeout", sum.State)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := listTerms(t, s, base)[watched].State; got != "running" {
		t.Fatalf("a watched session was reaped: %q", got)
	}
}

// One session takes at most maxTermStreams output streams at once.
func TestTerminalStreamLimit(t *testing.T) {
	s, base, _ := newTermServer(t)
	id := openTerm(t, s, base, "web")
	for i := 0; i < maxTermStreams; i++ {
		followTerm(t, s, base, id)
	}
	res, err := http.Get(base + "/api/term/stream?id=" + id + "&t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream %d: status %d, want 429", maxTermStreams+1, res.StatusCode)
	}
}
