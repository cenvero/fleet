// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// execEnvironment returns the agent's environment plus the payload's extra
// variables (later entries win, so these override inherited ones). Names must
// be [A-Za-z_][A-Za-z0-9_]*; values may not contain NUL. Errors name the
// variable only — values are secrets and never appear in an error.
func execEnvironment(extra map[string]string) ([]string, error) {
	names := make([]string, 0, len(extra))
	for name := range extra {
		if !validExecEnvName(name) {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		if strings.ContainsRune(extra[name], 0) {
			return nil, fmt.Errorf("environment variable %q contains a NUL byte", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	env := os.Environ()
	for _, name := range names {
		env = append(env, name+"="+extra[name])
	}
	return env, nil
}

func validExecEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
