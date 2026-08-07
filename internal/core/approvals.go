// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Approval status values. A staged request is pending until an operator
// approves or rejects it, or until its TTL elapses (expired).
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
)

// DefaultApprovalTTL is used by Stage when the caller passes a non-positive ttl.
const DefaultApprovalTTL = time.Hour

// Approval is a single staged command awaiting an approve/reject decision.
// It is persisted to <configDir>/approvals.json as part of a JSON array.
type Approval struct {
	ID        string    `json:"id"`
	Server    string    `json:"server"`
	Command   string    `json:"command"`
	Status    string    `json:"status"`
	Requested time.Time `json:"requested"`
	Expires   time.Time `json:"expires"`
}

// Expired reports whether a still-pending approval has passed its expiry at the
// given moment. Already-decided approvals (approved/rejected/expired) are never
// re-classified.
func (a Approval) Expired(now time.Time) bool {
	return a.Status == ApprovalPending && !a.Expires.IsZero() && !now.Before(a.Expires)
}

// ApprovalStore is a small standalone JSON-backed store for staged command
// approvals. Read/modify/write operations are serialized by a cross-process
// advisory lock, while write publishes the document with an atomic rename.
type ApprovalStore struct {
	path string
	// now is injected so tests can control time; defaults to time.Now.
	now func() time.Time
	// entropy is injected so crypto/rand failures can be propagated and tested.
	entropy io.Reader
}

func (s *ApprovalStore) withWriteLock(fn func() error) error {
	return withAdvisoryFileLock(s.path+".lock", fn)
}

// NewApprovalStore opens (without reading) an approval store rooted at
// configDir. If configDir is empty the default config dir is used.
func NewApprovalStore(configDir string) *ApprovalStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	path := ApprovalsPath(configDir)
	return &ApprovalStore{path: path, now: time.Now, entropy: rand.Reader}
}

// ApprovalsPath returns the on-disk location of the approvals document.
func ApprovalsPath(configDir string) string {
	return filepath.Join(configDir, "approvals.json")
}

func (s *ApprovalStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *ApprovalStore) read() ([]Approval, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read approvals: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var approvals []Approval
	if err := json.Unmarshal(data, &approvals); err != nil {
		return nil, fmt.Errorf("decode approvals: %w", err)
	}
	return approvals, nil
}

func (s *ApprovalStore) write(approvals []Approval) error {
	if approvals == nil {
		approvals = []Approval{}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(approvals, "", "  ")
	if err != nil {
		return fmt.Errorf("encode approvals: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".approvals-*.json")
	if err != nil {
		return fmt.Errorf("write approvals: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write approvals: %w", err)
	}
	if n, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write approvals: %w", err)
	} else if n != len(data) {
		_ = tmp.Close()
		return fmt.Errorf("write approvals: %w", io.ErrShortWrite)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write approvals: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write approvals: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write approvals: %w", err)
	}
	return nil
}

// markExpired flips any pending-but-past-expiry approvals to expired in place
// and reports whether anything changed.
func markExpired(approvals []Approval, now time.Time) bool {
	changed := false
	for i := range approvals {
		if approvals[i].Expired(now) {
			approvals[i].Status = ApprovalExpired
			changed = true
		}
	}
	return changed
}

// newApprovalID returns a random 16-character hex id for an approval.
func newApprovalID(entropy io.Reader) (string, error) {
	if entropy == nil {
		entropy = rand.Reader
	}
	var b [8]byte
	if _, err := io.ReadFull(entropy, b[:]); err != nil {
		return "", fmt.Errorf("generate approval id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Stage records a new pending approval for running command on server, expiring
// after ttl, and returns its generated id. A non-positive ttl uses
// DefaultApprovalTTL. Expired approvals are pruned to expired-status on the way.
func (s *ApprovalStore) Stage(server, command string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(server) == "" {
		return "", fmt.Errorf("server name is required")
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("command is required")
	}
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	var result string
	err := s.withWriteLock(func() error {
		id, err := newApprovalID(s.entropy)
		if err != nil {
			return err
		}
		approvals, err := s.read()
		if err != nil {
			return err
		}
		now := s.clock()
		markExpired(approvals, now)
		approval := Approval{
			ID:        id,
			Server:    server,
			Command:   command,
			Status:    ApprovalPending,
			Requested: now.UTC(),
			Expires:   now.UTC().Add(ttl),
		}
		approvals = append(approvals, approval)
		if err := s.write(approvals); err != nil {
			return err
		}
		result = approval.ID
		return nil
	})
	return result, err
}

// decide transitions a pending approval identified by id into status. It
// returns an error if the id is unknown or the approval is not pending (already
// approved, rejected, or expired).
func (s *ApprovalStore) decide(id, status string) (Approval, error) {
	var result Approval
	err := s.withWriteLock(func() error {
		approvals, err := s.read()
		if err != nil {
			return err
		}
		now := s.clock()
		// markExpired may flip pending->expired records as a side effect. Those
		// changes must be persisted even when this call ultimately returns an error.
		expiredChanged := markExpired(approvals, now)
		persistOnError := func(retErr error) error {
			if expiredChanged {
				if writeErr := s.write(approvals); writeErr != nil {
					return writeErr
				}
			}
			return retErr
		}
		idx := -1
		for i := range approvals {
			if approvals[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return persistOnError(fmt.Errorf("approval %q not found", id))
		}
		if approvals[idx].Status != ApprovalPending {
			return persistOnError(fmt.Errorf("approval %q is %s, not pending", id, approvals[idx].Status))
		}
		approvals[idx].Status = status
		if err := s.write(approvals); err != nil {
			return err
		}
		result = approvals[idx]
		return nil
	})
	return result, err
}

// Approve marks a pending approval approved and returns the updated record.
func (s *ApprovalStore) Approve(id string) (Approval, error) {
	return s.decide(id, ApprovalApproved)
}

// Reject marks a pending approval rejected and returns the updated record.
func (s *ApprovalStore) Reject(id string) (Approval, error) {
	return s.decide(id, ApprovalRejected)
}

// Get returns the approval with the given id. Pending-but-expired approvals are
// reported with status expired (and persisted so the on-disk state matches).
func (s *ApprovalStore) Get(id string) (Approval, error) {
	var result Approval
	err := s.withWriteLock(func() error {
		approvals, err := s.read()
		if err != nil {
			return err
		}
		if markExpired(approvals, s.clock()) {
			if err := s.write(approvals); err != nil {
				return err
			}
		}
		for _, approval := range approvals {
			if approval.ID == id {
				result = approval
				return nil
			}
		}
		return fmt.Errorf("approval %q not found", id)
	})
	return result, err
}

// List returns all approvals, newest request first, after flipping any expired
// pending requests to expired status (persisted so on-disk state matches).
func (s *ApprovalStore) List() ([]Approval, error) {
	var result []Approval
	err := s.withWriteLock(func() error {
		approvals, err := s.read()
		if err != nil {
			return err
		}
		if markExpired(approvals, s.clock()) {
			if err := s.write(approvals); err != nil {
				return err
			}
		}
		sort.SliceStable(approvals, func(i, j int) bool {
			return approvals[i].Requested.After(approvals[j].Requested)
		})
		result = approvals
		return nil
	})
	return result, err
}

// PruneExpired flips pending-but-past-expiry approvals to expired status and
// persists if anything changed. It returns the number of approvals expired.
func (s *ApprovalStore) PruneExpired() (int, error) {
	count := 0
	err := s.withWriteLock(func() error {
		approvals, err := s.read()
		if err != nil {
			return err
		}
		now := s.clock()
		for i := range approvals {
			if approvals[i].Expired(now) {
				approvals[i].Status = ApprovalExpired
				count++
			}
		}
		if count > 0 {
			return s.write(approvals)
		}
		return nil
	})
	return count, err
}
