// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"bufio"
	"bytes"
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

	// tail caches the Hash of the last entry on disk together with the identity,
	// size and modification time of the file it belongs to. Append reuses it
	// only while the file is provably unchanged since this instance's own last
	// append; an append by another process (or another AuditLog instance for the
	// same path) changes the size, so the tail is then re-read from disk.
	tail auditTailCache
}

type auditTailCache struct {
	valid   bool
	info    os.FileInfo
	size    int64
	modTime time.Time
	hash    string
}

// maxAuditLineBytes bounds a single audit line. It matches the bufio.Scanner
// buffer ReadAll has always used, so the backwards tail reader accepts exactly
// the lines the full reader accepts.
const maxAuditLineBytes = 4 * 1024 * 1024

// auditTailChunk is how much the backwards tail reader reads per step.
const auditTailChunk = 64 * 1024

func NewAuditLog(path string) *AuditLog {
	return &AuditLog{path: path}
}

// Append adds one entry to the end of the log, linking it into the hash chain.
//
// Its cost does not depend on the size of the log: the previous entry's Hash is
// found by reading the file backwards from the end (only the last line is
// decoded) and is cached in memory between appends. The read-last-hash + append
// sequence runs under a cross-process advisory lock on a "<log>.lock" sidecar,
// so concurrent fleet processes can never both link to the same predecessor and
// fork the chain.
func (a *AuditLog) Append(entry AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}

	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return fmt.Errorf("create audit log directory: %w", err)
	}

	return withAuditFileLock(a.path+".lock", func() error {
		return a.appendLocked(entry)
	})
}

// appendLocked performs the append. The caller holds a.mu and the cross-process
// file lock.
func (a *AuditLog) appendLocked(entry AuditEntry) error {
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat audit log: %w", err)
	}

	// Tamper-evidence: link this entry to the previous one's hash. The previous
	// hash is "" for the very first entry, or when the last existing entry is a
	// legacy (un-hashed) one — in which case the chain begins here. A
	// caller-supplied Hash/PrevHash is always overwritten so the chain can be
	// trusted.
	prevHash, missingNewline, err := a.lastEntryHashLocked(f, info)
	if err != nil {
		return err
	}
	entry.PrevHash = prevHash
	entry.Hash = entryDigest(entry)

	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal audit entry: %w", err)
	}
	line := make([]byte, 0, len(payload)+2)
	if missingNewline {
		// The last existing entry decoded fine but is not newline-terminated
		// (e.g. the file was hand-edited). Terminate it so the new entry lands
		// on its own line instead of being glued onto that one.
		line = append(line, '\n')
	}
	line = append(line, payload...)
	line = append(line, '\n')
	a.tail = auditTailCache{}
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	if after, err := f.Stat(); err == nil && after.Size() == info.Size()+int64(len(line)) {
		a.tail = auditTailCache{valid: true, info: after, size: after.Size(), modTime: after.ModTime(), hash: entry.Hash}
	}
	return nil
}

// lastEntryHashLocked returns the Hash of the final entry currently on disk, or
// "" when the log is empty or its last entry predates the chain (a legacy
// un-hashed entry). missingNewline reports that the file does not end with a
// newline. The caller must hold a.mu and the file lock. Only the last line is
// read and decoded; an undecodable last line (e.g. a torn write) is an error,
// exactly as it was when the whole file was decoded here.
func (a *AuditLog) lastEntryHashLocked(f *os.File, info os.FileInfo) (hash string, missingNewline bool, err error) {
	size := info.Size()
	if size == 0 {
		return "", false, nil
	}
	if c := a.tail; c.valid && c.size == size && c.modTime.Equal(info.ModTime()) && os.SameFile(c.info, info) {
		// The cache is only ever filled by this instance's own completed append,
		// which always ends with a newline.
		return c.hash, false, nil
	}
	lines, endsWithNewline, err := readTailLines(f, size, 1)
	if err != nil {
		return "", false, err
	}
	if len(lines) == 0 {
		return "", false, nil
	}
	var last AuditEntry
	if err := json.Unmarshal(lines[0], &last); err != nil {
		return "", false, fmt.Errorf("decode audit entry: %w", err)
	}
	return last.Hash, !endsWithNewline, nil
}

// Tail returns the last n entries of the log in file order (oldest first). It
// reads backwards from the end of the file, so its cost depends on n rather
// than on the size of the log. A missing log yields nil. Lines are decoded
// exactly as ReadAll decodes them; an undecodable line among the last n is an
// error.
func (a *AuditLog) Tail(n int) ([]AuditEntry, error) {
	if n <= 0 {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	f, err := os.Open(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat audit log: %w", err)
	}
	lines, _, err := readTailLines(f, info.Size(), n)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	entries := make([]AuditEntry, 0, len(lines))
	for _, line := range lines {
		var entry AuditEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("decode audit entry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// readTailLines returns (up to) the last n lines among the first size bytes of
// f, oldest first. It follows bufio.ScanLines, so it agrees with ReadAll on what
// a line is: lines are separated by '\n', a trailing '\r' is dropped, a final
// line without a newline still counts, and a trailing newline does not start an
// extra empty line. endsWithNewline reports whether the data ends with '\n'. A
// line longer than maxAuditLineBytes fails the way the scanner does.
func readTailLines(f *os.File, size int64, n int) (lines [][]byte, endsWithNewline bool, err error) {
	if size <= 0 || n <= 0 {
		return nil, false, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return nil, false, fmt.Errorf("read audit log: %w", err)
	}
	endsWithNewline = last[0] == '\n'
	limit := size // the lines are the '\n'-separated segments of [0, limit)
	if endsWithNewline {
		limit--
	}

	// Walk backwards until [start, limit) holds the last n lines: either the
	// n-th newline counting back from limit has been seen, or the start of the
	// file has been reached. Chunks are collected newest-first and joined once.
	var chunks [][]byte
	start := limit
	found := 0
	sinceNewline := 0 // bytes of the (partial) line currently being walked
	for start > 0 {
		chunk := int64(auditTailChunk)
		if chunk > start {
			chunk = start
		}
		buf := make([]byte, chunk)
		if _, err := f.ReadAt(buf, start-chunk); err != nil {
			return nil, false, fmt.Errorf("read audit log: %w", err)
		}
		cut := -1
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				sinceNewline++
				continue
			}
			sinceNewline = 0
			found++
			if found == n {
				cut = i
				break
			}
		}
		if sinceNewline > maxAuditLineBytes {
			// Longer than any line the scanner accepts: fail early instead of
			// buffering an unbounded newline-free run of bytes.
			return nil, false, fmt.Errorf("scan audit log: %w", bufio.ErrTooLong)
		}
		if cut >= 0 {
			chunks = append(chunks, buf[cut+1:])
			break
		}
		chunks = append(chunks, buf)
		start -= chunk
	}
	for i, j := 0, len(chunks)-1; i < j; i, j = i+1, j-1 {
		chunks[i], chunks[j] = chunks[j], chunks[i]
	}
	region := bytes.Join(chunks, nil)

	parts := bytes.Split(region, []byte{'\n'})
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	lines = make([][]byte, 0, len(parts))
	for _, part := range parts {
		if len(part) > maxAuditLineBytes {
			return nil, false, fmt.Errorf("scan audit log: %w", bufio.ErrTooLong)
		}
		lines = append(lines, bytes.TrimSuffix(part, []byte{'\r'}))
	}
	return lines, endsWithNewline, nil
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
