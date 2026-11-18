// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
)

// A connection attempt in flight when the server is removed used to save the
// copy of the record it started with (marked unreachable), bringing the
// removed server back; the daemon's poller makes such attempts every minute.
// Background writers now only update a record that still exists.
func TestRemovedServerIsNotBroughtBackByAConnectInFlight(t *testing.T) {
	app := newImportTestApp(t)
	dialing := make(chan struct{})
	release := make(chan struct{})
	app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		close(dialing)
		<-release
		return nil, errors.New("connection refused")
	}
	if err := app.AddServer(ServerRecord{Name: "web-01", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = app.CollectMetrics("web-01")
	}()
	select {
	case <-dialing:
	case <-time.After(30 * time.Second):
		t.Fatal("the metrics call never dialled")
	}
	if err := app.RemoveServerWithOptions("web-01", RemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the metrics call never returned")
	}
	if _, err := os.Stat(filepath.Join(app.ConfigDir, "servers", "web-01.toml")); !os.IsNotExist(err) {
		t.Fatalf("the removed server's record came back (stat err = %v)", err)
	}

	// Updating a removed server reports errServerGone without creating it.
	if err := app.updateExistingServer("web-01", func(*ServerRecord) bool { return true }); !errors.Is(err, errServerGone) {
		t.Fatalf("update of a removed server = %v, want errServerGone", err)
	}
	if _, err := os.Stat(filepath.Join(app.ConfigDir, "servers", "web-01.toml")); !os.IsNotExist(err) {
		t.Fatal("updateExistingServer created a removed server's record")
	}
}
