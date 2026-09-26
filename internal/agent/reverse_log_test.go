// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"golang.org/x/crypto/ssh"
)

// syncBuffer is a bytes.Buffer safe to read while RunReverse writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func logLines(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '\n' })
}

// A reverse agent that could not connect used to say nothing at all, so an
// operator had no way to tell why it stayed offline. Each failure is now
// logged with its reason and the next delay, and identical repeats are
// rate-limited so a long outage does not flood the log.
func TestReconnectLogRateLimitsRepeatedFailures(t *testing.T) {
	var out bytes.Buffer
	l := newReconnectLog(&out, "ctl.example:9443")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	// Ten timeouts over 4.5 minutes, each from a different local port.
	for i := range 10 {
		l.ended(false, fmt.Errorf("dial controller ctl.example:9443: read tcp 127.0.0.1:%d->10.0.0.1:9443: i/o timeout", 40000+i), 2*time.Second)
		now = now.Add(30 * time.Second)
	}
	lines := logLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("got %d lines for one repeated failure, want 1:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "connection attempt failed: dial controller ctl.example:9443") || !strings.Contains(lines[0], "i/o timeout") ||
		!strings.HasSuffix(lines[0], "; retrying in 2s") {
		t.Fatalf("line lacks the reason or the retry delay: %q", lines[0])
	}

	// Past the window the failure is logged again, with how often it recurred.
	now = now.Add(time.Minute)
	l.ended(false, errors.New("dial controller ctl.example:9443: read tcp 127.0.0.1:41000->10.0.0.1:9443: i/o timeout"), 30*time.Second)
	lines = logLines(out.String())
	if len(lines) != 2 || !strings.Contains(lines[1], "retrying in 30s (repeated 9 more times since last logged)") {
		t.Fatalf("expected a periodic reminder with the repeat count, got:\n%s", out.String())
	}

	// A different failure is not held back by the first one.
	l.ended(false, errors.New("establish reverse ssh connection to controller ctl.example:9443: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"), 30*time.Second)
	lines = logLines(out.String())
	if len(lines) != 3 || !strings.Contains(lines[2], "rejected this agent") {
		t.Fatalf("an authentication failure was not logged as one:\n%s", out.String())
	}

	// Recovery is reported, and a later failure is logged straight away.
	l.connected()
	l.ended(true, errors.New("open fleet-rpc channel: EOF"), time.Second)
	lines = logLines(out.String())
	if len(lines) != 5 || lines[3] != "fleet-agent: connected to controller ctl.example:9443 after 12 failed attempts" ||
		!strings.Contains(lines[4], "reverse session failed: open fleet-rpc channel") {
		t.Fatalf("recovery or the next failure missing:\n%s", out.String())
	}

	// A session that ends cleanly (controller restart) is not a failure.
	l.ended(true, nil, time.Second)
	if n := len(logLines(out.String())); n != 5 {
		t.Fatalf("a clean disconnect was logged as a failure:\n%s", out.String())
	}
}

// A controller presenting the wrong host key must be logged with the
// expected and presented fingerprints, once, however often the agent retries.
func TestRunReverseLogsFingerprintMismatch(t *testing.T) {
	t.Parallel()
	const controllerAddr = "127.0.0.1:9443"
	dir := t.TempDir()
	expected := newTestHostSigner(t)
	presented := newTestHostSigner(t)

	var dials atomic.Int32
	dial := func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:55555", controllerAddr)
		go serveFakeController(serverConn, presented)
		return clientConn, nil
	}
	var log syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RunReverse(ctx, ReverseOptions{
			ControllerAddress:     controllerAddr,
			ControllerFingerprint: ssh.FingerprintSHA256(expected.PublicKey()),
			ServerName:            "rev",
			KnownHostsPath:        filepath.Join(dir, "known_hosts"),
			MinRetryDelay:         time.Millisecond,
			MaxRetryDelay:         2 * time.Millisecond,
			NetworkDialContext:    dial,
			Log:                   &log,
		}, Server{Mode: transport.ModeReverse, HostKeyPath: filepath.Join(dir, "agent_key")})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for dials.Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("agent made only %d attempts", dials.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	lines := logLines(log.String())
	if len(lines) != 1 {
		t.Fatalf("%d attempts produced %d lines, want 1 (rate-limited):\n%s", dials.Load(), len(lines), log.String())
	}
	want := fmt.Sprintf("expected %s, presented %s", ssh.FingerprintSHA256(expected.PublicKey()), ssh.FingerprintSHA256(presented.PublicKey()))
	if !strings.Contains(lines[0], want) || !strings.Contains(lines[0], "retrying in") {
		t.Fatalf("log line %q lacks %q or the retry delay", lines[0], want)
	}
}

// A controller whose key no longer matches the pinned one is rejected, and
// the error names the pinned and the presented fingerprints.
func TestPinnedControllerKeyMismatchNamesFingerprints(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "known_hosts")
	address := "127.0.0.1:9443"
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9443}
	pinned := newTestHostSigner(t).PublicKey()
	other := newTestHostSigner(t).PublicKey()

	cb, err := verifiedControllerHostKeyCallback(path, ssh.FingerprintSHA256(pinned), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb(address, remote, pinned); err != nil {
		t.Fatalf("first pin: %v", err)
	}
	cb, err = verifiedControllerHostKeyCallback(path, ssh.FingerprintSHA256(pinned), false)
	if err != nil {
		t.Fatal(err)
	}
	err = cb(address, remote, other)
	if err == nil {
		t.Fatal("a changed controller key was accepted")
	}
	want := fmt.Sprintf("expected %s, presented %s", ssh.FingerprintSHA256(pinned), ssh.FingerprintSHA256(other))
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q lacks %q", err, want)
	}
}
