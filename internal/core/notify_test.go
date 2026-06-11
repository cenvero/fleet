// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestIsBlockedSSRFIP covers the address-classification policy: 169.254.169.254
// is ALWAYS blocked; loopback/link-local/private are blocked only without the
// allow-internal opt-in; public addresses are always allowed.
func TestIsBlockedSSRFIP(t *testing.T) {
	cases := []struct {
		ip            string
		allowInternal bool
		blocked       bool
	}{
		{"169.254.169.254", false, true}, // cloud metadata: blocked
		{"169.254.169.254", true, true},  // cloud metadata: blocked even with opt-in
		{"127.0.0.1", false, true},       // loopback
		{"127.0.0.1", true, false},       // loopback allowed with opt-in
		{"::1", false, true},             // loopback v6
		{"10.1.2.3", false, true},        // RFC1918
		{"10.1.2.3", true, false},        // RFC1918 allowed with opt-in
		{"192.168.0.5", false, true},     // RFC1918
		{"172.16.9.9", false, true},      // RFC1918
		{"169.254.10.20", false, true},   // link-local (not metadata)
		{"0.0.0.0", false, true},         // unspecified
		{"8.8.8.8", false, false},        // public
		{"1.1.1.1", true, false},         // public
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", c.ip)
		}
		if got := isBlockedSSRFIP(ip, c.allowInternal); got != c.blocked {
			t.Errorf("isBlockedSSRFIP(%s, allowInternal=%v) = %v, want %v", c.ip, c.allowInternal, got, c.blocked)
		}
	}
}

// TestGuardSSRFLiteralIPs checks the URL-level guard for literal-IP hosts.
func TestGuardSSRFLiteralIPs(t *testing.T) {
	// Metadata IP is always refused.
	if _, err := guardSSRF("http://169.254.169.254/latest/meta-data/", false); err == nil {
		t.Fatal("guardSSRF should block the cloud metadata IP")
	}
	if _, err := guardSSRF("http://169.254.169.254/", true); err == nil {
		t.Fatal("guardSSRF should block the cloud metadata IP even with allow-internal")
	}
	// Loopback blocked without opt-in, allowed with it.
	if _, err := guardSSRF("http://127.0.0.1:8080/hook", false); err == nil {
		t.Fatal("guardSSRF should block loopback without allow-internal")
	}
	if ip, err := guardSSRF("http://127.0.0.1:8080/hook", true); err != nil {
		t.Fatalf("guardSSRF should allow loopback with allow-internal: %v", err)
	} else if !ip.IsLoopback() {
		t.Fatalf("guardSSRF returned %v, want loopback", ip)
	}
	// Public literal IP is allowed.
	if _, err := guardSSRF("https://8.8.8.8/notify", false); err != nil {
		t.Fatalf("guardSSRF should allow a public IP: %v", err)
	}
	// A URL with no host is refused.
	if _, err := guardSSRF("http:///path-only", false); err == nil {
		t.Fatal("guardSSRF should reject a URL with no host")
	}
}

// TestSendBlocksMetadataIP asserts an end-to-end Send refuses the metadata IP and
// never opens a connection (the error is the SSRF refusal, not a dial error).
func TestSendBlocksMetadataIP(t *testing.T) {
	store := NewNotifyStore(t.TempDir())
	// Even with allow-internal, the metadata IP must be refused.
	target := NotifyTarget{Kind: NotifyKindWebhook, URL: "http://169.254.169.254/latest/meta-data/iam/", AllowInternal: true}
	err := store.Send(target, NotifyEventOffline, "hi")
	if err == nil {
		t.Fatal("Send to metadata IP should error")
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("Send error = %q, want a cloud-metadata refusal", err)
	}
}

// TestSendBlocksLoopbackWithoutOptIn confirms a loopback webhook is refused
// unless AllowInternal is set — and that the refusal happens BEFORE any request
// reaches the server.
func TestSendBlocksLoopbackWithoutOptIn(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := NewNotifyStore(t.TempDir())

	// Without opt-in: refused, server never hit.
	blocked := NotifyTarget{Kind: NotifyKindWebhook, URL: srv.URL}
	if err := store.Send(blocked, NotifyEventOffline, "msg"); err == nil {
		t.Fatal("Send to loopback without allow-internal should error")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("server was hit %d times despite SSRF block", n)
	}

	// With opt-in: delivered.
	allowed := NotifyTarget{Kind: NotifyKindWebhook, URL: srv.URL, AllowInternal: true}
	if err := store.Send(allowed, NotifyEventOffline, "msg"); err != nil {
		t.Fatalf("Send to loopback WITH allow-internal should succeed: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("server hit %d times, want 1 after allow-internal delivery", n)
	}
}

// TestSendPinnedDialReachesAllowedServer is a smoke test that the pinned-dial
// transport still actually delivers to an allowed (loopback, opted-in) endpoint.
func TestSendPinnedDialReachesAllowedServer(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	store := NewNotifyStore(t.TempDir())
	target := NotifyTarget{Kind: NotifyKindSlack, URL: srv.URL, AllowInternal: true}
	if err := store.Send(target, NotifyEventOnline, "ping"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(gotBody, "ping") {
		t.Fatalf("server received body %q, want it to contain the message", gotBody)
	}
}
