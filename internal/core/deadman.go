// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// FL-001 dead-man's-switch.
//
// `fleet guard` runs a risky command on a server and arms a DETACHED, server-side
// timer that runs an undo ("revert") script after a deadline UNLESS an operator
// confirms in time. This is the classic safeguard for changes that can lock you
// out (firewall, sshd, network): if the change is good you `fleet confirm <id>`;
// if you get locked out the timer reverts automatically with no controller round
// trip required, because the timer lives entirely on the server.
//
// The design touches no proto/agent code: everything is expressed as ordinary
// shell run through App.ExecCommand. The revert script is written to
// /run/fleet-guard-<id>.sh, the risky command runs, then an independent revert is
// scheduled with `systemd-run --on-active` (preferred) or a detached
// `setsid sh -c 'sleep N; ...'` fallback. Confirming touches a `.ok` sentinel and
// stops the timer unit; reverting just runs the script now.
//
// Guard records live in <configDir>/guards.json (a small JSON store, same shape
// as tags.go) so `confirm`/`revert` can find the server and the revert command
// without re-typing them. Change-ids are derived from the server name plus an
// incrementing counter (NOT random, NOT time) so they are deterministic and
// charset-safe for use in unit names and /run paths.

// GuardStatus is the lifecycle state of a guard.
type GuardStatus string

const (
	GuardPending   GuardStatus = "pending"
	GuardConfirmed GuardStatus = "confirmed"
	GuardReverted  GuardStatus = "reverted"
)

// GuardRecord is one armed dead-man's-switch, persisted in guards.json.
type GuardRecord struct {
	ID          string      `json:"id"`
	Server      string      `json:"server"`
	Status      GuardStatus `json:"status"`
	RevertCmd   string      `json:"revertCmd"`
	RiskyCmd    string      `json:"riskyCmd,omitempty"`
	RevertAfter string      `json:"revertAfter,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	UpdatedAt   time.Time   `json:"updatedAt"`
}

// guardsDocument is the on-disk JSON shape: a per-server counter (so ids are
// deterministic and incrementing) plus the guard records keyed by id.
type guardsDocument struct {
	Counters map[string]int          `json:"counters"`
	Guards   map[string]*GuardRecord `json:"guards"`
}

// GuardStore is a small standalone JSON store for guard records, modeled on
// TagStore in tags.go. It is kept off *App so it does not require touching
// app.go, and it is opened from a config dir.
type GuardStore struct {
	path string
	mu   sync.Mutex
}

// NewGuardStore opens (without reading) a guard store rooted at configDir. If
// configDir is empty the default config dir is used.
func NewGuardStore(configDir string) *GuardStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	return &GuardStore{path: GuardsPath(configDir)}
}

// GuardsPath returns the on-disk location of the guards document for a config dir.
func GuardsPath(configDir string) string {
	return filepath.Join(configDir, "guards.json")
}

func (s *GuardStore) read() (guardsDocument, error) {
	doc := guardsDocument{Counters: map[string]int{}, Guards: map[string]*GuardRecord{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, fmt.Errorf("read guards: %w", err)
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("decode guards: %w", err)
	}
	if doc.Counters == nil {
		doc.Counters = map[string]int{}
	}
	if doc.Guards == nil {
		doc.Guards = map[string]*GuardRecord{}
	}
	return doc, nil
}

func (s *GuardStore) write(doc guardsDocument) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode guards: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".guards-*.json")
	if err != nil {
		return fmt.Errorf("write guards: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write guards: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write guards: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write guards: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write guards: %w", err)
	}
	return nil
}

// guardIDPattern restricts a change-id to a strict charset so it is safe to embed
// in a systemd unit name and a /run/fleet-guard-<id>.sh path with no quoting
// surprises. Ids we generate are always of this shape; ids accepted from the CLI
// are validated against it before use.
var guardIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

// ValidateGuardID checks a change-id against the allowed charset.
func ValidateGuardID(id string) error {
	if !guardIDPattern.MatchString(id) {
		return fmt.Errorf("invalid guard id %q (use letters, digits, '.', '_', '-'; max 80 chars)", id)
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("invalid guard id %q (must not contain '..')", id)
	}
	return nil
}

// guardIDSlug reduces a server name to the guard-id charset. Server names are
// already restricted (validateSafeName), but we sanitize defensively so the
// derived id is always pattern-safe even if naming rules ever loosen.
func guardIDSlug(server string) string {
	var b strings.Builder
	for _, r := range server {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	slug := b.String()
	// Collapse any run of dots so a name like "web/../01" cannot yield a slug
	// containing ".." (which ValidateGuardID rejects and which is unsafe in a path).
	for strings.Contains(slug, "..") {
		slug = strings.ReplaceAll(slug, "..", ".")
	}
	slug = strings.Trim(slug, "-.")
	if slug == "" {
		slug = "server"
	}
	return slug
}

// NextGuardID derives the next change-id for a server: "<slug>-<counter>". The
// counter is incremented and persisted so ids are deterministic and unique per
// server without using randomness or wall-clock time.
func (s *GuardStore) NextGuardID(server string) (string, error) {
	if server == "" {
		return "", fmt.Errorf("server name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return "", err
	}
	slug := guardIDSlug(server)
	// Skip any id that already exists (e.g. records imported or restored) so we
	// never collide with a live guard.
	for {
		doc.Counters[slug]++
		id := fmt.Sprintf("%s-%d", slug, doc.Counters[slug])
		if _, exists := doc.Guards[id]; !exists {
			if err := s.write(doc); err != nil {
				return "", err
			}
			return id, nil
		}
	}
}

// Put inserts or replaces a guard record (stamping UpdatedAt).
func (s *GuardStore) Put(rec GuardRecord) error {
	if err := ValidateGuardID(rec.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	copy := rec
	doc.Guards[rec.ID] = &copy
	return s.write(doc)
}

// Get returns the guard record for an id (ok=false if not found).
func (s *GuardStore) Get(id string) (GuardRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return GuardRecord{}, false
	}
	rec, ok := doc.Guards[id]
	if !ok || rec == nil {
		return GuardRecord{}, false
	}
	return *rec, true
}

// SetStatus updates the status of a guard record. It returns an error if the id
// is unknown.
func (s *GuardStore) SetStatus(id string, status GuardStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	rec, ok := doc.Guards[id]
	if !ok || rec == nil {
		return fmt.Errorf("guard %q not found", id)
	}
	rec.Status = status
	rec.UpdatedAt = time.Now().UTC()
	return s.write(doc)
}

// List returns all guard records sorted by id.
func (s *GuardStore) List() ([]GuardRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]GuardRecord, 0, len(doc.Guards))
	for _, rec := range doc.Guards {
		if rec != nil {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// guardScriptPath / guardOKPath / guardUnit return the server-side paths and unit
// name for a guard id. Ids are charset-safe (ValidateGuardID), so these need no
// further quoting when used as literals, but callers still shell-quote them.
func guardScriptPath(id string) string { return "/run/fleet-guard-" + id + ".sh" }
func guardOKPath(id string) string     { return "/run/fleet-guard-" + id + ".ok" }
func guardUnit(id string) string       { return "fleet-guard-" + id }

// BuildGuardArmCommand builds the remote shell command that arms a guard: it
// writes the revert script to /run, runs the risky command, and schedules an
// INDEPENDENT revert that fires after revertSeconds unless the .ok sentinel
// exists. The revert prefers a systemd transient timer (survives the SSH session)
// and falls back to a detached setsid sleep loop.
//
// Every interpolated value (id, script body, risky command, delay) is
// shell-quoted. The risky command runs via `sh -c <quoted>` so the operator can
// pass a full command line; its exit status is preserved for the caller.
func BuildGuardArmCommand(id, riskyCmd, revertScript string, revertSeconds int) (string, error) {
	if err := ValidateGuardID(id); err != nil {
		return "", err
	}
	if revertSeconds <= 0 {
		return "", fmt.Errorf("revert delay must be a positive number of seconds")
	}
	if strings.TrimSpace(riskyCmd) == "" {
		return "", fmt.Errorf("a risky command is required")
	}
	if strings.TrimSpace(revertScript) == "" {
		return "", fmt.Errorf("a revert command is required")
	}

	script := guardScriptPath(id)
	ok := guardOKPath(id)
	unit := guardUnit(id)
	secs := fmt.Sprintf("%d", revertSeconds)

	// The revert script body is written verbatim inside a quoted heredoc (below),
	// so the only way a crafted revert command could break out is by containing a
	// line that exactly equals the heredoc delimiter. Reject that so the heredoc
	// is unbreakable. (The delimiter is a fixed, unlikely token.)
	const delim = "FLEET_GUARD_EOF"
	for _, line := range strings.Split(revertScript, "\n") {
		if strings.TrimRight(line, "\r") == delim {
			return "", fmt.Errorf("revert command must not contain a line equal to %q", delim)
		}
	}

	// The body of the revert script. It re-checks the .ok sentinel itself so the
	// systemd-run path is also a no-op once the operator has confirmed (systemd
	// stops the timer, but the script guarding itself is belt-and-suspenders).
	body := "#!/bin/sh\n" +
		"[ -f " + shellQuote(ok) + " ] && exit 0\n" +
		revertScript + "\n"

	// Write the revert script via a quoted heredoc so $, `, \ in the body are
	// passed through literally and never expanded by the writing shell. If /run
	// is not writable the cat (or chmod) fails; abort BEFORE running the risky
	// command so it never runs unguarded with no revert script in place. The
	// heredoc terminator cannot carry a trailing `|| exit 1`, so the write and
	// chmod are guarded on the lines that follow.
	writeScript := "cat > " + shellQuote(script) + " <<'" + delim + "'\n" + body + delim + "\n" +
		"[ -f " + shellQuote(script) + " ] || { echo 'fleet-guard: failed to write revert script' >&2; exit 1; }\n" +
		"chmod 700 " + shellQuote(script) + " || { echo 'fleet-guard: failed to chmod revert script' >&2; exit 1; }\n" +
		// Clear any stale sentinel from a previous guard reusing this id.
		"rm -f " + shellQuote(ok) + "\n"

	// Run the risky command, capturing its exit status to report back.
	runRisky := "sh -c " + shellQuote(riskyCmd) + "\n" +
		"__fleet_guard_rc=$?\n"

	// Schedule the detached revert. Prefer a systemd transient timer; fall back to
	// a setsid sleep loop fully detached from this session's stdio. If BOTH paths
	// fail to arm a timer, the dead-man's-switch is inactive — surface that loudly
	// and exit non-zero so the operator knows no auto-revert is scheduled, rather
	// than masking it behind the risky command's status.
	schedule := "__fleet_guard_armed=0\n" +
		"if command -v systemd-run >/dev/null 2>&1; then\n" +
		"  if systemd-run --on-active=" + shellQuote(secs) +
		" --unit=" + shellQuote(unit) +
		" /bin/sh " + shellQuote(script) + " >/dev/null 2>&1; then\n" +
		"    __fleet_guard_armed=1\n" +
		"  fi\n" +
		"fi\n" +
		// Fallback: a detached setsid sleep loop. `&` cannot be used as an `if`
		// condition, so gate on `command -v setsid` first, then launch in the
		// background; the background launch itself does not fail synchronously, so
		// reaching here with setsid present means the timer is armed.
		"if [ \"$__fleet_guard_armed\" -ne 1 ] && command -v setsid >/dev/null 2>&1; then\n" +
		"  setsid sh -c " +
		shellQuote("sleep "+secs+"; [ -f "+ok+" ] || sh "+script) +
		" </dev/null >/dev/null 2>&1 &\n" +
		"  __fleet_guard_armed=1\n" +
		"fi\n" +
		"if [ \"$__fleet_guard_armed\" -ne 1 ]; then\n" +
		"  echo 'fleet-guard: FAILED to schedule auto-revert; dead-man-switch is INACTIVE' >&2\n" +
		"  exit 1\n" +
		"fi\n"

	// Exit with the risky command's status so the caller sees whether it worked.
	tail := "exit $__fleet_guard_rc\n"

	return writeScript + runRisky + schedule + tail, nil
}

// BuildGuardConfirmCommand builds the remote shell command that confirms a guard:
// it touches the .ok sentinel (so any still-pending revert becomes a no-op) and
// stops the systemd timer unit to cancel it outright. The `|| true` keeps confirm
// succeeding even when the fallback (non-systemd) path was used, where there is no
// timer unit to stop.
func BuildGuardConfirmCommand(id string) (string, error) {
	if err := ValidateGuardID(id); err != nil {
		return "", err
	}
	ok := guardOKPath(id)
	unit := guardUnit(id)
	return "touch " + shellQuote(ok) + "\n" +
		"systemctl stop " + shellQuote(unit+".timer") + " 2>/dev/null || true\n", nil
}

// BuildGuardRevertCommand builds the remote shell command that reverts NOW. It
// stops the systemd timer unit so the scheduled revert cannot also fire, then
// runs the undo. The undo MUST actually execute, so the .ok sentinel is removed
// FIRST: the on-disk script begins with `[ -f <ok> ] && exit 0`, so leaving the
// sentinel in place would make the script self-skip and the revert a silent
// no-op. After the undo runs we touch the sentinel so any racing setsid fallback
// (which has no timer unit to stop) sees it and becomes a no-op. If the script is
// missing (e.g. /run was cleared by a reboot) the stored revert command is used
// as a fallback so revert still works.
func BuildGuardRevertCommand(id, revertCmd string) (string, error) {
	if err := ValidateGuardID(id); err != nil {
		return "", err
	}
	script := guardScriptPath(id)
	ok := guardOKPath(id)
	unit := guardUnit(id)
	fallback := strings.TrimSpace(revertCmd)
	// Stop the timer, then clear the sentinel so the script's own `[ -f <ok> ]`
	// self-skip guard does NOT trip and the undo really runs.
	cmd := "systemctl stop " + shellQuote(unit+".timer") + " 2>/dev/null || true\n" +
		"rm -f " + shellQuote(ok) + "\n"
	if fallback == "" {
		// No stored fallback: run whatever is on disk.
		cmd += "sh " + shellQuote(script) + "\n"
	} else {
		cmd += "if [ -f " + shellQuote(script) + " ]; then\n" +
			"  sh " + shellQuote(script) + "\n" +
			"else\n" +
			"  sh -c " + shellQuote(fallback) + "\n" +
			"fi\n"
	}
	// Capture the undo's exit status, then leave the sentinel in place so a
	// detached setsid fallback that is still sleeping treats the revert as
	// already handled. Exit with the undo's status so a failed revert is visible
	// to the caller rather than masked by the trailing touch.
	cmd += "__fleet_guard_rc=$?\n" +
		"touch " + shellQuote(ok) + "\n" +
		"exit $__fleet_guard_rc\n"
	return cmd, nil
}

// DefaultRevertCommand is used when the operator arms a guard without giving an
// explicit --revert-cmd. It does nothing but print a loud warning to the server's
// system log, so the operator is reminded that no automatic undo was configured.
func DefaultRevertCommand(id string) string {
	msg := "fleet-guard " + id + ": no --revert-cmd was provided; nothing to revert automatically"
	return "echo " + shellQuote(msg) + " | logger -t fleet-guard 2>/dev/null || echo " + shellQuote(msg)
}
