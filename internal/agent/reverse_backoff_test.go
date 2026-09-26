// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

func TestReconnectBackoffResetsAfterHealthySession(t *testing.T) {
	b := newReconnectBackoff(100*time.Millisecond, 3*time.Second)
	b.jitter = nil // exact values

	failed := errors.New("controller offline")
	var waits []time.Duration
	// An outage: every attempt fails, so the delay doubles up to the cap.
	for i := 0; i < 7; i++ {
		waits = append(waits, b.next(false, failed))
	}
	want := []time.Duration{100, 200, 400, 800, 1600, 3000, 3000}
	for i, w := range want {
		if waits[i] != w*time.Millisecond {
			t.Fatalf("outage wait %d = %s, want %s (all: %v)", i, waits[i], w*time.Millisecond, waits)
		}
	}
	// The controller came back; that session ran and ended cleanly (e.g. the
	// controller restarted). The next reconnect must use the minimum delay,
	// not the 3s left over from the outage — the off-by-one this fixes.
	if got := b.next(true, nil); got != 100*time.Millisecond {
		t.Fatalf("wait after a healthy session = %s, want the minimum 100ms", got)
	}
	// And failures start doubling from the minimum again, exactly as at start.
	if got := b.next(false, failed); got != 100*time.Millisecond {
		t.Fatalf("first failure after recovery waited %s, want 100ms", got)
	}
	if got := b.next(false, failed); got != 200*time.Millisecond {
		t.Fatalf("second failure after recovery waited %s, want 200ms", got)
	}
	// A session that authenticated but then failed keeps backing off.
	if got := b.next(true, failed); got != 400*time.Millisecond {
		t.Fatalf("authenticated-then-failed wait = %s, want continued backoff 400ms", got)
	}
}

func TestReconnectJitterStaysWithinTwentyPercent(t *testing.T) {
	const base = time.Second
	seenLow, seenHigh := false, false
	for i := 0; i < 2000; i++ {
		got := reconnectJitter(base)
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("jittered %s to %s, outside ±20%%", base, got)
		}
		seenLow = seenLow || got < 950*time.Millisecond
		seenHigh = seenHigh || got > 1050*time.Millisecond
	}
	if !seenLow || !seenHigh {
		t.Fatal("jitter does not spread delays across the range")
	}
	if reconnectJitter(0) != 0 {
		t.Fatal("a zero delay must stay zero")
	}
}

// TestRunReverseReconnectsPromptlyAfterControllerRestart drives RunReverse
// through an outage (backoff grows to the maximum), then a healthy session
// that the controller ends, and checks the following reconnect happens after
// about the minimum delay rather than the leftover maximum.
func TestRunReverseReconnectsPromptlyAfterControllerRestart(t *testing.T) {
	hostKey := newTestHostSigner(t)
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	const minDelay, maxDelay = 20 * time.Millisecond, 600 * time.Millisecond

	var mu sync.Mutex
	var dialTimes []time.Time
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunReverse(ctx, ReverseOptions{
			ControllerAddress:     "127.0.0.1:9443",
			ControllerFingerprint: ssh.FingerprintSHA256(hostKey.PublicKey()),
			ServerName:            "rev",
			KnownHostsPath:        knownHosts,
			MinRetryDelay:         minDelay,
			MaxRetryDelay:         maxDelay,
			NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
				n := attempts.Add(1)
				mu.Lock()
				dialTimes = append(dialTimes, time.Now())
				mu.Unlock()
				// Attempts 1-6 fail (backoff reaches the 600ms cap), 7 gets a
				// controller that accepts and then goes away, 8 is the one we time.
				if n < 7 || n > 7 {
					if n > 7 {
						cancel()
					}
					return nil, errors.New("controller offline")
				}
				clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:41000", "127.0.0.1:9443")
				// Accepts, serves ~50ms, then drops the connection.
				go serveFakeController(serverConn, hostKey)
				return clientConn, nil
			},
		}, Server{Mode: transport.ModeReverse, HostKeyPath: filepath.Join(t.TempDir(), "agent_key")})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunReverse did not reach the post-restart reconnect")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialTimes) < 8 {
		t.Fatalf("unexpected dial sequence: %d dials", len(dialTimes))
	}
	// Dial 7 → handshake → ~50ms session → controller drops it → wait → dial 8.
	// With the leftover-backoff bug the wait alone was the 600ms maximum.
	gap := dialTimes[7].Sub(dialTimes[6])
	t.Logf("healthy session + reconnect took %s (min delay %s, max %s)", gap, minDelay, maxDelay)
	if gap >= maxDelay {
		t.Fatalf("reconnect after a healthy session took %s — it waited out the leftover outage backoff instead of the minimum %s", gap, minDelay)
	}
}

type countingCollector struct{ n atomic.Int32 }

func (c *countingCollector) Collect(context.Context) (proto.MetricsSnapshot, error) {
	n := c.n.Add(1)
	return proto.MetricsSnapshot{Timestamp: time.Now().UTC(), ProcessCount: uint64(n)}, nil
}

type countingQueue struct {
	noopMetricsQueue
	n atomic.Int32
}

func (q *countingQueue) Enqueue(proto.MetricsSnapshot) error {
	q.n.Add(1)
	return nil
}

// TestOfflineMetricsFollowTheConfiguredInterval: with reconnect attempts much
// more frequent than the offline-metrics interval, snapshots must still be
// queued at the configured rate, not once per attempt.
func TestOfflineMetricsFollowTheConfiguredInterval(t *testing.T) {
	collector := &countingCollector{}
	queue := &countingQueue{}
	var attempts atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 550*time.Millisecond)
	defer cancel()
	err := RunReverse(ctx, ReverseOptions{
		ControllerAddress:      "127.0.0.1:9443",
		ControllerFingerprint:  "SHA256:unused",
		ServerName:             "rev",
		KnownHostsPath:         filepath.Join(t.TempDir(), "known_hosts"),
		MinRetryDelay:          5 * time.Millisecond,
		MaxRetryDelay:          10 * time.Millisecond,
		OfflineMetricsInterval: 100 * time.Millisecond,
		NetworkDialContext: func(context.Context, string, string) (net.Conn, error) {
			attempts.Add(1)
			return nil, errors.New("controller offline")
		},
	}, Server{Mode: transport.ModeReverse, HostKeyPath: filepath.Join(t.TempDir(), "agent_key"),
		MetricsCollector: collector, MetricsQueue: queue})
	if err != nil {
		t.Fatalf("RunReverse error = %v", err)
	}
	queued := queue.n.Load()
	t.Logf("%d reconnect attempts, %d offline snapshots queued in 550ms at a 100ms interval", attempts.Load(), queued)
	if attempts.Load() < 20 {
		t.Fatalf("test needs frequent reconnect attempts, got %d", attempts.Load())
	}
	if queued < 3 || queued > 6 {
		t.Fatalf("queued %d snapshots in 550ms at a 100ms interval; want ~5 (one per interval, not one per attempt)", queued)
	}
}

// While a session is up nothing is queued: the controller collects live.
func TestOfflineMetricsPauseWhileConnected(t *testing.T) {
	collector := &countingCollector{}
	queue := &countingQueue{}
	var connected atomic.Bool
	connected.Store(true)
	stop := startOfflineMetrics(context.Background(), 10*time.Millisecond, collector, queue, &connected)
	time.Sleep(60 * time.Millisecond)
	if queue.n.Load() != 0 {
		t.Fatalf("queued %d snapshots while connected", queue.n.Load())
	}
	connected.Store(false)
	time.Sleep(60 * time.Millisecond)
	stop()
	if queue.n.Load() == 0 {
		t.Fatal("nothing queued after the session ended")
	}
}
