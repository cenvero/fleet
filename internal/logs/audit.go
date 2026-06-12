// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry is one appended audit record.
//
// Tamper-evidence (hash chain): each appended entry carries PrevHash (the Hash of
// the immediately preceding entry, "" for the first chained entry) and Hash (the
// SHA-256 of this entry's content together with PrevHash). Because each Hash
// commits to the previous Hash, deleting, reordering, or editing any entry breaks
// the chain at that point and is detectable by Verify. The log is still plain
// append-only JSONL — this does not encrypt or sign anything, it only makes
// undetectable tampering by a writer infeasible without rewriting every later
// entry's hash.
//
// Backward-compat: both fields are omitempty, so a legacy log written before the
// chain existed (entries with neither field) still parses unchanged. The chain
// simply BEGINS at the next Append: that entry's PrevHash links to the last
// entry's Hash if one exists ("" otherwise), and every entry from there on is
// chained. Verify tolerates a leading run of legacy un-hashed entries.
type AuditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Operator  string    `json:"operator"`
	Details   string    `json:"details,omitempty"`
	// PrevHash and Hash form the tamper-evidence chain (see type doc). They are
	// excluded from the hashed content (Hash never hashes itself) but PrevHash IS
	// part of what Hash commits to, via entryDigest.
	PrevHash string `json:"prev_hash,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

// entryDigest returns the hex SHA-256 over an entry's content plus its PrevHash,
// deterministically and independently of JSON field ordering or the Hash field
// itself. Changing this serialization would invalidate every previously written
// Hash, so it is fixed: timestamp (RFC3339Nano, UTC) | action | target |
// operator | details | prev_hash, NUL-separated.
func entryDigest(e AuditEntry) string {
	h := sha256.New()
	sep := []byte{0}
	h.Write([]byte(e.Timestamp.UTC().Format(time.RFC3339Nano)))
	h.Write(sep)
	h.Write([]byte(e.Action))
	h.Write(sep)
	h.Write([]byte(e.Target))
	h.Write(sep)
	h.Write([]byte(e.Operator))
	h.Write(sep)
	h.Write([]byte(e.Details))
	h.Write(sep)
	h.Write([]byte(e.PrevHash))
	return hex.EncodeToString(h.Sum(nil))
}

type AuditLog struct {
	path string
	mu   sync.Mutex
}

func NewAuditLog(path string) *AuditLog {
	return &AuditLog{path: path}
}

func (a *AuditLog) Append(entry AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}

	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return fmt.Errorf("create audit log directory: %w", err)
	}

	// Tamper-evidence: link this entry to the previous one's hash. The previous
	// hash is "" for the very first entry, or when the existing log is entirely
	// legacy (un-hashed) — in which case the chain begins here. A caller-supplied
	// Hash/PrevHash is always overwritten so the chain can be trusted.
	prevHash, err := a.lastEntryHashLocked()
	if err != nil {
		return err
	}
	entry.PrevHash = prevHash
	entry.Hash = entryDigest(entry)

	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()

	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal audit entry: %w", err)
	}
	if _, err := f.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	return nil
}

// lastEntryHashLocked returns the Hash of the final entry currently on disk, or
// "" when the log is missing, empty, or its last entry predates the chain (a
// legacy un-hashed entry). The caller must hold a.mu. It reads the whole file —
// audit logs are bounded and ReadAll already does the same.
func (a *AuditLog) lastEntryHashLocked() (string, error) {
	entries, err := a.readAllLocked()
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", nil
	}
	return entries[len(entries)-1].Hash, nil
}

func (a *AuditLog) ReadAll() ([]AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readAllLocked()
}

// readAllLocked reads and decodes every entry from the log. The caller must hold
// a.mu. A missing file yields a nil slice with no error.
func (a *AuditLog) readAllLocked() ([]AuditEntry, error) {
	f, err := os.Open(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()

	var entries []AuditEntry
	scanner := bufio.NewScanner(f)
	// Default 64 KiB limit is too small for audit lines that embed large payloads.
	// 4 MiB handles any realistic single-line entry without unbounded allocation.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var entry AuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("decode audit entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan audit log: %w", err)
	}
	return entries, nil
}

// Verify checks the tamper-evidence hash chain over the whole log and reports the
// 0-based index of the first entry that fails, or -1 when the log is intact.
//
// It tolerates a leading run of LEGACY entries written before the chain existed
// (entries with an empty Hash): those are accepted without verification, and the
// chain is required to be contiguous from the first hashed entry onward. Once a
// hashed entry is seen, every subsequent entry MUST be hashed, its Hash must
// match entryDigest(entry), and its PrevHash must equal the previous entry's
// Hash. A break at index i means the entry at i (or an entry before it) was
// edited, reordered, or that an entry was deleted. ok is true and idx is -1 when
// the chain verifies (including a wholly-legacy log).
func (a *AuditLog) Verify() (ok bool, idx int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entries, err := a.readAllLocked()
	if err != nil {
		return false, -1, err
	}
	prevHash := ""
	chainStarted := false
	for i, e := range entries {
		if e.Hash == "" {
			if chainStarted {
				// A hashed entry was already seen; a later un-hashed entry means an
				// entry was removed or tampered to drop its hash.
				return false, i, nil
			}
			// Still in the legacy prefix: accept and keep scanning. Do not advance
			// prevHash, so the first hashed entry's PrevHash links to the last
			// hashed entry ("" while in the legacy prefix).
			continue
		}
		chainStarted = true
		if e.PrevHash != prevHash {
			return false, i, nil
		}
		if entryDigest(e) != e.Hash {
			return false, i, nil
		}
		prevHash = e.Hash
	}
	return true, -1, nil
}
