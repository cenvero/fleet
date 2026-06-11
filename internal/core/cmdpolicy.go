// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CmdPolicyStore holds the dangerous-command policy: a list of deny patterns
// (commands matching these are blocked outright) and a list of confirm patterns
// (commands matching these require explicit operator confirmation). It is
// persisted as a single JSON document at <configDir>/cmd-policy.json (0600).
//
// This is intentionally a SEPARATE file from policy.json (used by the
// output-redaction RedactStore) so the two policies never clobber each other.
//
// Patterns use simple substring + glob matching (see matchPattern): a pattern
// with no '*' or '?' metacharacters matches when it appears anywhere in the
// command (substring); a pattern containing '*'/'?' is treated as an UNANCHORED
// glob that matches when it occurs anywhere in the command (substring-glob),
// consistent with the plain-substring behaviour. This matters for a DENY list:
// a dangerous fragment must block the command wherever it appears, not only when
// it spans the whole command line. Matching is case-sensitive. Examples:
//
//	rm -rf /        substring: blocks any command containing "rm -rf /"
//	mkfs            substring: blocks any command containing "mkfs"
//	rm -rf /*       glob: blocks "cd /tmp && rm -rf /etc", "rm -rf /var", etc.
//	dd of=/dev/sd*  glob: blocks "dd of=/dev/sda", "sudo dd of=/dev/sdb1 bs=1M", etc.
//
// The store is a read/modify/write store opened from a config dir and kept off
// *App so it does not require touching app.go. The main loop wires MatchDeny
// (block) and MatchConfirm (require --confirm) into the exec path.
type CmdPolicyStore struct {
	path string

	mu      sync.RWMutex
	deny    []string
	confirm []string
}

// cmdPolicyDocument is the on-disk JSON shape.
type cmdPolicyDocument struct {
	DenyPatterns    []string `json:"deny_patterns"`
	ConfirmPatterns []string `json:"confirm_patterns"`
}

// NewCmdPolicyStore opens and loads a command-policy store rooted at configDir.
// If configDir is empty the default config dir is used. A missing or empty
// policy file yields an empty store (no error). A corrupt file yields an error
// so callers don't silently run with no command policy.
func NewCmdPolicyStore(configDir string) (*CmdPolicyStore, error) {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	s := &CmdPolicyStore{path: CmdPolicyPath(configDir)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// CmdPolicyPath returns the on-disk location of the command-policy document for
// a config dir. It is deliberately distinct from PolicyPath (policy.json).
func CmdPolicyPath(configDir string) string {
	return filepath.Join(configDir, "cmd-policy.json")
}

// load reads the policy document, replacing in-memory state.
func (s *CmdPolicyStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.deny = nil
			s.confirm = nil
			s.mu.Unlock()
			return nil
		}
		return fmt.Errorf("read cmd policy: %w", err)
	}
	doc := cmdPolicyDocument{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("decode cmd policy: %w", err)
		}
	}
	s.mu.Lock()
	s.deny = cleanPatterns(doc.DenyPatterns)
	s.confirm = cleanPatterns(doc.ConfirmPatterns)
	s.mu.Unlock()
	return nil
}

// cleanPatterns trims whitespace and drops empty entries, preserving order.
func cleanPatterns(patterns []string) []string {
	cleaned := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if t := trimSpace(p); t != "" {
			cleaned = append(cleaned, t)
		}
	}
	return cleaned
}

// trimSpace is a tiny local helper so this file does not pull in strings just
// for TrimSpace; it trims ASCII spaces and tabs from both ends.
func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// SetDenyPatterns replaces the deny patterns and persists. Empty entries are
// dropped. Passing nil or an all-empty slice clears the deny list.
func (s *CmdPolicyStore) SetDenyPatterns(patterns []string) error {
	cleaned := cleanPatterns(patterns)
	s.mu.Lock()
	s.deny = cleaned
	confirm := append([]string(nil), s.confirm...)
	s.mu.Unlock()
	return s.persist(cleaned, confirm)
}

// SetConfirmPatterns replaces the confirm patterns and persists. Empty entries
// are dropped. Passing nil or an all-empty slice clears the confirm list.
func (s *CmdPolicyStore) SetConfirmPatterns(patterns []string) error {
	cleaned := cleanPatterns(patterns)
	s.mu.Lock()
	confirm := cleaned
	s.confirm = cleaned
	deny := append([]string(nil), s.deny...)
	s.mu.Unlock()
	return s.persist(deny, confirm)
}

// DenyPatterns returns a copy of the configured deny patterns.
func (s *CmdPolicyStore) DenyPatterns() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.deny...)
}

// ConfirmPatterns returns a copy of the configured confirm patterns.
func (s *CmdPolicyStore) ConfirmPatterns() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.confirm...)
}

// MatchDeny reports whether the command matches any deny pattern, returning the
// first matching pattern. The main loop blocks execution when this returns true.
func (s *CmdPolicyStore) MatchDeny(command string) (bool, string) {
	s.mu.RLock()
	patterns := s.deny
	s.mu.RUnlock()
	return matchAny(command, patterns)
}

// MatchConfirm reports whether the command matches any confirm pattern,
// returning the first matching pattern. The main loop requires --confirm when
// this returns true (and the command is not already denied).
func (s *CmdPolicyStore) MatchConfirm(command string) (bool, string) {
	s.mu.RLock()
	patterns := s.confirm
	s.mu.RUnlock()
	return matchAny(command, patterns)
}

// matchAny returns the first pattern in patterns that matches command.
func matchAny(command string, patterns []string) (bool, string) {
	for _, p := range patterns {
		if matchPattern(command, p) {
			return true, p
		}
	}
	return false, ""
}

// matchPattern matches command against a single pattern. A pattern containing
// any glob metacharacter ('*' or '?') is treated as an UNANCHORED glob: it
// matches when it occurs anywhere in command (substring-glob), the same as a
// plain pattern is a substring. Otherwise it is a plain substring match (the
// pattern appears anywhere in command). Matching is case-sensitive.
//
// Anchoring a deny glob to the whole command would fail open: a deny pattern
// like "rm -rf /*" would not block "cd /tmp && rm -rf /etc" because the whole
// command does not match the glob. Substring-glob keeps the deny list reliable.
func matchPattern(command, pattern string) bool {
	if pattern == "" {
		return false
	}
	if hasGlobMeta(pattern) {
		return globMatchSubstring(pattern, command)
	}
	return containsSubstring(command, pattern)
}

// hasGlobMeta reports whether pattern uses glob metacharacters.
func hasGlobMeta(pattern string) bool {
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '*' || pattern[i] == '?' {
			return true
		}
	}
	return false
}

// containsSubstring reports whether sub appears anywhere in s.
func containsSubstring(s, sub string) bool {
	if sub == "" {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// globMatchSubstring reports whether pattern matches any substring of name —
// i.e. an UNANCHORED glob. It is equivalent to anchoring pattern with an implicit
// '*' on both ends, so a deny/confirm glob fires wherever its dangerous fragment
// appears in the command (not only when it spans the whole command line).
//
// It works by anchoring the pattern's leading and trailing '*' semantics: it
// tries the whole-string globMatch with '*' wrapped around the pattern, which it
// achieves by attempting a match at every start offset of name. Because '*' may
// match the empty string, an empty name is handled by trying offset 0.
func globMatchSubstring(pattern, name string) bool {
	// A pattern made only of '*' (and matching anything) still needs one attempt
	// even when name is empty, so iterate start from 0 to len(name) inclusive.
	for start := 0; start <= len(name); start++ {
		if globMatchPrefix(pattern, name[start:]) {
			return true
		}
	}
	return false
}

// globMatchPrefix reports whether pattern matches a PREFIX of name (the pattern
// is anchored at the start of name but a trailing '*' is implied — it need not
// consume the rest of name). Combined with trying every start offset in
// globMatchSubstring, this yields an unanchored substring glob.
func globMatchPrefix(pattern, name string) bool {
	var (
		px, nx           int
		starPx, starNx   = -1, -1
		hasStarBacktrack bool
	)
	for px < len(pattern) {
		if px < len(pattern) {
			switch pattern[px] {
			case '*':
				hasStarBacktrack = true
				starPx, starNx = px, nx
				px++
				continue
			case '?':
				if nx < len(name) {
					px++
					nx++
					continue
				}
			default:
				if nx < len(name) && pattern[px] == name[nx] {
					px++
					nx++
					continue
				}
			}
		}
		if hasStarBacktrack && starNx < len(name) {
			px = starPx + 1
			starNx++
			nx = starNx
			continue
		}
		return false
	}
	// All of pattern consumed -> it matched a prefix of name (trailing chars in
	// name are allowed; that is the "implicit trailing '*'" of a substring match).
	return true
}

// globMatch reports whether pattern matches the whole of name, where '*' matches
// any run of characters (including none) and '?' matches exactly one character.
// It uses an iterative backtracking algorithm (no regexp, no allocation).
func globMatch(pattern, name string) bool {
	var (
		px, nx           int
		starPx, starNx   = -1, -1
		hasStarBacktrack bool
	)
	for nx < len(name) {
		if px < len(pattern) {
			switch pattern[px] {
			case '*':
				// Record a backtrack point: try to match '*' against as little
				// as possible first, expanding on failure.
				hasStarBacktrack = true
				starPx, starNx = px, nx
				px++
				continue
			case '?':
				px++
				nx++
				continue
			default:
				if pattern[px] == name[nx] {
					px++
					nx++
					continue
				}
			}
		}
		// Mismatch: backtrack to the last '*' if one exists and let it consume
		// one more character of name.
		if hasStarBacktrack {
			px = starPx + 1
			starNx++
			nx = starNx
			continue
		}
		return false
	}
	// Consume any trailing '*' in the pattern.
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// persist writes the policy document atomically (0600), creating the config
// dir if needed.
func (s *CmdPolicyStore) persist(deny, confirm []string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	doc := cmdPolicyDocument{DenyPatterns: deny, ConfirmPatterns: confirm}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cmd policy: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".cmd-policy-*.json")
	if err != nil {
		return fmt.Errorf("write cmd policy: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write cmd policy: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write cmd policy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write cmd policy: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write cmd policy: %w", err)
	}
	return nil
}
