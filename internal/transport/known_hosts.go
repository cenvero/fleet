// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package transport

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type HostKeyState struct {
	Fingerprint string
	StoredAs    string
	Outcome     string
}

func NewTOFUHostKeyCallback(path string, acceptReplacement bool, state *HostKeyState) (ssh.HostKeyCallback, error) {
	if err := ensureKnownHostsFile(path); err != nil {
		return nil, err
	}

	base, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", path, err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		normalized := knownhosts.Normalize(remote.String())
		if state != nil {
			state.Fingerprint = ssh.FingerprintSHA256(key)
			state.StoredAs = normalized
		}

		err := base(hostname, remote, key)
		if err == nil {
			if state != nil {
				state.Outcome = "known"
			}
			return nil
		}

		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}

		if len(keyErr.Want) == 0 {
			if err := appendKnownHost(path, remote.String(), key); err != nil {
				return err
			}
			if state != nil {
				state.Outcome = "pinned"
			}
			return nil
		}

		if !acceptReplacement {
			return fmt.Errorf("host key mismatch for %s: %w", normalized, err)
		}
		if err := replaceKnownHost(path, remote.String(), key); err != nil {
			return err
		}
		if state != nil {
			state.Outcome = "replaced"
		}
		return nil
	}, nil
}

// NewInteractiveHostKeyCallback creates a host key callback that:
//   - Accepts and pins new hosts (TOFU)
//   - Shows the fingerprint when pinning a new host
//   - Prompts via promptFn when an existing host key changes; returns error if rejected
func NewInteractiveHostKeyCallback(path string, promptFn func(hostname, oldFP, newFP string) bool, state *HostKeyState) (ssh.HostKeyCallback, error) {
	if err := ensureKnownHostsFile(path); err != nil {
		return nil, err
	}
	base, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", path, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		normalized := knownhosts.Normalize(remote.String())
		fp := ssh.FingerprintSHA256(key)
		if state != nil {
			state.Fingerprint = fp
			state.StoredAs = normalized
		}
		err := base(hostname, remote, key)
		if err == nil {
			if state != nil {
				state.Outcome = "known"
			}
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}
		if len(keyErr.Want) == 0 {
			// New host — TOFU pin
			if addErr := appendKnownHost(path, remote.String(), key); addErr != nil {
				return addErr
			}
			if state != nil {
				state.Outcome = "pinned"
			}
			return nil
		}
		// Host key changed
		oldFP := ssh.FingerprintSHA256(keyErr.Want[0].Key)
		if promptFn == nil || !promptFn(normalized, oldFP, fp) {
			return fmt.Errorf("host key for %s has changed and was rejected", normalized)
		}
		if replErr := replaceKnownHost(path, remote.String(), key); replErr != nil {
			return replErr
		}
		if state != nil {
			state.Outcome = "replaced"
		}
		return nil
	}, nil
}

// ReadLine reads a single line from a bufio.Reader, used for interactive prompts.
func ReadLine(r *bufio.Reader) string {
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func ensureKnownHostsFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create known_hosts directory: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("create known_hosts file: %w", err)
	}
	return nil
}

func appendKnownHost(path, address string, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open known_hosts for append: %w", err)
	}
	defer f.Close()
	line := knownhosts.Line([]string{address}, key)
	if _, err := fmt.Fprintln(f, line); err != nil {
		return fmt.Errorf("append known_hosts entry: %w", err)
	}
	return nil
}

func replaceKnownHost(path, address string, key ssh.PublicKey) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read known_hosts for replacement: %w", err)
	}

	normalized := knownhosts.Normalize(address)
	lines := strings.Split(string(data), "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			if trimmed != "" {
				filtered = append(filtered, line)
			}
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 || fields[0] == normalized {
			continue
		}
		filtered = append(filtered, line)
	}
	filtered = append(filtered, knownhosts.Line([]string{address}, key))
	content := strings.Join(filtered, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("rewrite known_hosts: %w", err)
	}
	return nil
}
