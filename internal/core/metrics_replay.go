// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

const (
	// metricsReplayPageSize is how many queued snapshots the controller asks a
	// reverse agent for per batch (~350 bytes each, so ~175 KiB per message).
	// Each page is persisted in one transaction and acknowledged on its own, so
	// a long backlog never has to fit in one envelope or one timeout.
	metricsReplayPageSize = 500
	// metricsReplayMaxPages bounds one connection's replay. The agent caps its
	// queue far below this, so it only stops a misbehaving peer from looping.
	metricsReplayMaxPages = 200
	// metricsReplayPageTimeout bounds each page's peek + persist + ack.
	metricsReplayPageTimeout = 30 * time.Second
	// metricsReplayMaxSnapshots bounds what one connection may replay in all.
	// An agent queues at most 10,000 snapshots (an older agent returns them as
	// one batch), so this only stops a hostile peer from writing an unbounded
	// history into the controller's database on every reconnect.
	metricsReplayMaxSnapshots = 20_000
)

// replayQueuedMetrics drains a reverse agent's offline metrics queue: peek a
// page, persist it, acknowledge it, repeat while the agent reports more. It
// returns how many snapshots were replayed.
//
// Paging is negotiated by the request alone: an agent that understands
// MetricsPeekPayload.MaxSnapshots answers with one page and sets More; an older
// agent ignores the field and returns its whole queue with More unset, which
// is handled as a single (large) page exactly as before.
func (h *ReverseHub) replayQueuedMetrics(serverName string, session *transport.Session, capabilities []string) (int, error) {
	// Older agents implemented metrics.flush_queue destructively. Never probe it:
	// absence of the explicit peek/ack capability means queued data stays on the
	// agent until it is upgraded.
	if !slices.Contains(capabilities, proto.CapabilityMetricsPeekAck) {
		return 0, nil
	}
	call := func(ctx context.Context, env proto.Envelope) (proto.Envelope, error) {
		return h.callOn(ctx, serverName, session, env)
	}

	total := 0
	var newest proto.MetricsSnapshot
	lastBatch := ""
	var replayErr error
	for page := 0; page < metricsReplayMaxPages && total < metricsReplayMaxSnapshots; page++ {
		n, batchID, pageNewest, more, err := h.replayMetricsPage(serverName, call, lastBatch)
		if err != nil {
			replayErr = err
			break
		}
		if n == 0 {
			break
		}
		total += n
		lastBatch = batchID
		if newest.Timestamp.IsZero() || pageNewest.Timestamp.After(newest.Timestamp) {
			newest = pageNewest
		}
		if !more {
			break
		}
	}
	if total > 0 {
		if err := h.finishMetricsReplay(serverName, newest, total); err != nil && replayErr == nil {
			replayErr = err
		}
	}
	return total, replayErr
}

// replayMetricsPage replays one batch. It returns the number of snapshots in
// it (0 when the queue is empty), the batch ID, the newest snapshot, and
// whether the agent has more queued after it.
func (h *ReverseHub) replayMetricsPage(serverName string, call func(context.Context, proto.Envelope) (proto.Envelope, error), previousBatch string) (int, string, proto.MetricsSnapshot, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), metricsReplayPageTimeout)
	defer cancel()
	response, err := call(ctx, proto.Envelope{
		Action:  proto.ActionMetricsPeekQueue,
		Payload: proto.MetricsPeekPayload{Server: serverName, MaxSnapshots: metricsReplayPageSize},
	})
	if err != nil {
		return 0, "", proto.MetricsSnapshot{}, false, err
	}
	if response.Error != nil {
		return 0, "", proto.MetricsSnapshot{}, false, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}
	replay, err := proto.DecodePayload[proto.MetricsReplayResult](response.Payload)
	if err != nil {
		return 0, "", proto.MetricsSnapshot{}, false, err
	}
	if len(replay.Snapshots) == 0 {
		return 0, "", proto.MetricsSnapshot{}, false, nil
	}
	if replay.BatchID == "" {
		return 0, "", proto.MetricsSnapshot{}, false, fmt.Errorf("peek/ack capable agent returned metrics without a batch id")
	}
	if len(replay.Snapshots) > metricsReplayMaxSnapshots {
		return 0, "", proto.MetricsSnapshot{}, false, fmt.Errorf("agent returned %d queued metrics snapshots in one batch; refusing more than %d", len(replay.Snapshots), metricsReplayMaxSnapshots)
	}
	if replay.BatchID == previousBatch {
		// The agent handed back the batch it just acknowledged; stop rather
		// than loop on it.
		return 0, "", proto.MetricsSnapshot{}, false, fmt.Errorf("agent re-sent acknowledged metrics batch %s", replay.BatchID)
	}

	// Persist before acknowledging: the agent deletes the batch only on the
	// ack, so a failure here leaves it queued for the next connection.
	if err := h.app.persistReplayedMetrics(serverName, replay.Snapshots); err != nil {
		return 0, "", proto.MetricsSnapshot{}, false, err
	}
	ack, err := call(ctx, proto.Envelope{
		Action:  proto.ActionMetricsAckQueue,
		Payload: proto.MetricsReplayAck{BatchID: replay.BatchID},
	})
	if err == nil && ack.Error != nil {
		err = fmt.Errorf("%s: %s", ack.Error.Code, ack.Error.Message)
	}
	if err != nil {
		// Already persisted: if the agent re-sends this batch, the idempotent
		// save skips what is stored.
		return 0, "", proto.MetricsSnapshot{}, false, fmt.Errorf("acknowledge replayed metrics: %w", err)
	}
	newest := replay.Snapshots[0]
	for _, snapshot := range replay.Snapshots[1:] {
		if snapshot.Timestamp.After(newest.Timestamp) {
			newest = snapshot
		}
	}
	return len(replay.Snapshots), replay.BatchID, newest, replay.More, nil
}

// finishMetricsReplay applies a completed replay to the server record, alerts
// and audit log, as the single-batch replay always did.
func (h *ReverseHub) finishMetricsReplay(serverName string, newest proto.MetricsSnapshot, total int) error {
	server, err := h.app.GetServer(serverName)
	if err != nil {
		return err
	}
	// The session is already live, so the poller may have stored a fresher
	// snapshot than anything replayed; never move the record backwards.
	if server.Metrics.Timestamp.IsZero() || newest.Timestamp.After(server.Metrics.Timestamp) {
		server.Metrics = newest
	}
	server.Observed.LastSeen = time.Now().UTC()
	server.Observed.LastError = ""
	if err := h.app.SaveServer(server); err != nil {
		return err
	}
	if err := h.app.evaluateMetricAlerts(serverName, server.Metrics); err != nil {
		return err
	}
	if err := h.app.clearCollectionFailureAlert(serverName); err != nil {
		return err
	}
	return h.app.AuditLog.Append(logs.AuditEntry{
		Action:   "metrics.replay",
		Target:   serverName,
		Operator: h.app.operator(),
		Details:  fmt.Sprintf("snapshots=%d", total),
	})
}

// persistReplayedMetrics stores one replayed batch in a single transaction.
// Snapshots already stored (a batch re-sent because its acknowledgement was
// lost) are skipped, and the latest-snapshot state only moves forward.
func (a *App) persistReplayedMetrics(serverName string, snapshots []proto.MetricsSnapshot) error {
	if a.MetricsDB == nil || len(snapshots) == 0 {
		return nil
	}
	entries := make([]store.MetricSnapshotEntry, 0, len(snapshots))
	newest := -1
	for i, snapshot := range snapshots {
		snapshot = boundSnapshotText(snapshot)
		snapshots[i] = snapshot
		data, err := json.Marshal(snapshot)
		if err != nil {
			return fmt.Errorf("marshal metrics snapshot: %w", err)
		}
		entries = append(entries, store.MetricSnapshotEntry{Server: serverName, Timestamp: snapshot.Timestamp, Payload: string(data)})
		if newest < 0 || snapshot.Timestamp.After(snapshots[newest].Timestamp) {
			newest = i
		}
	}
	latest := entries[newest].Payload
	// A missing (or unreadable) latest state is simply replaced; a broken
	// database fails the append below.
	if current, err := a.MetricsDB.GetState("latest." + serverName); err == nil {
		var stored proto.MetricsSnapshot
		if json.Unmarshal([]byte(current), &stored) == nil && !snapshots[newest].Timestamp.After(stored.Timestamp) {
			latest = "" // what is stored is at least as new
		}
	}
	_, err := a.MetricsDB.AppendMetricSnapshots(serverName, entries, latest)
	return err
}
