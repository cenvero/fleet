// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package store

import (
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AppendMetricSnapshots stores a batch of snapshots for one server in a single
// transaction and returns how many rows it inserted.
//
// It exists for the reverse-mode offline replay, which used to save each queued
// snapshot with its own INSERT plus a latest-state upsert — two commits (and on
// SQLite two fsyncs) per snapshot — so a day's backlog took seconds just in
// commits. One transaction per batch commits once.
//
// It is also idempotent: a snapshot whose exact payload is already stored for
// the server is skipped. A replayed batch is only removed from the agent's queue
// after the controller acknowledges it, so when that acknowledgement is lost
// (the connection drops between the commit and the ack) the agent re-sends the
// same batch on the next connection; this is what keeps that from duplicating
// history.
//
// latest, when non-empty, becomes the server's latest-snapshot state in the
// same transaction; the caller decides whether the batch is actually newer.
func (s *Store) AppendMetricSnapshots(server string, entries []MetricSnapshotEntry, latest string) (int, error) {
	if s.workload != WorkloadMetrics {
		return 0, fmt.Errorf("append metric snapshots is unsupported for workload %q", s.workload)
	}
	if len(entries) == 0 && latest == "" {
		return 0, nil
	}
	// Stores open lazily: connect (and migrate) on first use rather than
	// assuming s.db is already set.
	db, err := s.conn()
	if err != nil {
		return 0, err
	}
	inserted := 0
	err = db.Transaction(func(tx *gorm.DB) error {
		rows := make([]metricsSnapshotRow, 0, len(entries))
		if len(entries) > 0 {
			minTS, maxTS := entries[0].Timestamp.UTC(), entries[0].Timestamp.UTC()
			for _, entry := range entries[1:] {
				ts := entry.Timestamp.UTC()
				if ts.Before(minTS) {
					minTS = ts
				}
				if ts.After(maxTS) {
					maxTS = ts
				}
			}
			// Look for rows already stored in the batch's time window. The window
			// is padded because some backends store timestamps at lower precision
			// than Go's nanoseconds; the payload comparison is what decides.
			// Column names go through clause.Column so each dialect quotes them
			// ("timestamp" is a keyword in several).
			var existing []string
			if err := tx.Model(&metricsSnapshotRow{}).
				Where(clause.Eq{Column: clause.Column{Name: "server"}, Value: server}).
				Where(clause.Gte{Column: clause.Column{Name: "timestamp"}, Value: minTS.Add(-time.Second)}).
				Where(clause.Lte{Column: clause.Column{Name: "timestamp"}, Value: maxTS.Add(time.Second)}).
				Pluck("payload", &existing).Error; err != nil {
				return err
			}
			seen := make(map[string]struct{}, len(existing)+len(entries))
			for _, payload := range existing {
				seen[payload] = struct{}{}
			}
			for _, entry := range entries {
				if _, dup := seen[entry.Payload]; dup {
					continue
				}
				seen[entry.Payload] = struct{}{}
				rows = append(rows, metricsSnapshotRow{
					Server:    server,
					Timestamp: entry.Timestamp.UTC(),
					Payload:   entry.Payload,
				})
			}
			if len(rows) > 0 {
				if err := tx.CreateInBatches(rows, 200).Error; err != nil {
					return err
				}
			}
		}
		if latest != "" {
			row := metricsRow{Key: "latest." + server, Value: latest}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		inserted = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}
