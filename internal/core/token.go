// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// TokenStore is a standalone JSON store of scoped RBAC tokens, persisted as a
// single document at <configDir>/tokens.json (0600). Like TagStore it is a
// read/modify/write store opened from a config dir and kept off *App so it does
// not require touching app.go.
//
// FL-030: this powers CONTROLLER-side enforcement only. A scoped token limits
// which commands and servers a controller invocation may touch. Agent-side
// enforcement (the agent validates the presented token per-RPC) is a future
// hardening (FL-030 server-side) and is intentionally out of scope here.
type TokenStore struct {
	path string
	mu   sync.Mutex
}

// Token is a scoped credential. The ID is the bearer secret: anyone holding it
// can act within the token's scope, so it is generated from crypto/rand and is
// shown to the operator exactly once at creation time.
type Token struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Servers            []string  `json:"servers,omitempty"`
	Groups             []string  `json:"groups,omitempty"`
	AllowCommands      []string  `json:"allow_commands,omitempty"`
	DenyCommands       []string  `json:"deny_commands,omitempty"`
	ReadOnlyDefault    bool      `json:"read_only_default,omitempty"`
	DestructiveAllowed bool      `json:"destructive_allowed,omitempty"`
	Created            time.Time `json:"created"`
}

// tokensDocument is the on-disk JSON shape: token ID -> token.
type tokensDocument struct {
	Tokens map[string]Token `json:"tokens"`
}

// NewTokenStore opens (without reading) a token store rooted at configDir. If
// configDir is empty the default config dir is used.
func NewTokenStore(configDir string) *TokenStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	return &TokenStore{path: TokensPath(configDir)}
}

// TokensPath returns the on-disk location of the tokens document for a config dir.
func TokensPath(configDir string) string {
	return filepath.Join(configDir, "tokens.json")
}

func (s *TokenStore) read() (tokensDocument, error) {
	doc := tokensDocument{Tokens: map[string]Token{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, fmt.Errorf("read tokens: %w", err)
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("decode tokens: %w", err)
	}
	if doc.Tokens == nil {
		doc.Tokens = map[string]Token{}
	}
	return doc, nil
}

func (s *TokenStore) write(doc tokensDocument) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tokens: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".tokens-*.json")
	if err != nil {
		return fmt.Errorf("write tokens: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tokens: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tokens: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write tokens: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write tokens: %w", err)
	}
	return nil
}

// newTokenID returns a 32-hex-char (16 random byte) credential.
func newTokenID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Create persists a new token. ID and Created are filled in by the store; any
// caller-supplied values for those fields are ignored. The stored token (with
// its generated ID) is returned so the caller can show the secret once.
func (s *TokenStore) Create(t Token) (Token, error) {
	if strings.TrimSpace(t.Name) == "" {
		return Token{}, fmt.Errorf("token name is required")
	}
	id, err := newTokenID()
	if err != nil {
		return Token{}, err
	}
	t.ID = id
	t.Created = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return Token{}, err
	}
	if _, exists := doc.Tokens[t.ID]; exists {
		// Astronomically unlikely with 128 bits of entropy, but be explicit.
		return Token{}, fmt.Errorf("token id collision; please retry")
	}
	doc.Tokens[t.ID] = t
	if err := s.write(doc); err != nil {
		return Token{}, err
	}
	return t, nil
}

// List returns all tokens sorted by creation time (oldest first).
func (s *TokenStore) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(doc.Tokens))
	for _, t := range doc.Tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created.Equal(out[j].Created) {
			return out[i].ID < out[j].ID
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out, nil
}

// Get returns the token with the given ID, or an error if it does not exist
// (i.e. unknown or revoked).
func (s *TokenStore) Get(id string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return Token{}, err
	}
	t, ok := doc.Tokens[id]
	if !ok {
		return Token{}, fmt.Errorf("token not found")
	}
	return t, nil
}

// Revoke deletes the token with the given ID. Revoking an unknown token is an
// error so the operator gets clear feedback.
func (s *TokenStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := doc.Tokens[id]; !ok {
		return fmt.Errorf("token not found")
	}
	delete(doc.Tokens, id)
	return s.write(doc)
}

// readCommands is the set of top-level command names that only read controller
// or fleet state and never mutate a managed server. Used by Authorize when a
// token sets ReadOnlyDefault: anything outside this set is denied unless it is
// explicitly listed in AllowCommands.
var readCommands = map[string]bool{
	"status":    true,
	"list":      true,
	"show":      true,
	"inventory": true,
	"logs":      true,
	"journal":   true,
	"health":    true,
	"top":       true,
	"alerts":    true,
	"drift":     true,
	"config":    true,
	"tag":       true,
	"port":      true, // `port list` is read; `port open/close` is gated by deny/allow + scope
	"version":   true,
	"report":    true,
}

// IsReadCommand reports whether a top-level command is in the read-only set.
func IsReadCommand(topCommand string) bool {
	return readCommands[topCommand]
}

// destructiveCommands documents the (topCommand -> subcommands) pairs that are
// treated as destructive for FL-030. A token without DestructiveAllowed cannot
// run any of these. The list is intentionally conservative and explicit:
//
//	server   remove                      tears down/forgets a managed server
//	file     rm                          deletes a remote file/dir
//	key      rotate                      rotates controller keys + rolls them out
//	firewall enable                      changes live firewall state (lock-out risk)
//	fw       enable                      alias of firewall enable (if present)
//	guard    (any)                       arms/triggers the dead-man guard
//	revert   (any)                       reverts servers to a prior state
//
// Notes:
//   - drift is read-only (it reports divergence; it does not change anything),
//     so it is NOT destructive.
//   - exec cannot be classified from here (we can't see what the remote command
//     does), so exec is treated as NON-destructive by default; operators scope
//     it via DenyCommands/AllowCommands and the server set instead.
var destructiveCommands = map[string]map[string]bool{
	"server":   {"remove": true},
	"file":     {"rm": true},
	"key":      {"rotate": true},
	"firewall": {"enable": true},
	"fw":       {"enable": true},
	"guard":    {"*": true},
	"revert":   {"*": true},
}

// IsDestructiveCommand reports whether the given top-level command (and its
// args, where the first arg is usually the subcommand) is destructive per the
// documented destructiveCommands table.
func IsDestructiveCommand(topCommand string, args []string) bool {
	subs, ok := destructiveCommands[topCommand]
	if !ok {
		return false
	}
	if subs["*"] {
		return true
	}
	if len(args) > 0 && subs[args[0]] {
		return true
	}
	return false
}

// stringInSlice reports whether want is present in list (case-sensitive).
func stringInSlice(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// resolvedServers expands a token's explicit Servers plus any Groups (tag
// filter expressions resolved against allServerNames via the TagStore) into a
// de-duplicated set of server names the token may target.
func (t Token) resolvedServers(allServerNames []string, tags *TagStore) (map[string]bool, error) {
	set := map[string]bool{}
	for _, s := range t.Servers {
		s = strings.TrimSpace(s)
		if s != "" {
			set[s] = true
		}
	}
	if len(t.Groups) > 0 {
		if tags == nil {
			return nil, fmt.Errorf("token references groups but no tag store is available")
		}
		for _, expr := range t.Groups {
			expr = strings.TrimSpace(expr)
			if expr == "" {
				continue
			}
			matched, err := tags.ServersMatching(expr, allServerNames)
			if err != nil {
				return nil, fmt.Errorf("resolve group %q: %w", expr, err)
			}
			for _, name := range matched {
				set[name] = true
			}
		}
	}
	return set, nil
}

// scopesAnyServer reports whether the token restricts the set of servers it can
// touch (either explicit servers or groups). When false, the token is not
// server-scoped and may target any server (subject to the command checks).
func (t Token) scopesAnyServer() bool {
	return len(t.Servers) > 0 || len(t.Groups) > 0
}

// Authorize enforces a scoped token against a single controller invocation. It
// returns a clear "denied: ..." error when the invocation is out of scope, or
// nil when allowed. Checks, in order:
//
//  1. command allow-list: when AllowCommands is non-empty, topCommand must be in it;
//  2. command deny-list: topCommand must not be in DenyCommands;
//  3. read-only default: when ReadOnlyDefault, a non-read command is denied
//     unless explicitly allowed via AllowCommands;
//  4. destructive: when isDestructive and not DestructiveAllowed, deny;
//  5. server scope: when the token is server-scoped and a server is targeted,
//     targetServer must be in the token's resolved server set (Servers + Groups).
//
// targetServer may be empty for commands that don't target a specific server;
// the server-scope check is skipped in that case.
func Authorize(t Token, topCommand, targetServer string, isDestructive bool, allServerNames []string, tags *TagStore) error {
	topCommand = strings.TrimSpace(topCommand)

	// 2. Explicit deny always wins, even over an allow-list entry.
	if stringInSlice(t.DenyCommands, topCommand) {
		return fmt.Errorf("denied: command %q is in this token's deny list", topCommand)
	}

	// 1. Allow-list: when set, the command must be present.
	explicitlyAllowed := stringInSlice(t.AllowCommands, topCommand)
	if len(t.AllowCommands) > 0 && !explicitlyAllowed {
		return fmt.Errorf("denied: command %q is not in this token's allow list", topCommand)
	}

	// 3. Read-only default: deny non-read commands unless explicitly allowed.
	if t.ReadOnlyDefault && !explicitlyAllowed && !IsReadCommand(topCommand) {
		return fmt.Errorf("denied: token is read-only by default; command %q is not a read command and is not explicitly allowed", topCommand)
	}

	// 4. Destructive operations require DestructiveAllowed.
	if isDestructive && !t.DestructiveAllowed {
		return fmt.Errorf("denied: command %q is destructive and this token does not allow destructive operations", topCommand)
	}

	// 5. Server scope: only enforced when the token is server-scoped AND the
	// invocation targets a specific server.
	if t.scopesAnyServer() && strings.TrimSpace(targetServer) != "" {
		set, err := t.resolvedServers(allServerNames, tags)
		if err != nil {
			return fmt.Errorf("denied: %w", err)
		}
		if !set[targetServer] {
			return fmt.Errorf("denied: server %q is not in this token's scope", targetServer)
		}
	}

	return nil
}
