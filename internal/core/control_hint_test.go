// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package core

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// With the daemon stopped, reverse-mode calls say so and how to start it,
// rather than only "no such file" or "connection refused".
func TestControlErrorsSayTheDaemonIsNotRunning(t *testing.T) {
	_, err := readControlTokenFile(filepath.Join(t.TempDir(), "control.token"))
	if err == nil || !strings.Contains(err.Error(), "fleet daemon is not running") || !strings.Contains(err.Error(), "fleet start") {
		t.Fatalf("missing token file: err = %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close() // nothing listens there now
	app := &App{Config: Config{Runtime: RuntimeConfig{ControlAddress: address}}}
	_, _, err = app.controlDial(context.Background(), "call")
	if err == nil || !strings.Contains(err.Error(), "fleet start") {
		t.Fatalf("refused control connection: err = %v", err)
	}
}
