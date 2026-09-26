// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os/user"
	"strings"
)

// IsDestructiveOperation reports whether a CLI command is one of the documented
// destructive operations — the destructiveCommands table plus the key/config/
// tag rules in token.go — and is also classified as mutating by the RBAC
// classifier. It decides when the "destructive" notification event fires.
//
// It is deliberately narrower than the RBAC mutation gate: that gate also covers
// commands like exec, ssh, job run, dashboard or daemon, whose effect cannot be
// judged here or which are not destructive in themselves, and firing for every
// exec would drown subscribers.
func IsDestructiveOperation(top, sub string, args []string) bool {
	top, sub = strings.TrimSpace(top), strings.TrimSpace(sub)
	path := top
	if sub != "" {
		path += " " + sub
	}
	if mutating, known := ClassifyCommandMutation(path, args); known && !mutating {
		return false
	}
	switch top {
	case "key":
		return sub != "" && !keyReadSubs[sub]
	case "config":
		return sub != "" && !configReadSubs[sub]
	case "tag":
		for _, a := range args {
			if strings.Contains(a, "=") {
				return true
			}
		}
		return false
	}
	subs, ok := destructiveCommands[top]
	if !ok {
		return false
	}
	return subs["*"] || (sub != "" && subs[sub])
}

// OperatorName is who an action is attributed to outside an App: the verified
// token's label when one was presented, else the configured operator, else the
// OS user (the same order App.operator uses).
func OperatorName(configDir, acting string) string {
	if acting = strings.TrimSpace(acting); acting != "" {
		return acting
	}
	if cfg, err := LoadConfigShared(ConfigPath(configDir)); err == nil && cfg.Operator != "" {
		return cfg.Operator
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}
