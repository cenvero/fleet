// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

type Alert struct {
	ID              string     `json:"id"`
	Code            string     `json:"code,omitempty"`
	Server          string     `json:"server,omitempty"`
	Severity        Severity   `json:"severity"`
	Message         string     `json:"message"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	AcknowledgedAt  *time.Time `json:"acknowledged_at,omitempty"`
	SuppressedUntil *time.Time `json:"suppressed_until,omitempty"`
	LastNotifiedAt  *time.Time `json:"last_notified_at,omitempty"`
	Occurrences     int        `json:"occurrences,omitempty"`
	NotifyCount     int        `json:"notify_count,omitempty"`
}

type Store struct {
	dir string
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) Get(id string) (Alert, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return Alert{}, fmt.Errorf("read alert: %w", err)
	}
	var alert Alert
	if err := json.Unmarshal(data, &alert); err != nil {
		return Alert{}, fmt.Errorf("decode alert: %w", err)
	}
	return alert, nil
}

func (s *Store) Save(alert Alert) error {
	if alert.ID == "" {
		return fmt.Errorf("alert id is required")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create alerts directory: %w", err)
	}
	existing, err := s.Get(alert.ID)
	switch {
	case err == nil:
		if alert.CreatedAt.IsZero() {
			alert.CreatedAt = existing.CreatedAt
		}
		if alert.UpdatedAt.IsZero() {
			alert.UpdatedAt = existing.UpdatedAt
		}
		if alert.LastNotifiedAt == nil {
			alert.LastNotifiedAt = existing.LastNotifiedAt
		}
		if alert.Occurrences == 0 {
			alert.Occurrences = existing.Occurrences
		}
		if alert.NotifyCount == 0 {
			alert.NotifyCount = existing.NotifyCount
		}
	case errors.Is(err, os.ErrNotExist):
		if alert.CreatedAt.IsZero() {
			alert.CreatedAt = time.Now().UTC()
		}
	case err != nil:
		return err
	}
	if alert.UpdatedAt.IsZero() {
		alert.UpdatedAt = time.Now().UTC()
	}
	if alert.Occurrences == 0 {
		alert.Occurrences = 1
	}
	path := filepath.Join(s.dir, alert.ID+".json")
	data, err := json.MarshalIndent(alert, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write alert: %w", err)
	}
	return nil
}

func (s *Store) List(severity string) ([]Alert, error) {
	return s.ListFiltered("", severity)
}

func (s *Store) ListFiltered(server, severity string) ([]Alert, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read alerts directory: %w", err)
	}

	var alerts []Alert
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read alert: %w", err)
		}
		var alert Alert
		if err := json.Unmarshal(data, &alert); err != nil {
			return nil, fmt.Errorf("decode alert: %w", err)
		}
		if server != "" && alert.Server != server {
			continue
		}
		if severity != "" && string(alert.Severity) != severity {
			continue
		}
		alerts = append(alerts, alert)
	}

	sort.Slice(alerts, func(i, j int) bool {
		left := alerts[i].UpdatedAt
		if left.IsZero() {
			left = alerts[i].CreatedAt
		}
		right := alerts[j].UpdatedAt
		if right.IsZero() {
			right = alerts[j].CreatedAt
		}
		return left.After(right)
	})
	return alerts, nil
}

func (s *Store) Ack(id string) error {
	alert, err := s.Get(id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	alert.AcknowledgedAt = &now
	alert.UpdatedAt = now
	return s.Save(alert)
}

func (s *Store) Suppress(id string, until time.Time) error {
	alert, err := s.Get(id)
	if err != nil {
		return err
	}
	until = until.UTC()
	alert.SuppressedUntil = &until
	alert.UpdatedAt = time.Now().UTC()
	return s.Save(alert)
}

func (s *Store) Unsuppress(id string) error {
	alert, err := s.Get(id)
	if err != nil {
		return err
	}
	alert.SuppressedUntil = nil
	alert.UpdatedAt = time.Now().UTC()
	return s.Save(alert)
}

func (s *Store) Delete(id string) error {
	path := filepath.Join(s.dir, id+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete alert: %w", err)
	}
	return nil
}
