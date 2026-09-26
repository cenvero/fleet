// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

// TestDaemonHelperProcess is not a real test: StartDaemon tests launch this
// test binary with FLEET_TEST_DAEMON_HELPER set, and it then behaves like a
// minimal `fleet daemon` (claim, listen on the control address, exit on
// SIGTERM) — or, in "fail" mode, like one that cannot start.
func TestDaemonHelperProcess(t *testing.T) {
	mode := os.Getenv("FLEET_TEST_DAEMON_HELPER")
	if mode == "" {
		t.Skip("helper process for StartDaemon tests")
	}
	fmt.Printf("helper token=%q\n", os.Getenv("FLEET_TOKEN"))
	if mode == "fail" {
		fmt.Println("boom: cannot bind the reverse listener")
		os.Exit(3)
	}
	instance, err := ClaimDaemon(os.Getenv("FLEET_TEST_DAEMON_DIR"))
	if err != nil {
		fmt.Println(err)
		os.Exit(4)
	}
	listener, err := net.Listen("tcp", os.Getenv("FLEET_TEST_DAEMON_CONTROL"))
	if err != nil {
		fmt.Println(err)
		instance.Release()
		os.Exit(5)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	<-ctx.Done()
	stop()
	_ = listener.Close()
	instance.Release()
	os.Exit(0)
}

func helperDaemonOptions(t *testing.T, configDir, mode string) DaemonStartOptions {
	t.Helper()
	control := freeLoopbackAddress(t)
	t.Setenv("FLEET_TEST_DAEMON_HELPER", mode)
	t.Setenv("FLEET_TEST_DAEMON_DIR", configDir)
	t.Setenv("FLEET_TEST_DAEMON_CONTROL", control)
	return DaemonStartOptions{
		ConfigDir:      configDir,
		ControlAddress: control,
		Executable:     os.Args[0],
		// "daemon" keeps the Linux /proc/<pid>/cmdline check satisfied.
		Args: []string{"-test.run=^TestDaemonHelperProcess$", "--", "daemon"},
	}
}

func TestClaimDaemonIsExclusiveAndReleases(t *testing.T) {
	old := daemonClaimWait
	daemonClaimWait = 100 * time.Millisecond
	t.Cleanup(func() { daemonClaimWait = old })
	dir := t.TempDir()

	first, err := ClaimDaemon(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pid, err := ReadDaemonPID(dir); err != nil || pid != os.Getpid() {
		t.Fatalf("pid file = %d, %v; want %d", pid, err, os.Getpid())
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(DaemonPIDPath(dir)); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("pid file mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
	_, err = ClaimDaemon(dir)
	if !errors.Is(err, ErrDaemonRunning) || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("second claim = %v; want ErrDaemonRunning naming pid %d", err, os.Getpid())
	}
	if state := DaemonStatus(dir, ""); !state.Running || state.PID != os.Getpid() {
		t.Fatalf("status while claimed = %+v", state)
	}
	first.Release()
	if _, err := os.Stat(DaemonPIDPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("pid file left behind after Release: %v", err)
	}
	second, err := ClaimDaemon(dir)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	second.Release()
}

// TestStopDaemonNeverSignalsStalePID: a pid file naming a live process that
// does not hold the daemon lock (here: this test process) is stale, so stop
// removes it and signals nothing.
func TestStopDaemonNeverSignalsStalePID(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonPIDFile(dir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	state := DaemonStatus(dir, "")
	if state.Running || state.PID != 0 || !state.StalePIDFile {
		t.Fatalf("status with a stale pid file = %+v", state)
	}
	res, err := StopDaemon(dir, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.WasRunning || !res.RemovedStalePIDFile {
		t.Fatalf("stop result = %+v; want not running with the stale pid file removed", res)
	}
	if _, err := os.Stat(DaemonPIDPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("stale pid file kept: %v", err)
	}
	// Nothing to stop is not an error either.
	if res, err := StopDaemon(dir, "", time.Second); err != nil || res.WasRunning {
		t.Fatalf("second stop = %+v, %v", res, err)
	}
}

func TestStopDaemonRefusesUnmanagedControlListener(t *testing.T) {
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().String()
	if state := DaemonStatus(dir, address); !state.Running || state.PID != 0 || !state.ControlReachable {
		t.Fatalf("status = %+v; want running via the control address", state)
	}
	if _, err := StopDaemon(dir, address, time.Second); err == nil || !strings.Contains(err.Error(), "did not record a pid file") {
		t.Fatalf("stop of an unmanaged daemon = %v", err)
	}
}

func TestStartAndStopDaemonProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses SIGTERM semantics")
	}
	dir := t.TempDir()
	opts := helperDaemonOptions(t, dir, "run")
	// The operator's token must not reach the daemon (argv or environment).
	t.Setenv("FLEET_TOKEN", "operator-secret-token")

	res, err := StartDaemon(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = StopDaemon(dir, opts.ControlAddress, 5*time.Second) })
	if res.AlreadyRunning || res.PID <= 0 || res.LogPath != DaemonLogPath(dir) {
		t.Fatalf("start result = %+v", res)
	}
	if pid, err := ReadDaemonPID(dir); err != nil || pid != res.PID {
		t.Fatalf("pid file = %d, %v; want %d", pid, err, res.PID)
	}
	logData, err := os.ReadFile(res.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "operator-secret-token") || !strings.Contains(string(logData), `helper token=""`) {
		t.Fatalf("daemon saw the operator token:\n%s", logData)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(res.LogPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("log mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
	if slices.Contains(DefaultDaemonArgs(dir), "operator-secret-token") {
		t.Fatal("default daemon argv carries the token")
	}
	if state := DaemonStatus(dir, opts.ControlAddress); !state.Running || state.PID != res.PID || !state.ControlReachable {
		t.Fatalf("status after start = %+v", state)
	}

	again, err := StartDaemon(opts)
	if err != nil || !again.AlreadyRunning || again.PID != res.PID {
		t.Fatalf("second start = %+v, %v; want already running as pid %d", again, err, res.PID)
	}

	stopped, err := StopDaemon(dir, opts.ControlAddress, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped.WasRunning || stopped.PID != res.PID {
		t.Fatalf("stop result = %+v", stopped)
	}
	if state := DaemonStatus(dir, opts.ControlAddress); state.Running {
		t.Fatalf("status after stop = %+v", state)
	}
	if _, err := os.Stat(DaemonPIDPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("pid file left after stop: %v", err)
	}
}

func TestStartDaemonReportsEarlyExitWithLogTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper process relies on POSIX exit semantics")
	}
	dir := t.TempDir()
	opts := helperDaemonOptions(t, dir, "fail")
	_, err := StartDaemon(opts)
	if err == nil {
		t.Fatal("start of a failing daemon succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "failed to start") || !strings.Contains(msg, "boom: cannot bind the reverse listener") {
		t.Fatalf("error does not carry the daemon's log tail:\n%s", msg)
	}
}

// TestRunDaemonAnnouncesReadyAfterBinding: the ready hook runs only once the
// listeners accept connections, and never for a daemon that cannot bind.
func TestRunDaemonAnnouncesReadyAfterBinding(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatal(err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	app.Config.Runtime.ListenAddress = busy.Addr().String()
	app.Config.Runtime.ControlAddress = freeLoopbackAddress(t)
	called := false
	err = app.RunDaemon(WithDaemonReady(context.Background(), func() { called = true }))
	if err == nil || called {
		t.Fatalf("daemon on a busy port: err=%v ready called=%v", err, called)
	}

	app.Config.Runtime.ListenAddress = freeLoopbackAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- app.RunDaemon(WithDaemonReady(ctx, func() {
			conn, err := net.DialTimeout("tcp", app.Config.Runtime.ControlAddress, time.Second)
			if err == nil {
				_ = conn.Close()
			}
			ready <- err
		}))
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("ready announced before the control listener accepted connections: %v", err)
		}
	case err := <-done:
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("ready hook never ran")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
