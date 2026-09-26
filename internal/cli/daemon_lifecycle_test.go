// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"encoding/json"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
)

func freeCLIAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func setRuntimeAddresses(t *testing.T, dir, listen, control string) {
	t.Helper()
	cfg, err := core.LoadConfig(core.ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runtime.ListenAddress = listen
	cfg.Runtime.ControlAddress = control
	if err := core.SaveConfig(core.ConfigPath(dir), cfg); err != nil {
		t.Fatal(err)
	}
}

// TestCLIDaemonHelper is not a real test: TestStartStatusStop launches this
// test binary with FLEET_TEST_CLI_DAEMON_DIR set and it runs the real
// `fleet daemon` command for that config dir.
func TestCLIDaemonHelper(t *testing.T) {
	dir := os.Getenv("FLEET_TEST_CLI_DAEMON_DIR")
	if dir == "" {
		t.Skip("helper process for fleet start tests")
	}
	root := NewRootCommand()
	root.SetArgs([]string{"--config-dir", dir, "daemon"})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStartStatusStopRunTheRealDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses SIGTERM semantics")
	}
	t.Setenv("FLEET_TOKEN", "")
	dir := newServerCommandTestConfig(t)
	control := freeCLIAddress(t)
	setRuntimeAddresses(t, dir, freeCLIAddress(t), control)
	t.Setenv("FLEET_TEST_CLI_DAEMON_DIR", dir)
	old := daemonStartOptions
	daemonStartOptions = func(app *core.App) core.DaemonStartOptions {
		opts := old(app)
		opts.Executable = os.Args[0]
		opts.Args = []string{"-test.run=^TestCLIDaemonHelper$", "--", "daemon"}
		return opts
	}
	t.Cleanup(func() {
		daemonStartOptions = old
		_, _ = core.StopDaemon(dir, control, 5*time.Second)
	})

	r := runExecFleet(t, dir, "stop")
	if r.err != nil || strings.TrimSpace(r.stdout) != "fleet daemon is not running" {
		t.Fatalf("stop with nothing running: err=%v out=%q", r.err, r.combined)
	}

	r = runExecFleet(t, dir, "start")
	if r.err != nil || !strings.HasPrefix(r.stdout, "fleet daemon started (pid ") || !strings.Contains(r.stdout, "logs: "+core.DaemonLogPath(dir)) {
		t.Fatalf("start: err=%v out=%q", r.err, r.combined)
	}
	pid, err := core.ReadDaemonPID(dir)
	if err != nil {
		t.Fatal(err)
	}
	logData, _ := os.ReadFile(core.DaemonLogPath(dir))
	if !strings.Contains(string(logData), "local control listening on "+control) {
		t.Fatalf("daemon log lacks the listening banner:\n%s", logData)
	}

	r = runExecFleet(t, dir, "start")
	if r.err != nil || !strings.Contains(r.stdout, "fleet daemon is running (pid") {
		t.Fatalf("second start: err=%v out=%q", r.err, r.combined)
	}

	r = runExecFleet(t, dir, "status")
	var status core.Status
	if err := json.Unmarshal([]byte(r.stdout), &status); err != nil || r.err != nil {
		t.Fatalf("status: %v %v\n%s", err, r.err, r.combined)
	}
	if status.Daemon == nil || !status.Daemon.Running || status.Daemon.PID != pid {
		t.Fatalf("status daemon block = %+v; want running as pid %d", status.Daemon, pid)
	}

	r = runExecFleet(t, dir, "stop")
	if r.err != nil || !strings.Contains(r.stdout, "fleet daemon stopped (pid") {
		t.Fatalf("stop: err=%v out=%q", r.err, r.combined)
	}
	if _, err := os.Stat(core.DaemonPIDPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("pid file left after stop: %v", err)
	}
	r = runExecFleet(t, dir, "status")
	if !strings.Contains(r.stdout, `"running": false`) {
		t.Fatalf("status after stop:\n%s", r.stdout)
	}
}

// TestDaemonDoesNotAnnounceListenersItCannotBind is the regression for a
// second daemon printing "listening on ..." before its bind failed.
func TestDaemonDoesNotAnnounceListenersItCannotBind(t *testing.T) {
	t.Setenv("FLEET_TOKEN", "")
	dir := newServerCommandTestConfig(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	setRuntimeAddresses(t, dir, busy.Addr().String(), freeCLIAddress(t))

	r := runExecFleet(t, dir, "daemon")
	if r.err == nil || !strings.Contains(r.err.Error(), "listen for reverse agents") {
		t.Fatalf("daemon on a busy port: err=%v", r.err)
	}
	if strings.Contains(r.combined, "listening") {
		t.Fatalf("daemon announced listeners it could not bind:\n%s", r.combined)
	}
	if _, err := os.Stat(core.DaemonPIDPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("failed daemon left its pid file: %v", err)
	}
}

func TestDaemonRefusesSecondInstance(t *testing.T) {
	t.Setenv("FLEET_TOKEN", "")
	dir := newServerCommandTestConfig(t)
	setRuntimeAddresses(t, dir, freeCLIAddress(t), freeCLIAddress(t))
	holder, err := core.ClaimDaemon(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()

	r := runExecFleet(t, dir, "daemon")
	if r.err == nil || !strings.Contains(r.err.Error(), "a fleet daemon is already running for "+dir) {
		t.Fatalf("second daemon: err=%v", r.err)
	}
	if strings.Contains(r.combined, "listening") {
		t.Fatalf("refused daemon printed a listening banner:\n%s", r.combined)
	}
	if pid, err := core.ReadDaemonPID(dir); err != nil || pid != os.Getpid() {
		t.Fatalf("refused daemon disturbed the holder's pid file: %d, %v", pid, err)
	}
}
