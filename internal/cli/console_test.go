// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/webui"
)

// TestMain lets the test binary stand in for the fleet binary: a background
// console is a child `fleet console serve`, and core.StartConsole runs this
// executable for it.
func TestMain(m *testing.M) {
	if os.Getenv("FLEET_TEST_CLI_MAIN") == "1" {
		root := NewRootCommand()
		root.SetArgs(os.Args[1:])
		if err := root.Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func consoleTestGet(t *testing.T, u string) (int, string) {
	t.Helper()
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

var launchTokenRE = regexp.MustCompile(`name="fleet-launch-token" content="([0-9a-f]{64})"`)

// A console started in the background is reachable only through one-time
// launch links that `fleet console open` fetches with the launch key; its
// session token never appears on a command line or in its files; `fleet
// console stop` ends it and cleans up.
func TestBackgroundConsoleLifecycle(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	t.Setenv("FLEET_TEST_CLI_MAIN", "1")
	t.Cleanup(func() { _, _ = core.StopConsole(configDir, 0) })

	out, err := runFleetIn(t, configDir, "console", "--background", "--open=no", "--addr", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("console --background: %v\n%s", err, out)
	}
	st := core.ReadConsole(configDir)
	if !st.Running || st.Starting || st.Address == "" {
		t.Fatalf("after --background: %+v", st)
	}
	// Background mode never keeps Control or Terminal on unless asked.
	if st.Control || st.Terminal {
		t.Fatalf("background console has Control/Terminal on by default: %+v", st)
	}
	idle, lifetime := core.ConsoleLimits(false, false)
	if st.IdleLimit != idle.String() || st.StopsBy.Sub(st.StartedAt) != lifetime {
		t.Fatalf("limits: %+v", st)
	}

	// A second start does not start another one.
	out, err = runFleetIn(t, configDir, "console", "--background", "--open=no")
	if err != nil || !strings.Contains(out, "already runs in the background") {
		t.Fatalf("second console --background: %v\n%s", err, out)
	}

	out, err = runFleetIn(t, configDir, "console", "open", "--print", "--files")
	if err != nil {
		t.Fatalf("console open --print: %v\n%s", err, out)
	}
	link := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if !strings.HasPrefix(link, st.Address+"?c=") || !strings.HasSuffix(link, "#files") {
		t.Fatalf("one-time link %q", link)
	}
	code, page := consoleTestGet(t, strings.TrimSuffix(link, "#files"))
	m := launchTokenRE.FindStringSubmatch(page)
	if code != http.StatusOK || m == nil {
		t.Fatalf("opening the link: %d, token in page: %v", code, m != nil)
	}
	token := m[1]
	if code, _ := consoleTestGet(t, st.Address+"api/servers?t="+token); code != http.StatusOK {
		t.Fatalf("the token from the link: %d", code)
	}
	// Nothing on disk carries the session token.
	entries, _ := os.ReadDir(filepath.Join(configDir, "data"))
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(configDir, "data", e.Name()))
		if strings.Contains(string(data), token) {
			t.Fatalf("%s carries the session token", e.Name())
		}
	}
	for _, name := range []string{"console.json", "console.key"} {
		info, err := os.Stat(filepath.Join(configDir, "data", name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 && runtimeIsUnix() {
			t.Fatalf("%s is readable by others: %v", name, info.Mode())
		}
	}

	// Opened a second time, the link has been used by two parties: it opens
	// nothing and the token it handed out stops working.
	if _, again := consoleTestGet(t, strings.TrimSuffix(link, "#files")); launchTokenRE.MatchString(again) {
		t.Fatal("the one-time link worked twice")
	}
	if code, _ := consoleTestGet(t, st.Address+"api/servers?t="+token); code != http.StatusUnauthorized {
		t.Fatalf("the token from a reused link still works: %d", code)
	}

	out, err = runFleetIn(t, configDir, "console", "stop")
	if err != nil {
		t.Fatalf("console stop: %v\n%s", err, out)
	}
	var res core.ConsoleStopResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.WasRunning {
		t.Fatalf("console stop = %s (%v)", out, err)
	}
	if st := core.ReadConsole(configDir); st.Running {
		t.Fatalf("still running after stop: %+v", st)
	}
	for _, name := range []string{"console.json", "console.key"} {
		if _, err := os.Stat(filepath.Join(configDir, "data", name)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind after stop (%v)", name, err)
		}
	}
	if _, err := runFleetIn(t, configDir, "console", "open"); err == nil {
		t.Fatal("console open succeeded with nothing running")
	}
}

// Moving a console to the background hands its session token to the new
// process on stdin, so tabs the foreground console opened keep working.
func TestBackgroundConsoleKeepsAHandedOverToken(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	t.Setenv("FLEET_TEST_CLI_MAIN", "1")
	t.Cleanup(func() { _, _ = core.StopConsole(configDir, 0) })
	token := strings.Repeat("5a", 32)
	st, err := core.StartConsole(core.ConsoleStartOptions{ConfigDir: configDir, Args: consoleServeArgs("127.0.0.1:0", false, false), HandoverToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := consoleTestGet(t, st.Address+"api/servers?t="+token); code != http.StatusOK {
		t.Fatalf("the handed-over token: %d", code)
	}
	if code, _ := consoleTestGet(t, st.Address+"api/servers?t="+strings.Repeat("00", 32)); code != http.StatusUnauthorized {
		t.Fatalf("another token: %d, want 401", code)
	}
}

// Under an RBAC token a background console is refused: it would have to keep
// the token, or act with more authority than it.
func TestConsoleBackgroundRefusedUnderAToken(t *testing.T) {
	configDir := newServerCommandTestConfig(t)
	tok, err := core.NewTokenStore(configDir).Create(core.Token{Name: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_TOKEN", tok.ID)
	t.Setenv("FLEET_TEST_CLI_MAIN", "1")
	// The token gate records the verified operator process-wide; do not let
	// it leak into the tests that run after this one.
	prevOperator := actingOperator
	t.Cleanup(func() { actingOperator = prevOperator })
	for _, args := range [][]string{{"console", "--background", "--open=no"}, {"console", "open"}, {"console", "serve"}} {
		out, err := runFleetIn(t, configDir, args...)
		if err == nil || !strings.Contains(err.Error(), "--token") {
			t.Fatalf("%v under a token: %v\n%s", args, err, out)
		}
	}
	if st := core.ReadConsole(configDir); st.Running {
		t.Fatal("a background console started under a token")
	}
}

// The limits watchdog stops a console at its absolute limit and when it has
// gone unused.
func TestConsoleLimitsStopIt(t *testing.T) {
	old := consoleWatchInterval
	consoleWatchInterval = 10 * time.Millisecond
	t.Cleanup(func() { consoleWatchInterval = old })
	srv, err := webui.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		idle    time.Duration
		stopsBy time.Time
	}{
		"time limit": {idle: time.Hour, stopsBy: time.Now().Add(-time.Second)},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { watchConsoleLimits(ctx, cancel, srv, tc.idle, tc.stopsBy, io.Discard); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the console was not stopped", name)
		}
		if ctx.Err() == nil {
			t.Fatalf("%s: watchdog returned without stopping", name)
		}
		cancel()
	}

	// Unused: serving, with nobody making requests.
	l, err := webui.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(serveCtx, l, nil) }()
	done := make(chan struct{})
	go func() {
		watchConsoleLimits(serveCtx, stopServe, srv, 50*time.Millisecond, time.Now().Add(time.Hour), io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an unused console was not stopped")
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func runtimeIsUnix() bool { return os.PathSeparator == '/' }

// Moving a console to the background hands over the listening socket itself,
// so the port is never free for another account to bind and collect the
// token from reconnecting tabs. The parent keeps holding the port here the
// whole time: the child can only be serving it through the inherited socket.
func TestBackgroundConsoleInheritsTheListeningSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot hand a listening socket to a child; there the token is replaced instead")
	}
	configDir := newServerCommandTestConfig(t)
	t.Setenv("FLEET_TEST_CLI_MAIN", "1")
	t.Cleanup(func() { _, _ = core.StopConsole(configDir, 0) })
	l, err := webui.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	sock, err := listenerFile(l)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("6b", 32)
	st, err := core.StartConsole(core.ConsoleStartOptions{
		ConfigDir: configDir, Args: append(consoleServeArgs(l.Addr().String(), false, false), "--listen-fd", "3"),
		HandoverToken: token, ExtraFiles: []*os.File{sock},
	})
	_ = sock.Close()
	if err != nil {
		t.Fatal(err)
	}
	if st.Address != webui.BaseURL(l) {
		t.Fatalf("the background console serves %s, want the handed-over %s", st.Address, webui.BaseURL(l))
	}
	_ = l.Close() // the foreground one stops; the socket stays open in the child
	if code, _ := consoleTestGet(t, st.Address+"api/servers?t="+token); code != http.StatusOK {
		t.Fatalf("after the handover: %d", code)
	}
}
