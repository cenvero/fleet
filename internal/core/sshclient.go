// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/transport"
)

const (
	sshKeepaliveInterval = 30 * time.Second
	sshReconnectDelay    = 5 * time.Second
	sshMaxRetries        = 3
)

// RunSSHSession opens an interactive root shell on the named server.
// If the connection drops it automatically retries up to 3 times before giving up.
func (a *App) RunSSHSession(serverName string, out io.Writer) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	keyPath := filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
	signer, err := crypto.LoadPrivateKeySigner(keyPath, nil)
	if err != nil {
		return fmt.Errorf("load private key: %w", err)
	}

	knownHostsPath := a.Config.Crypto.KnownHostsPath
	promptFn := func(hostname, oldFP, newFP string) bool {
		fmt.Fprintf(out, "\n⚠  WARNING: host key for %s has changed!\n", hostname)
		fmt.Fprintf(out, "   Old fingerprint: %s\n", oldFP)
		fmt.Fprintf(out, "   New fingerprint: %s\n", newFP)
		fmt.Fprintf(out, "\nThis could indicate a server rebuild or a man-in-the-middle attack.\n")
		fmt.Fprintf(out, "Accept the new key? Type 'yes' to continue: ")
		reader := bufio.NewReader(os.Stdin)
		resp := transport.ReadLine(reader)
		return strings.ToLower(resp) == "yes"
	}

	var state transport.HostKeyState
	hostKeyCallback, err := transport.NewInteractiveHostKeyCallback(knownHostsPath, promptFn, &state)
	if err != nil {
		return fmt.Errorf("known_hosts: %w", err)
	}

	user := server.User
	if user == "" {
		user = "root"
	}
	addr := fmt.Sprintf("%s:%d", server.Address, server.Port)

	clientConfig := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}

	for attempt := 0; attempt <= sshMaxRetries; attempt++ {
		if attempt > 0 {
			fmt.Fprintf(out, "\nReconnecting... (attempt %d/%d)\n", attempt, sshMaxRetries)
			time.Sleep(sshReconnectDelay)
		}

		err = a.runSSHOnce(addr, clientConfig, &state, out)
		if err == nil || isCleanSSHExit(err) {
			return nil
		}

		if attempt < sshMaxRetries {
			fmt.Fprintf(out, "\nConnection lost. Retrying in 5s...\n")
		}
	}

	return fmt.Errorf("disconnected — could not reconnect after %d attempts", sshMaxRetries)
}

func (a *App) runSSHOnce(addr string, cfg *ssh.ClientConfig, state *transport.HostKeyState, out io.Writer) error {
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	if state.Outcome == "pinned" {
		fmt.Fprintf(out, "Fingerprint: %s (saved to fleet known hosts)\n", state.Fingerprint)
	}

	// Keepalive: send ping every 30s; close connection if server stops responding
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
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					client.Close()
					return
				}
			}
		}
	}()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("make raw terminal: %w", err)
		}
		defer term.Restore(fd, oldState) //nolint:errcheck

		w, h, _ := term.GetSize(fd)
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		if err := session.RequestPty("xterm-256color", h, w, modes); err != nil {
			return fmt.Errorf("request pty: %w", err)
		}

		go watchWindowResize(session, fd, done)
	}

	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	if err := session.Shell(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	return session.Wait()
}

// isCleanSSHExit returns true when the remote shell exited on its own (user typed
// exit/logout or a command returned non-zero) — these are not reconnect-worthy.
func isCleanSSHExit(err error) bool {
	var exitErr *ssh.ExitError
	return errors.As(err, &exitErr)
}

func watchWindowResize(session *ssh.Session, fd int, done <-chan struct{}) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-done:
			return
		case <-sigCh:
			w, h, err := term.GetSize(fd)
			if err == nil {
				_ = session.WindowChange(h, w)
			}
		}
	}
}
