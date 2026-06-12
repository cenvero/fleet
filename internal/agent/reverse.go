// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/transport"
	"golang.org/x/crypto/ssh"
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
	ServerName             string
	EnrollToken            string
	KnownHostsPath         string
	AcceptNewHostKey       bool
	MinRetryDelay          time.Duration
	MaxRetryDelay          time.Duration
	OfflineMetricsInterval time.Duration
	MetricsQueuePath       string
	NetworkDialContext     func(context.Context, string, string) (net.Conn, error)
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

	backoff := opts.MinRetryDelay

	for {
		err := runReverseSession(ctx, opts, server)
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

		wait := backoff
		if err == nil {
			backoff = opts.MinRetryDelay
		} else {
			backoff = nextBackoff(backoff, opts.MaxRetryDelay)
		}
		if err := waitForReconnect(ctx, wait, opts.OfflineMetricsInterval, server.metricsCollector(), server.metricsQueue()); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func runReverseSession(ctx context.Context, opts ReverseOptions, server Server) error {
	signer, err := fleetcrypto.EnsureEd25519Signer(server.HostKeyPath)
	if err != nil {
		return err
	}

	hostKeyCallback, err := transport.NewTOFUHostKeyCallback(opts.KnownHostsPath, opts.AcceptNewHostKey, &transport.HostKeyState{})
	if err != nil {
		return err
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
		return fmt.Errorf("dial controller %s: %w", opts.ControllerAddress, err)
	}
	defer rawConn.Close()

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, opts.ControllerAddress, config)
	if err != nil {
		return fmt.Errorf("establish reverse ssh connection to controller %s: %w", opts.ControllerAddress, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	channel, requests, err := client.OpenChannel(transport.RPCChannelType, nil)
	if err != nil {
		return fmt.Errorf("open %s channel: %w", transport.RPCChannelType, err)
	}
	go ssh.DiscardRequests(requests)

	server.Mode = transport.ModeReverse
	done := make(chan error, 1)
	go func() {
		server.serveRPC(channel)
		done <- nil
	}()

	select {
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return nil
	case err := <-done:
		return err
	}
}

func waitForReconnect(ctx context.Context, delay, interval time.Duration, collector MetricsCollector, queue MetricsQueue) error {
	if interval > 0 {
		collectOfflineMetric(ctx, collector, queue)
	}
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	var ticker *time.Ticker
	if interval > 0 {
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-tickerChan(ticker):
			collectOfflineMetric(ctx, collector, queue)
		}
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

func tickerChan(ticker *time.Ticker) <-chan time.Time {
	if ticker == nil {
		return nil
	}
	return ticker.C
}
