// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/transport"
)

const (
	sshKeepaliveInterval = 10 * time.Second
	sshKeepaliveTimeout  = 15 * time.Second
	sshReconnectDelay    = 5 * time.Second
	sshMaxRetries        = 3
)

// SSH wire-format structs for PTY and window-change channel requests (RFC 4254).
// These are sent as channel requests on the fleet-shell channel — same wire
// encoding used by standard SSH, but over the fleet agent's own authenticated
// channel type so only fleet clients can open a shell.
type ptyRequestPayload struct {
	Term     string
	Columns  uint32
	Rows     uint32
	Width    uint32
	Height   uint32
	Modelist string
}

type windowChangePayload struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

// RunSSHSession opens an interactive shell through the fleet agent's own SSH
// transport (fleet-shell channel on the agent port — not port 22).
//
// Security model:
//   - The connection is authenticated with the controller's Ed25519 key.
//   - The channel type "fleet-shell" is unknown to standard SSH clients — a
//     port scanner will see "SSH-2.0-cenvero-fleet-agent" and be unable to open
//     any session without both the correct key and the fleet channel type.
//
// If the connection drops it automatically retries up to 3 times before giving up.
func (a *App) RunSSHSession(serverName string, out io.Writer) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	// Use the per-server key override if set; fall back to the primary controller key.
	keyPath := a.serverPrivateKeyPath(server)
	signer, err := crypto.LoadPrivateKeySigner(keyPath, nil)
	if err != nil {
		return fmt.Errorf("load private key: %w", err)
	}

	knownHostsPath := a.Config.Crypto.KnownHostsPath
	promptFn := func(hostname, oldFP, newFP string) bool {
		fmt.Fprintf(out, "\n  WARNING: host key for %s has changed!\n", hostname)
		fmt.Fprintf(out, "   Old fingerprint: %s\n", oldFP)
		fmt.Fprintf(out, "   New fingerprint: %s\n", newFP)
		fmt.Fprintf(out, "\nThis could indicate a server rebuild or a man-in-the-middle attack.\n")
		fmt.Fprintf(out, "Accept the new key? Type 'yes' to continue: ")
		reader := bufio.NewReader(os.Stdin)
		resp := transport.ReadLine(reader)
		return strings.ToLower(resp) == "yes"
	}

	user := server.User
	if user == "" {
		user = "root"
	}
	addr := fmt.Sprintf("%s:%d", server.Address, server.Port)

	// HostKeyCallback is intentionally NOT set here — it is created fresh
	// inside each runSSHOnce call so that reconnect attempts re-read the
	// current known_hosts file and don't re-pin already-known hosts.
	clientConfig := &ssh.ClientConfig{
		User:    user,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: 15 * time.Second,
	}

	for attempt := 0; attempt <= sshMaxRetries; attempt++ {
		if attempt > 0 {
			fmt.Fprintf(out, "\r\nReconnecting... (attempt %d/%d)\r\n", attempt, sshMaxRetries)
			time.Sleep(sshReconnectDelay)
		}

		err = a.runSSHOnce(addr, clientConfig, knownHostsPath, promptFn, out)
		if err == nil {
			return nil
		}
		if isTerminalSSHError(err) {
			return err
		}

		if attempt < sshMaxRetries {
			fmt.Fprintf(out, "\r\nConnection lost. Reconnecting in %ds... (%d/%d)\r\n",
				int(sshReconnectDelay.Seconds()), attempt+1, sshMaxRetries)
		}
	}

	return fmt.Errorf("disconnected — could not reconnect after %d attempts", sshMaxRetries)
}

func (a *App) runSSHOnce(addr string, cfg *ssh.ClientConfig, knownHostsPath string, promptFn func(string, string, string) bool, out io.Writer) error {
	// Fresh callback on every attempt: re-reads known_hosts from disk so a
	// host pinned in attempt N is visible to attempt N+1 and won't be re-pinned.
	var state transport.HostKeyState
	hostKeyCallback, err := transport.NewInteractiveHostKeyCallback(knownHostsPath, promptFn, &state)
	if err != nil {
		return fmt.Errorf("known_hosts: %w", err)
	}
	cfg.HostKeyCallback = hostKeyCallback

	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	// Only show fingerprint the very first time a host is pinned (TOFU).
	// Already-known hosts show nothing. Host-key changes trigger the warning
	// prompt handled by promptFn in RunSSHSession above.
	if state.Outcome == "pinned" {
		fmt.Fprintf(out, "Fingerprint: %s (saved to fleet known hosts)\n", state.Fingerprint)
	}

	// Keepalive: ping every 10s; if the server doesn't reply within 15s, close
	// the connection so io.Copy unblocks and the outer loop can reconnect.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(sshKeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// AfterFunc closes the connection if SendRequest blocks past the timeout.
				closeTimer := time.AfterFunc(sshKeepaliveTimeout, func() { _ = client.Close() })
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				closeTimer.Stop()
				if err != nil {
					_ = client.Close()
					return
				}
			}
		}
	}()

	// Open a fleet-shell channel instead of a standard "session" channel.
	// The agent only accepts fleet-rpc and fleet-shell — standard SSH clients
	// connecting to the agent port cannot open any shell.
	channel, reqs, err := client.OpenChannel(transport.ShellChannelType, nil)
	if err != nil {
		return fmt.Errorf("open fleet-shell channel: %w", err)
	}
	defer channel.Close()

	// Track clean exit: agent sends "exit-status" before closing the channel
	// (SSH protocol ordering). reqsDone is closed when the goroutine has
	// drained all requests — including exit-status — so we wait for it after
	// io.Copy returns instead of racing against it.
	cleanExit := make(chan struct{}, 1)
	reqsDone := make(chan struct{})
	go func() {
		defer close(reqsDone)
		for req := range reqs {
			if req.Type == "exit-status" {
				select {
				case cleanExit <- struct{}{}:
				default:
				}
			}
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()

	fd := stdinFd()
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("make raw terminal: %w", err)
		}
		defer term.Restore(fd, oldState) //nolint:errcheck

		w, h, _ := term.GetSize(fd)
		ptyPayload := ssh.Marshal(ptyRequestPayload{
			Term:    "xterm-256color",
			Columns: clampSSHDimension(w),
			Rows:    clampSSHDimension(h),
		})
		ok, err := channel.SendRequest("pty-req", true, ptyPayload)
		if err != nil {
			return fmt.Errorf("pty-req: %w", err)
		}
		if !ok {
			return fmt.Errorf("agent rejected pty-req")
		}

		go watchWindowResize(channel, fd, done)
	}

	ok, err := channel.SendRequest("shell", true, nil)
	if err != nil {
		return fmt.Errorf("shell request: %w", err)
	}
	if !ok {
		return fmt.Errorf("agent rejected shell request")
	}

	// Proxy I/O between the local terminal and the remote shell.
	go func() { _, _ = io.Copy(channel, os.Stdin) }()
	go func() { _, _ = io.Copy(os.Stderr, channel.Stderr()) }()

	// Blocks until the shell exits (channel closed by agent) or the connection drops.
	_, copyErr := io.Copy(os.Stdout, channel)

	// Wait for the reqs goroutine to drain. The agent sends exit-status before
	// closing the channel, so by the time reqs is closed, exit-status has been
	// processed and cleanExit will be set if this was a clean shell exit.
	<-reqsDone

	// If the agent sent exit-status before closing, it was a clean shell exit — no reconnect.
	select {
	case <-cleanExit:
		return nil
	default:
	}

	// No exit-status received: connection was dropped.
	if copyErr == nil || strings.Contains(strings.ToLower(copyErr.Error()), "eof") {
		return fmt.Errorf("connection dropped")
	}
	return copyErr
}

// isTerminalSSHError returns true for errors that will never succeed on retry —
// authentication failures, handshake errors, host key rejections.
func isTerminalSSHError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, phrase := range []string{
		"unable to authenticate",
		"no supported methods remain",
		"handshake failed",
		"host key",
		"permission denied",
	} {
		if strings.Contains(strings.ToLower(msg), phrase) {
			return true
		}
	}
	return false
}
