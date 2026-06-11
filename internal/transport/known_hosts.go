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
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// pinMu serializes the read-check-then-append/replace sequence used when
// pinning host keys. The knownhosts callback re-reads the file from disk, so
// concurrent first-connections to the same (or different) hosts can otherwise
// interleave their check-then-write and double-pin or corrupt the file. A
// single process-wide mutex is sufficient: known_hosts writes are infrequent
// and cheap, and they all funnel through this package.
var pinMu sync.Mutex

type HostKeyState struct {
	Fingerprint string
	StoredAs    string
	Outcome     string
}

// NewTOFUHostKeyCallback returns a host-key callback that pins a host's key on
// first use (TOFU) and thereafter verifies it against the pinned value.
//
// SECURITY: a host-key MISMATCH (a pinned key exists but the presented key
// differs) is the exact signal host-key pinning is meant to catch — it means
// either the server's key legitimately rotated or an attacker is performing a
// man-in-the-middle attack. By default this callback REJECTS the connection on
// mismatch; it never silently overwrites a pinned key. Re-trusting a changed
// key is a deliberate operator action: either delete the pin (RemoveKnownHost
// / `fleet server reconnect --accept-new-host-key`) after out-of-band
// verification, or pass forceReplace=true.
//
// forceReplace must ONLY be set true on an explicit, operator-initiated re-pin
// after the new fingerprint has been verified out of band. It must NEVER be
// wired to a default-on flag or to any automated/unattended path, because that
// reintroduces the silent-replace MITM hole. First-use pinning (no existing
// pin) always happens regardless of forceReplace.
func NewTOFUHostKeyCallback(path string, forceReplace bool, state *HostKeyState) (ssh.HostKeyCallback, error) {
	if err := ensureKnownHostsFile(path); err != nil {
		return nil, err
	}

	base, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", path, err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		// knownhosts.check() looks up by hostname (the address argument to
		// ssh.Dial), not by the remote IP. Store and replace using the same
		// hostname-based key so lookups always match what we wrote.
		normalized := knownhosts.Normalize(hostname)
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
			if err := appendKnownHost(path, normalized, key); err != nil {
				return err
			}
			if state != nil {
				state.Outcome = "pinned"
			}
			return nil
		}

		// A pin exists and the presented key does not match it: possible MITM.
		// Refuse by default and tell the operator exactly how to recover.
		if !forceReplace {
			oldFP := ssh.FingerprintSHA256(keyErr.Want[0].Key)
			newFP := ssh.FingerprintSHA256(key)
			if state != nil {
				state.Outcome = "rejected"
			}
			return fmt.Errorf(
				"host key changed for %s: possible MITM; pinned %s but server presented %s. "+
					"Remove the pin to re-trust (e.g. `fleet server reconnect --accept-new-host-key` "+
					"after verifying the new key out of band)",
				normalized, oldFP, newFP)
		}
		// Explicit, operator-authorized re-pin after out-of-band verification.
		if err := replaceKnownHost(path, normalized, key); err != nil {
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
		normalized := knownhosts.Normalize(hostname)
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
			if addErr := appendKnownHost(path, normalized, key); addErr != nil {
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
		if replErr := replaceKnownHost(path, normalized, key); replErr != nil {
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

// RemoveKnownHost removes all known_hosts entries for the given address.
// address should be in "host:port" form; it is normalized before matching.
// No-ops silently if the file does not exist or the address isn't present.
func RemoveKnownHost(path, address string) error {
	normalized := knownhosts.Normalize(address)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read known_hosts: %w", err)
	}
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
		if len(fields) > 0 && fields[0] == normalized {
			continue // drop this entry
		}
		filtered = append(filtered, line)
	}
	content := strings.Join(filtered, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func appendKnownHost(path, address string, key ssh.PublicKey) error {
	// Serialize the read-check-then-append so two concurrent first-connections
	// can't both observe "no entry" and append duplicate (or racing) pins.
	pinMu.Lock()
	defer pinMu.Unlock()
	// Skip if an entry for this address already exists — prevents duplicates
	// even if the in-memory callback somehow misses a recently written entry.
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == address {
				return nil
			}
		}
	}
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
	// Shares pinMu with appendKnownHost so a replace can't interleave with a
	// concurrent append (or another replace) and clobber the file mid-rewrite.
	pinMu.Lock()
	defer pinMu.Unlock()
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
	filtered = append(filtered, knownhosts.Line([]string{normalized}, key))
	content := strings.Join(filtered, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("rewrite known_hosts: %w", err)
	}
	return nil
}
