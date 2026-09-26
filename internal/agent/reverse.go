// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/transport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// reverseAuthUser encodes the SSH username the reverse agent presents: just the
// server name once enrolled, or "<serverName>:<token>" during first-time
// enrollment so the controller can verify the join token before pinning the key.
func reverseAuthUser(serverName, enrollToken string) string {
	if enrollToken != "" {
		return serverName + ":" + enrollToken
	}
	return serverName
}

type ReverseOptions struct {
	ControllerAddress      string
	ControllerFingerprint  string
	ServerName             string
	EnrollToken            string
	EnrollTokenPath        string
	KnownHostsPath         string
	AcceptNewHostKey       bool
	MinRetryDelay          time.Duration
	MaxRetryDelay          time.Duration
	OfflineMetricsInterval time.Duration
	MetricsQueuePath       string
	NetworkDialContext     func(context.Context, string, string) (net.Conn, error)
	// Log receives a line for each failed connection attempt (rate-limited)
	// and for the recovery after one. Nil means os.Stderr.
	Log io.Writer
}

func DefaultControllerKnownHostsPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ".cenvero-fleet-agent/known_hosts"
	}
	return filepath.Join(home, ".cenvero-fleet-agent", "known_hosts")
}

func DefaultMetricsQueuePath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ".cenvero-fleet-agent/reverse-metrics.jsonl"
	}
	return filepath.Join(home, ".cenvero-fleet-agent", "reverse-metrics.jsonl")
}

func RunReverse(ctx context.Context, opts ReverseOptions, server Server) error {
	if opts.ControllerAddress == "" {
		return fmt.Errorf("controller address is required")
	}
	if opts.ServerName == "" {
		hostname, _ := os.Hostname()
		opts.ServerName = hostname
	}
	if opts.KnownHostsPath == "" {
		opts.KnownHostsPath = DefaultControllerKnownHostsPath()
	}
	if opts.EnrollToken == "" && opts.EnrollTokenPath != "" {
		info, err := os.Lstat(opts.EnrollTokenPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read enrollment token file metadata: %w", err)
		}
		if err == nil {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("enrollment token file %s must be a regular owner-only file (mode 0600)", opts.EnrollTokenPath)
			}
			data, err := os.ReadFile(opts.EnrollTokenPath)
			if err != nil {
				return fmt.Errorf("read enrollment token file: %w", err)
			}
			opts.EnrollToken = strings.TrimSpace(string(data))
			if opts.EnrollToken == "" {
				return fmt.Errorf("enrollment token file %s is empty", opts.EnrollTokenPath)
			}
		}
	}
	if opts.MinRetryDelay <= 0 {
		opts.MinRetryDelay = time.Second
	}
	if opts.MaxRetryDelay <= 0 || opts.MaxRetryDelay < opts.MinRetryDelay {
		opts.MaxRetryDelay = 30 * time.Second
	}
	if opts.OfflineMetricsInterval < 0 {
		opts.OfflineMetricsInterval = 0
	}
	if opts.MetricsQueuePath == "" {
		opts.MetricsQueuePath = DefaultMetricsQueuePath()
	}
	if strings.TrimSpace(server.ControllerAddress) == "" {
		server.ControllerAddress = opts.ControllerAddress
	}
	if strings.TrimSpace(server.ControllerKnownHostsPath) == "" {
		server.ControllerKnownHostsPath = opts.KnownHostsPath
	}
	if server.MetricsQueue == nil {
		server.MetricsQueue = NewFileMetricsQueue(opts.MetricsQueuePath)
	}

	// Offline metrics are queued on their own steady ticker, independent of the
	// reconnect loop, and only while no session is up.
	var connected atomic.Bool
	stopOffline := startOfflineMetrics(ctx, opts.OfflineMetricsInterval, server.metricsCollector(), server.metricsQueue(), &connected)
	defer stopOffline()

	retry := newReconnectBackoff(opts.MinRetryDelay, opts.MaxRetryDelay)
	logOut := opts.Log
	if logOut == nil {
		logOut = os.Stderr
	}
	attempts := newReconnectLog(logOut, opts.ControllerAddress)

	for {
		authenticated, err := runReverseSession(ctx, opts, server, &connected, attempts.connected)
		if authenticated {
			// The controller accepted this agent identity. Never present the one-use
			// credential on later reconnects, and remove the bootstrap file so it is
			// not retained in service argv, unit text, or on disk indefinitely.
			opts.EnrollToken = ""
			if opts.EnrollTokenPath != "" {
				if removeErr := os.Remove(opts.EnrollTokenPath); removeErr != nil && !os.IsNotExist(removeErr) {
					return fmt.Errorf("remove consumed enrollment token file: %w", removeErr)
				}
			}
		}
		// --accept-new-host-key authorizes a one-time re-pin of the controller's
		// host key on the FIRST connection attempt only. Clearing it afterwards
		// means every later reconnect uses strict pinning, so a MITM who shows up
		// at some future reconnect cannot silently swap the controller key —
		// re-trusting a changed key then requires a fresh operator action.
		// (First-use TOFU pinning, when no pin exists yet, happens regardless of
		// this flag, so clearing it never blocks a legitimate initial enrollment.)
		opts.AcceptNewHostKey = false
		if ctx.Err() != nil {
			return nil
		}

		delay := retry.next(authenticated, err)
		attempts.ended(authenticated, err, delay)
		if err := waitForReconnect(ctx, delay); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// reconnectLogRepeat is how long an identical failure stays quiet after it
// was last logged, so a long outage costs a line every few minutes rather
// than one per attempt.
const reconnectLogRepeat = 5 * time.Minute

// reconnectLog tells the operator why a reverse agent is not connected: each
// failed attempt with its reason and the next retry delay, identical repeats
// rate-limited, and the recovery once a session is up again. It is used only
// by the RunReverse loop's goroutine.
type reconnectLog struct {
	out        io.Writer
	controller string
	now        func() time.Time
	every      time.Duration
	seen       map[string]*reconnectLogEntry // by failure kind (see logKey)
	failures   int                           // failed attempts since the last session
}

type reconnectLogEntry struct {
	loggedAt   time.Time
	suppressed int
}

func newReconnectLog(out io.Writer, controller string) *reconnectLog {
	return &reconnectLog{out: out, controller: controller, now: time.Now, every: reconnectLogRepeat,
		seen: make(map[string]*reconnectLogEntry)}
}

// ended records how an attempt finished. A session that authenticated and
// then ended without an error is a normal disconnect and is not reported.
func (l *reconnectLog) ended(authenticated bool, err error, delay time.Duration) {
	if err == nil {
		return
	}
	l.failures++
	var reason string
	switch {
	case authenticated:
		reason = fmt.Sprintf("reverse session failed: %v", err)
	case strings.Contains(err.Error(), "unable to authenticate"):
		reason = fmt.Sprintf("controller rejected this agent (check --server-name; an agent that is not enrolled needs a fresh enrollment token): %v", err)
	default:
		reason = fmt.Sprintf("connection attempt failed: %v", err)
	}
	key := logKey(reason)
	now := l.now()
	entry := l.seen[key]
	if entry != nil && now.Sub(entry.loggedAt) < l.every {
		entry.suppressed++
		return
	}
	line := fmt.Sprintf("fleet-agent: %s; retrying in %s", reason, delay.Round(10*time.Millisecond))
	if entry != nil && entry.suppressed > 0 {
		line += fmt.Sprintf(" (repeated %d more times since last logged)", entry.suppressed)
	}
	fmt.Fprintln(l.out, line)
	if entry == nil {
		if len(l.seen) >= 32 {
			clear(l.seen) // many distinct failures: forget old ones rather than grow
		}
		entry = &reconnectLogEntry{}
		l.seen[key] = entry
	}
	entry.loggedAt, entry.suppressed = now, 0
}

// connected records that a session is up; after failures it says so.
func (l *reconnectLog) connected() {
	if l.failures > 0 {
		fmt.Fprintf(l.out, "fleet-agent: connected to controller %s after %d failed attempts\n", l.controller, l.failures)
	}
	l.failures = 0
	clear(l.seen)
}

// logKey groups failures that differ only in numbers, such as the local
// port of each attempt in a timeout message, so they are rate-limited as one.
func logKey(reason string) string {
	var b strings.Builder
	digits := false
	for _, r := range reason {
		if r >= '0' && r <= '9' {
			if !digits {
				b.WriteByte('#')
			}
			digits = true
			continue
		}
		digits = false
		b.WriteRune(r)
	}
	return b.String()
}

// reconnectBackoff decides how long the reverse agent waits before its next
// connection attempt.
type reconnectBackoff struct {
	min, max, current time.Duration
	jitter            func(time.Duration) time.Duration
}

func newReconnectBackoff(minDelay, maxDelay time.Duration) *reconnectBackoff {
	return &reconnectBackoff{min: minDelay, max: maxDelay, current: minDelay, jitter: reconnectJitter}
}

// next returns the delay before the next attempt, given how the last one ended.
//
// A session that authenticated and later ended cleanly — the usual picture of
// a controller restart — proves the controller was reachable, so the delay
// starts over from the minimum. The reset used to happen only after the wait
// had already been taken from the old value, so after any earlier outage the
// agent sat out the leftover backoff (up to --retry-max, 30 s by default)
// before reconnecting to a controller that was back within a second. Failed
// attempts keep doubling the delay up to the maximum as before.
//
// Every delay is jittered by ±20% so a fleet of agents that lost the same
// controller at the same moment does not come back in lockstep.
func (b *reconnectBackoff) next(authenticated bool, err error) time.Duration {
	if authenticated && err == nil {
		b.current = b.min
	}
	wait := b.current
	if err != nil {
		b.current = nextBackoff(b.current, b.max)
	}
	if b.jitter != nil {
		wait = b.jitter(wait)
	}
	return wait
}

// reconnectJitter spreads d uniformly over [0.8d, 1.2d].
var reconnectJitter = func(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// #nosec G404 -- jitter only de-synchronises reconnect timing; it guards nothing.
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func runReverseSession(ctx context.Context, opts ReverseOptions, server Server, connected *atomic.Bool, onConnected func()) (bool, error) {
	signer, err := fleetcrypto.EnsureEd25519Signer(server.HostKeyPath)
	if err != nil {
		return false, err
	}

	hostKeyCallback, err := verifiedControllerHostKeyCallback(opts.KnownHostsPath, opts.ControllerFingerprint, opts.AcceptNewHostKey)
	if err != nil {
		return false, err
	}

	config := &ssh.ClientConfig{
		Config: ssh.Config{
			Ciphers:      transport.SupportedCiphers(),
			KeyExchanges: transport.SupportedKEX(),
			MACs:         transport.SupportedMACs(),
		},
		User:              reverseAuthUser(opts.ServerName, opts.EnrollToken),
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: transport.SupportedHostKeyAlgos(),
		Timeout:           10 * time.Second,
	}

	var rawConn net.Conn
	if opts.NetworkDialContext != nil {
		rawConn, err = opts.NetworkDialContext(ctx, "tcp", opts.ControllerAddress)
	} else {
		rawConn, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", opts.ControllerAddress)
	}
	if err != nil {
		return false, fmt.Errorf("dial controller %s: %w", opts.ControllerAddress, err)
	}
	defer rawConn.Close()

	handshakeDeadline := time.Now().Add(config.Timeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	if err := rawConn.SetDeadline(handshakeDeadline); err != nil {
		return false, fmt.Errorf("set reverse ssh handshake deadline: %w", err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, opts.ControllerAddress, config)
	if err != nil {
		return false, fmt.Errorf("establish reverse ssh connection to controller %s: %w", opts.ControllerAddress, err)
	}
	if err := rawConn.SetDeadline(time.Time{}); err != nil {
		_ = sshConn.Close()
		return true, fmt.Errorf("clear reverse ssh handshake deadline: %w", err)
	}
	if opts.EnrollTokenPath != "" && opts.EnrollToken != "" {
		if err := os.Remove(opts.EnrollTokenPath); err != nil && !os.IsNotExist(err) {
			_ = sshConn.Close()
			return true, fmt.Errorf("remove consumed enrollment token file: %w", err)
		}
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()
	// Probe the controller so a connection whose far end vanished without a
	// FIN (controller host lost, NAT state dropped) is torn down within about a
	// minute and the agent reconnects, instead of waiting on it indefinitely.
	// Every controller answers the probe, older ones included.
	stopKeepalive := transport.StartKeepalive(client, reverseKeepaliveInterval, reverseKeepaliveMaxMissed, nil)
	defer stopKeepalive()

	// Accept extra fleet-rpc channels the controller opens back to us before
	// opening our own, so none is missed in between.
	//
	// SSH channels can be opened from either end. A reverse agent used to open
	// exactly one and serve only that, so every RPC the controller made — every
	// chunk of a file transfer included — had to queue behind one serialised
	// channel. Serving inbound channels lets a reverse transfer use as many
	// streams as a direct-mode one. Rejecting them, as an older agent does,
	// simply leaves the controller on the single-channel path.
	inbound := client.HandleChannelOpen(transport.RPCChannelType)
	go serveReverseInboundChannels(inbound, server)

	channel, requests, err := client.OpenChannel(transport.RPCChannelType, nil)
	if err != nil {
		return true, fmt.Errorf("open %s channel: %w", transport.RPCChannelType, err)
	}
	go ssh.DiscardRequests(requests)

	server.Mode = transport.ModeReverse
	if connected != nil {
		connected.Store(true)
		defer connected.Store(false)
	}
	if onConnected != nil {
		onConnected()
	}
	done := make(chan error, 1)
	go func() {
		server.serveRPC(channel)
		done <- nil
	}()

	select {
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return true, nil
	case err := <-done:
		return true, err
	}
}

// verifiedControllerHostKeyCallback preserves strict reconnects to an existing
// pin, but refuses to create a first pin unless the presented controller key
// matches an out-of-band fingerprint provisioned by the controller. This closes
// the reverse enrollment MITM window that plain TOFU leaves open.
func verifiedControllerHostKeyCallback(path, expectedFingerprint string, forceReplace bool) (ssh.HostKeyCallback, error) {
	tofu, err := transport.NewTOFUHostKeyCallback(path, forceReplace, &transport.HostKeyState{})
	if err != nil {
		return nil, err
	}
	known, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("load controller known_hosts %s: %w", path, err)
	}
	expectedFingerprint = strings.TrimSpace(expectedFingerprint)
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := known(hostname, remote, key)
		if err == nil {
			return tofu(hostname, remote, key)
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return tofu(hostname, remote, key)
		}
		if len(keyErr.Want) != 0 {
			// Existing-key mismatch is handled by the normal strict/explicit-repin
			// callback so its established behavior is preserved; a rejection
			// names both keys so the operator can tell what changed.
			if err := tofu(hostname, remote, key); err != nil {
				pinned := make([]string, 0, len(keyErr.Want))
				for _, want := range keyErr.Want {
					pinned = append(pinned, ssh.FingerprintSHA256(want.Key))
				}
				return fmt.Errorf("%w: expected %s, presented %s", err, strings.Join(pinned, " or "), ssh.FingerprintSHA256(key))
			}
			return nil
		}
		if expectedFingerprint == "" {
			return fmt.Errorf("controller %s is not pinned; --controller-fingerprint is required for first enrollment", hostname)
		}
		presented := ssh.FingerprintSHA256(key)
		if subtle.ConstantTimeCompare([]byte(presented), []byte(expectedFingerprint)) != 1 {
			return fmt.Errorf("controller fingerprint mismatch for %s: expected %s, presented %s", hostname, expectedFingerprint, presented)
		}
		return tofu(hostname, remote, key)
	}, nil
}

// maxReverseInboundChannels caps how many controller-opened fleet-rpc channels a
// reverse agent will serve at once. The controller is authenticated and pinned,
// so this is a resource guard rather than a trust boundary — it keeps a buggy or
// runaway controller from spawning unbounded goroutines on the agent. It sits
// comfortably above any transfer's stream count.
const maxReverseInboundChannels = 32

// serveReverseInboundChannels serves fleet-rpc channels opened by the controller
// over an established reverse connection. It returns when the channel-open feed
// closes, which happens when the connection goes away.
func serveReverseInboundChannels(inbound <-chan ssh.NewChannel, server Server) {
	if inbound == nil {
		return // another handler is already registered for this type
	}
	slots := make(chan struct{}, maxReverseInboundChannels)
	for newChannel := range inbound {
		select {
		case slots <- struct{}{}:
		default:
			_ = newChannel.Reject(ssh.ResourceShortage, "too many concurrent channels")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			<-slots
			continue
		}
		go ssh.DiscardRequests(requests)
		go func() {
			defer func() { <-slots }()
			server.serveRPC(channel)
		}()
	}
}

// Keepalive settings for the agent's connection to the controller; variables so
// tests can shorten them.
var (
	reverseKeepaliveInterval  = transport.DefaultKeepaliveInterval
	reverseKeepaliveMaxMissed = transport.DefaultKeepaliveMaxMissed
)

func waitForReconnect(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// startOfflineMetrics queues one local metrics snapshot per interval for as
// long as the agent has no live session with the controller, so the history the
// controller replays later has the configured resolution.
//
// Snapshots used to be taken inside the reconnect wait: one on every attempt,
// plus one per interval during the wait. With the default --retry-max (30 s)
// shorter than --offline-metrics-interval (1 m) every wait ended before its
// ticker fired, so the queue grew at one snapshot per attempt — twice the
// configured rate, and faster still with shorter retry settings. A steady
// ticker decouples the two.
func startOfflineMetrics(ctx context.Context, interval time.Duration, collector MetricsCollector, queue MetricsQueue, connected *atomic.Bool) (stop func()) {
	if interval <= 0 || collector == nil || queue == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if connected == nil || !connected.Load() {
					collectOfflineMetric(ctx, collector, queue)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func collectOfflineMetric(ctx context.Context, collector MetricsCollector, queue MetricsQueue) {
	if collector == nil || queue == nil {
		return
	}
	snapshot, err := collector.Collect(ctx)
	if err != nil {
		return
	}
	_ = queue.Enqueue(snapshot)
}

func nextBackoff(current, maxDelay time.Duration) time.Duration {
	if current <= 0 {
		return time.Second
	}
	next := current * 2
	if next > maxDelay {
		return maxDelay
	}
	return next
}
