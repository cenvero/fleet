// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/cenvero/fleet/pkg/proto"
)

type AuthorizedKeysManager interface {
	Update(context.Context, proto.AuthorizedKeysPayload) (proto.AuthorizedKeysResult, error)
}

type fileAuthorizedKeysManager struct {
	path string
}

func (s Server) authorizedKeysManager() AuthorizedKeysManager {
	if s.AuthorizedKeysMgr != nil {
		return s.AuthorizedKeysMgr
	}
	return fileAuthorizedKeysManager{path: s.AuthorizedKeysPath}
}

func (m fileAuthorizedKeysManager) Update(_ context.Context, payload proto.AuthorizedKeysPayload) (proto.AuthorizedKeysResult, error) {
	if strings.TrimSpace(m.path) == "" {
		return proto.AuthorizedKeysResult{}, &RPCError{
			Code:    "missing_authorized_keys_path",
			Message: "authorized keys path is required",
		}
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return proto.AuthorizedKeysResult{}, &RPCError{
			Code:    "authorized_keys_directory_failed",
			Message: err.Error(),
		}
	}

	lines, mode, err := readAuthorizedKeyLines(m.path)
	if err != nil {
		return proto.AuthorizedKeysResult{}, err
	}

	removeSet := make(map[string]struct{}, len(payload.RemoveKeys))
	for _, key := range payload.RemoveKeys {
		canonical, err := canonicalAuthorizedKey(key)
		if err != nil {
			return proto.AuthorizedKeysResult{}, err
		}
		removeSet[canonical] = struct{}{}
	}

	filtered := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines)+len(payload.AddKeys))
	for _, line := range lines {
		if _, remove := removeSet[line]; remove {
			continue
		}
		if _, duplicate := seen[line]; duplicate {
			continue
		}
		seen[line] = struct{}{}
		filtered = append(filtered, line)
	}

	for _, key := range payload.AddKeys {
		canonical, err := canonicalAuthorizedKey(key)
		if err != nil {
			return proto.AuthorizedKeysResult{}, err
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		filtered = append(filtered, canonical)
	}

	if len(filtered) == 0 {
		return proto.AuthorizedKeysResult{}, &RPCError{
			Code:    "invalid_authorized_keys",
			Message: "authorized keys cannot be empty",
		}
	}

	content := strings.Join(filtered, "\n") + "\n"
	if mode == 0 {
		mode = 0o600
	}
	if err := os.WriteFile(m.path, []byte(content), mode); err != nil {
		return proto.AuthorizedKeysResult{}, &RPCError{
			Code:    "authorized_keys_write_failed",
			Message: err.Error(),
		}
	}
	return proto.AuthorizedKeysResult{Keys: filtered}, nil
}

func readAuthorizedKeyLines(path string) ([]string, os.FileMode, error) {
	info, err := os.Stat(path)
	mode := os.FileMode(0)
	if err == nil {
		mode = info.Mode().Perm()
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return nil, mode, nil
	default:
		return nil, mode, &RPCError{
			Code:    "authorized_keys_read_failed",
			Message: err.Error(),
		}
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lines := make([]string, 0, 8)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		canonical, err := canonicalAuthorizedKey(line)
		if err != nil {
			return nil, mode, err
		}
		lines = append(lines, canonical)
	}
	if err := scanner.Err(); err != nil {
		return nil, mode, &RPCError{
			Code:    "authorized_keys_read_failed",
			Message: err.Error(),
		}
	}
	return lines, mode, nil
}

func canonicalAuthorizedKey(line string) (string, error) {
	_, canonical, err := parseAuthorizedKeyLine(line)
	return canonical, err
}
