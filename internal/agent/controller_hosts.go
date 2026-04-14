// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type ControllerKnownHostsManager interface {
	Update(context.Context, proto.ControllerKnownHostsPayload) (proto.ControllerKnownHostsResult, error)
}

type fileControllerKnownHostsManager struct {
	path    string
	address string
}

func (s Server) controllerKnownHostsManager() ControllerKnownHostsManager {
	if s.ControllerKnownHostsMgr != nil {
		return s.ControllerKnownHostsMgr
	}
	return fileControllerKnownHostsManager{
		path:    s.ControllerKnownHostsPath,
		address: s.ControllerAddress,
	}
}

func (m fileControllerKnownHostsManager) Update(_ context.Context, payload proto.ControllerKnownHostsPayload) (proto.ControllerKnownHostsResult, error) {
	if strings.TrimSpace(m.path) == "" {
		return proto.ControllerKnownHostsResult{}, &RPCError{
			Code:    "missing_known_hosts_path",
			Message: "controller known_hosts path is required",
		}
	}
	address := strings.TrimSpace(payload.Address)
	if address == "" {
		address = strings.TrimSpace(m.address)
	}
	if address == "" {
		return proto.ControllerKnownHostsResult{}, &RPCError{
			Code:    "missing_controller_address",
			Message: "controller address is required",
		}
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return proto.ControllerKnownHostsResult{}, &RPCError{
			Code:    "known_hosts_directory_failed",
			Message: err.Error(),
		}
	}

	normalizedAddress := knownhosts.Normalize(address)
	entries, passthrough, mode, err := readKnownHostEntries(m.path, normalizedAddress)
	if err != nil {
		return proto.ControllerKnownHostsResult{}, err
	}

	removeSet := make(map[string]struct{}, len(payload.RemoveKeys))
	for _, key := range payload.RemoveKeys {
		canonical, err := canonicalAuthorizedKey(key)
		if err != nil {
			return proto.ControllerKnownHostsResult{}, err
		}
		removeSet[canonical] = struct{}{}
	}

	filtered := make([]knownHostEntry, 0, len(entries)+len(payload.AddKeys))
	seen := make(map[string]struct{}, len(entries)+len(payload.AddKeys))
	for _, entry := range entries {
		if _, remove := removeSet[entry.Key]; remove {
			continue
		}
		if _, duplicate := seen[entry.Key]; duplicate {
			continue
		}
		seen[entry.Key] = struct{}{}
		filtered = append(filtered, entry)
	}

	for _, key := range payload.AddKeys {
		pub, canonical, err := parseAuthorizedKeyLine(key)
		if err != nil {
			return proto.ControllerKnownHostsResult{}, err
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		filtered = append(filtered, knownHostEntry{
			Address: normalizedAddress,
			Key:     canonical,
			Raw:     strings.TrimSpace(knownhosts.Line([]string{normalizedAddress}, pub)),
		})
	}

	lines := make([]string, 0, len(passthrough)+len(filtered))
	lines = append(lines, passthrough...)
	fingerprints := make([]string, 0, len(filtered))
	for _, entry := range filtered {
		lines = append(lines, entry.Raw)
		pub, _, err := parseKnownHostRawLine(entry.Raw)
		if err == nil {
			fingerprints = append(fingerprints, ssh.FingerprintSHA256(pub))
		}
	}
	slices.Sort(fingerprints)

	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if mode == 0 {
		mode = 0o600
	}
	if err := os.WriteFile(m.path, []byte(content), mode); err != nil {
		return proto.ControllerKnownHostsResult{}, &RPCError{
			Code:    "known_hosts_write_failed",
			Message: err.Error(),
		}
	}

	return proto.ControllerKnownHostsResult{
		Address:      normalizedAddress,
		EntryCount:   len(filtered),
		Fingerprints: fingerprints,
	}, nil
}

type knownHostEntry struct {
	Address string
	Key     string
	Raw     string
}

func readKnownHostEntries(path, targetAddress string) ([]knownHostEntry, []string, os.FileMode, error) {
	info, err := os.Stat(path)
	mode := os.FileMode(0)
	if err == nil {
		mode = info.Mode().Perm()
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return nil, nil, mode, nil
	default:
		return nil, nil, mode, &RPCError{
			Code:    "known_hosts_read_failed",
			Message: err.Error(),
		}
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	entries := make([]knownHostEntry, 0, 4)
	passthrough := make([]string, 0, 4)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			passthrough = append(passthrough, line)
			continue
		}
		pub, canonical, err := parseKnownHostRawLine(line)
		if err != nil {
			passthrough = append(passthrough, line)
			continue
		}
		if !knownHostMatchesAddress(fields[0], targetAddress) {
			passthrough = append(passthrough, line)
			continue
		}
		entries = append(entries, knownHostEntry{
			Address: targetAddress,
			Key:     canonical,
			Raw:     strings.TrimSpace(knownhosts.Line([]string{targetAddress}, pub)),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, mode, &RPCError{
			Code:    "known_hosts_read_failed",
			Message: err.Error(),
		}
	}
	return entries, passthrough, mode, nil
}

func knownHostMatchesAddress(hostsField, targetAddress string) bool {
	for _, host := range strings.Split(hostsField, ",") {
		if knownhosts.Normalize(host) == targetAddress || strings.TrimSpace(host) == targetAddress {
			return true
		}
	}
	return false
}

func parseKnownHostRawLine(line string) (ssh.PublicKey, string, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 3 {
		return nil, "", &RPCError{
			Code:    "invalid_known_hosts_entry",
			Message: "known_hosts entry must include host and public key",
		}
	}
	return parseAuthorizedKeyLine(strings.Join(fields[1:], " "))
}

func parseAuthorizedKeyLine(line string) (ssh.PublicKey, string, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return nil, "", &RPCError{
			Code:    "invalid_authorized_key",
			Message: err.Error(),
		}
	}
	return pub, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), nil
}
