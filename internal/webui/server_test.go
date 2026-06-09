// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := core.Initialize(core.InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	app, err := core.Open(configDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	s, err := New(app)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestWebUIRequiresToken(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)

	res, err := http.Get(ts.URL + "/api/servers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", res.StatusCode)
	}

	res2, err := http.Get(ts.URL + "/api/servers?t=" + s.Token())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res2.Body)
		t.Fatalf("expected 200 with token, got %d: %s", res2.StatusCode, body)
	}
}

func TestWebUIRejectsBadToken(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/servers?t=deadbeef")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with bad token, got %d", res.StatusCode)
	}
}

func TestWebUIServesIndex(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for index, got %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) == 0 {
		t.Fatalf("index body empty")
	}
}

func TestEnsureLoopbackAddr(t *testing.T) {
	t.Parallel()
	ok := []string{"127.0.0.1:9445", "localhost:8080", "[::1]:9445"}
	for _, a := range ok {
		if err := ensureLoopbackAddr(a); err != nil {
			t.Fatalf("expected %q to be allowed: %v", a, err)
		}
	}
	bad := []string{"0.0.0.0:9445", "192.168.1.5:9445", "example.com:80"}
	for _, a := range bad {
		if err := ensureLoopbackAddr(a); err == nil {
			t.Fatalf("expected %q to be rejected", a)
		}
	}
}
