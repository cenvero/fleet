// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"crypto/sha256"
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
//
// SECURITY: the bearer ID is NEVER persisted in cleartext. At rest the store
// keeps only Hash (a SHA-256 of the id) and Prefix (the first 8 chars of the id,
// for `token list` display); a presented --token is hashed and looked up by
// Hash. ID is populated in memory only at creation time (and is empty for tokens
// loaded from disk), so the full secret is shown exactly once.
type Token struct {
	ID                 string    `json:"id,omitempty"`     // bearer secret; in-memory only, never written
	Hash               string    `json:"hash,omitempty"`   // SHA-256(id) hex; the at-rest key
	Prefix             string    `json:"prefix,omitempty"` // first 8 chars of id, for display
	Name               string    `json:"name"`
	Servers            []string  `json:"servers,omitempty"`
	Groups             []string  `json:"groups,omitempty"`
	AllowCommands      []string  `json:"allow_commands,omitempty"`
	DenyCommands       []string  `json:"deny_commands,omitempty"`
	ReadOnlyDefault    bool      `json:"read_only_default,omitempty"`
	DestructiveAllowed bool      `json:"destructive_allowed,omitempty"`
	Created            time.Time `json:"created"`
}

// tokensDocument is the on-disk JSON shape: token HASH -> token. The map key is
// the SHA-256 hash of the bearer id (NOT the id itself), so the cleartext secret
// never touches disk.
type tokensDocument struct {
	Tokens map[string]Token `json:"tokens"`
}

// hashTokenID returns the hex SHA-256 of a bearer token id. This is the at-rest
// key and the value looked up when a --token is presented.
func hashTokenID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// tokenIDPrefix returns the first 8 chars of an id (or the whole id when
// shorter) for non-secret display in `token list` / `token revoke`.
func tokenIDPrefix(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
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

// Create persists a new token. ID, Hash, Prefix and Created are filled in by the
// store; any caller-supplied values for those fields are ignored. Only the Hash
// and Prefix are written to disk — the cleartext ID never is. The returned token
// carries the in-memory ID so the caller can show the bearer secret exactly once.
func (s *TokenStore) Create(t Token) (Token, error) {
	if strings.TrimSpace(t.Name) == "" {
		return Token{}, fmt.Errorf("token name is required")
	}
	id, err := newTokenID()
	if err != nil {
		return Token{}, err
	}
	t.ID = id
	t.Hash = hashTokenID(id)
	t.Prefix = tokenIDPrefix(id)
	t.Created = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return Token{}, err
	}
	if _, exists := doc.Tokens[t.Hash]; exists {
		// Astronomically unlikely with 128 bits of entropy, but be explicit.
		return Token{}, fmt.Errorf("token id collision; please retry")
	}
	// Persist WITHOUT the cleartext id: the on-disk record keeps only Hash+Prefix.
	stored := t
	stored.ID = ""
	doc.Tokens[t.Hash] = stored
	if err := s.write(doc); err != nil {
		return Token{}, err
	}
	return t, nil
}

// List returns all tokens sorted by creation time (oldest first). The cleartext
// bearer id is never stored, so listed tokens carry no secret ID; for display
// convenience the (non-secret) 8-char Prefix is surfaced in the ID field so
// existing prefix-rendering callers keep working. Use Token.Prefix directly when
// the distinction matters.
func (s *TokenStore) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(doc.Tokens))
	for _, t := range doc.Tokens {
		// Surface the prefix as the display id; the real secret is gone from disk.
		t.ID = t.Prefix
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created.Equal(out[j].Created) {
			return out[i].Hash < out[j].Hash
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out, nil
}

// Get returns the token whose bearer id is the given value, or an error if it
// does not exist (i.e. unknown or revoked). The id is hashed and looked up by
// hash, since the cleartext id is never stored. The returned token does NOT
// carry the cleartext ID (only Hash/Prefix), so callers must not rely on it.
func (s *TokenStore) Get(id string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return Token{}, err
	}
	t, ok := doc.Tokens[hashTokenID(id)]
	if !ok {
		return Token{}, fmt.Errorf("token not found")
	}
	return t, nil
}

// Revoke deletes the token whose bearer id is the given value. The id is hashed
// and removed by hash. Revoking an unknown token is an error so the operator
// gets clear feedback.
func (s *TokenStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	hash := hashTokenID(id)
	if _, ok := doc.Tokens[hash]; !ok {
		return fmt.Errorf("token not found")
	}
	delete(doc.Tokens, hash)
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
	"config":    true, // `config show/get/...` are read; mutating subs are classified destructive
	// NOTE: `tag` is intentionally NOT a read command: `tag set` is a WRITE. A
	// read-only token that legitimately needs `tag list` must allow it explicitly
	// via AllowCommands. `tag set` is additionally classified destructive.
	"port":    true, // `port list` is read; `port open/close` are classified destructive
	"version": true,
	"report":  true,
}

// IsReadCommand reports whether a top-level command is in the read-only set.
func IsReadCommand(topCommand string) bool {
	return readCommands[topCommand]
}

// destructiveCommands documents the (topCommand -> subcommands) pairs that are
// treated as destructive for FL-030. A token without DestructiveAllowed cannot
// run any of these. The list is intentionally conservative and explicit:
//
//	server    remove                     tears down/forgets a managed server
//	file      rm                         deletes a remote file/dir
//	key       (any mutating sub)         rotates/regenerates controller keys
//	firewall  enable                     changes live firewall state (lock-out risk)
//	fw        enable                     alias of firewall enable (if present)
//	guard     (any)                      arms/triggers the dead-man guard
//	revert    (any)                      reverts servers to a prior state
//	tag       set                        rewrites server tags (group membership)
//	config    (any mutating sub)         edits controller/server config
//	port      open/close                 changes live exposed-port state
//	cron      add/rm                     schedules/removes remote jobs
//	cmd-policy set                       rewrites the command policy
//	secret    set/rotate/rm              mutates stored secrets
//	policy    set                        rewrites policy
//	svc       restart/stop/disable/      changes live systemd unit state
//	          enable/start
//
// Notes:
//   - drift is read-only (it reports divergence; it does not change anything),
//     so it is NOT destructive.
//   - exec cannot be classified from here (we can't see what the remote command
//     does), so exec is treated as NON-destructive by default; operators scope
//     it via DenyCommands/AllowCommands and the server set instead.
//
// A subcommand value of "*" means the whole top-level command is destructive.
// For "key", every mutating sub is destructive; only the read sub
// "fingerprint" is exempted via keyReadSubs below.
var destructiveCommands = map[string]map[string]bool{
	"server":   {"remove": true},
	"file":     {"rm": true},
	"firewall": {"enable": true},
	"fw":       {"enable": true},
	"guard":    {"*": true},
	"revert":   {"*": true},
	// NOTE: `tag` is special-cased in IsDestructiveCommand (it is a flat command
	// with key=value positionals, not a sub), so it is intentionally not listed
	// here.
	"port":       {"open": true, "close": true},
	"cron":       {"add": true, "rm": true},
	"cmd-policy": {"set": true},
	"secret":     {"set": true, "rotate": true, "rm": true},
	"policy":     {"set": true},
	// svc control mutates live systemd state and must clear the --destructive
	// gate; only `svc status` (read) is exempt by omission.
	"svc": {"restart": true, "stop": true, "disable": true, "enable": true, "start": true},
}

// keyReadSubs are the read-only subcommands of `key`. Every other `key`
// subcommand (rotate, regenerate, etc.) mutates controller keys and is
// destructive. A bare `key` (no sub) is treated as read (it prints help/list).
var keyReadSubs = map[string]bool{
	"fingerprint": true,
	"list":        true,
	"show":        true,
	"export":      true,
}

// configReadSubs are the read-only subcommands of `config`. Every other
// `config` subcommand (set/edit/import/...) mutates config and is destructive.
// A bare `config` (no sub) is treated as read.
var configReadSubs = map[string]bool{
	"show":     true,
	"get":      true,
	"list":     true,
	"validate": true,
	"diff":     true,
}

// IsDestructiveCommand reports whether the given top-level command plus its
// resolved subcommand (sub, from topLevelCommand) is destructive per the
// documented destructiveCommands table.
//
// IMPORTANT: by the time controller-side enforcement runs, cobra has already
// consumed the subcommand token, so the leaf command's positional args do NOT
// contain the subcommand. Callers MUST pass the real subcommand in sub (from
// topLevelCommand), not args[0]. args is accepted for future use and is not
// relied on for classification.
func IsDestructiveCommand(topCommand, sub string, args []string) bool {
	topCommand = strings.TrimSpace(topCommand)
	sub = strings.TrimSpace(sub)

	// `key`: any mutating subcommand is destructive (read subs exempted).
	if topCommand == "key" {
		return sub != "" && !keyReadSubs[sub]
	}
	// `config`: any mutating subcommand is destructive (read subs exempted).
	if topCommand == "config" {
		return sub != "" && !configReadSubs[sub]
	}
	// `tag` is a FLAT command (no cobra subcommand, so sub is always ""):
	// `tag <server> key=value...` WRITES tags (destructive) while `tag` and
	// `tag <server>` only READ. Classify by the presence of a key=value arg.
	if topCommand == "tag" {
		for _, a := range args {
			if strings.Contains(a, "=") {
				return true
			}
		}
		return false
	}

	subs, ok := destructiveCommands[topCommand]
	if !ok {
		return false
	}
	if subs["*"] {
		return true
	}
	return sub != "" && subs[sub]
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

// IsScoped reports whether the token carries ANY constraint at all: a server
// scope (Servers/Groups), a command allow/deny list, or read-only/destructive
// flags. A token with no constraints whatsoever is the admin-equivalent
// credential; everything else is "scoped" and is denied sensitive local-store
// mutations (secrets/policy/cmd-policy/token) by the controller (FL-030).
func (t Token) IsScoped() bool {
	return t.scopesAnyServer() ||
		len(t.AllowCommands) > 0 ||
		len(t.DenyCommands) > 0 ||
		t.ReadOnlyDefault ||
		t.DestructiveAllowed
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
