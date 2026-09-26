// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/pkg/proto"
)

func metricsReply(server string, cpu float64) proto.Envelope {
	return proto.Envelope{Payload: proto.MetricsSnapshot{Timestamp: time.Now().UTC(), Hostname: server, CPUPercent: cpu, MemoryPercent: 30, DiskPercent: 40}}
}

// TestCollectMetricsCycleIsParallelAndBounded: the poller used to collect one
// server at a time.
func TestCollectMetricsCycleIsParallelAndBounded(t *testing.T) {
	t.Parallel()
	app := newFanoutTestApp(t, 48)
	var inflight, maxInflight atomic.Int32
	app.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			prev := maxInflight.Load()
			if cur <= prev || maxInflight.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		return metricsReply(server, 10), nil
	}
	start := time.Now()
	app.collectMetricsCycle()
	elapsed := time.Since(start)
	if got := maxInflight.Load(); got > metricsPollParallelism || got < 2 {
		t.Fatalf("max in flight = %d, want 2..%d", got, metricsPollParallelism)
	}
	// Sequentially this is 48*40ms ≈ 1.9s.
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("cycle took %s", elapsed)
	}
	if entries, err := app.MetricsDB.ListMetricSnapshots("", 0); err != nil || len(entries) != 48 {
		t.Fatalf("stored %d snapshots (%v), want 48", len(entries), err)
	}
}

// TestCollectMetricsCycleHungServerTimesOut: one agent that never answers must
// not stall the cycle; its failure is audited in server order with the others.
func TestCollectMetricsCycleHungServerTimesOut(t *testing.T) {
	orig := metricsCollectTimeout
	metricsCollectTimeout = 200 * time.Millisecond
	defer func() { metricsCollectTimeout = orig }()

	app := newFanoutTestApp(t, 5)
	hung := fanoutServerName(2)
	app.ReverseRPCContext = func(ctx context.Context, server string, env proto.Envelope) (proto.Envelope, error) {
		if server == hung {
			<-ctx.Done()
			return proto.Envelope{}, ctx.Err()
		}
		if server == fanoutServerName(4) {
			return proto.Envelope{}, errors.New("agent exploded")
		}
		return metricsReply(server, 10), nil
	}
	done := make(chan struct{})
	go func() { app.collectMetricsCycle(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung agent stalled the poll cycle")
	}
	entries, err := app.AuditLog.Tail(1)
	if err != nil || len(entries) != 1 || entries[0].Action != "metrics.poll.failed" {
		t.Fatalf("poll failure not audited: %+v %v", entries, err)
	}
	details := entries[0].Details
	if !strings.Contains(details, hung+": context deadline exceeded") || strings.Index(details, hung) > strings.Index(details, fanoutServerName(4)) {
		t.Fatalf("failure details not in server order / missing timeout: %q", details)
	}
}

func alertFileState(t *testing.T, app *App, id string) (os.FileInfo, alerts.Alert) {
	t.Helper()
	info, err := os.Stat(filepath.Join(app.ConfigDir, "alerts", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	alert, err := app.Alerts.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return info, alert
}

// TestObserveAlertSkipsUnchangedRewrites: a firing alert whose state did not
// change used to be rewritten (and fsynced) on every poll.
func TestObserveAlertSkipsUnchangedRewrites(t *testing.T) {
	t.Parallel()
	app := newFanoutTestApp(t, 0)
	app.Notifier = nil
	alert := alerts.Alert{ID: "metrics-web-cpu-warning", Code: "metrics.cpu.warning", Server: "web", Severity: alerts.SeverityWarning, Message: "CPU usage is 85.0% on web"}
	if err := app.observeAlert(alert); err != nil {
		t.Fatal(err)
	}
	info1, saved1 := alertFileState(t, app, alert.ID)
	for i := 0; i < 5; i++ {
		if err := app.observeAlert(alert); err != nil {
			t.Fatal(err)
		}
	}
	info2, saved2 := alertFileState(t, app, alert.ID)
	if !os.SameFile(info1, info2) || !info1.ModTime().Equal(info2.ModTime()) || saved2.Occurrences != saved1.Occurrences {
		t.Fatal("unchanged alert was rewritten on every observation")
	}

	// Once the touch interval has passed, the next observation persists and
	// folds in the occurrences counted meanwhile (1 + 5 + 1).
	saved2.UpdatedAt = time.Now().Add(-alertTouchInterval - time.Second)
	if err := app.Alerts.Save(saved2); err != nil {
		t.Fatal(err)
	}
	if err := app.observeAlert(alert); err != nil {
		t.Fatal(err)
	}
	if _, saved3 := alertFileState(t, app, alert.ID); saved3.Occurrences != 7 || time.Since(saved3.UpdatedAt) > time.Minute {
		t.Fatalf("touch did not fold occurrences / refresh updated_at: %+v", saved3)
	}

	// A changed message is written immediately.
	changed := alert
	changed.Message = "CPU usage is 88.0% on web"
	if err := app.observeAlert(changed); err != nil {
		t.Fatal(err)
	}
	if _, saved4 := alertFileState(t, app, alert.ID); saved4.Message != changed.Message || saved4.Occurrences != 8 {
		t.Fatalf("changed alert not persisted immediately: %+v", saved4)
	}
}

// TestObserveAlertKeepsNotificationCooldown: a still-firing critical alert whose
// reminder is due is persisted and notified right away.
func TestObserveAlertKeepsNotificationCooldown(t *testing.T) {
	t.Parallel()
	app := newFanoutTestApp(t, 0)
	app.Config.Runtime.AlertNotifyCooldown = "1h"
	notifier := &fakeNotifier{}
	app.Notifier = notifier
	alert := alerts.Alert{ID: "cpu-hot", Code: "metrics.cpu.critical", Server: "web", Severity: alerts.SeverityCritical, Message: "hot"}
	for i := 0; i < 3; i++ {
		if err := app.observeAlert(alert); err != nil {
			t.Fatal(err)
		}
	}
	if notifier.Count() != 1 {
		t.Fatalf("notifications = %d, want 1 within the cooldown", notifier.Count())
	}
	saved, _ := app.Alerts.Get(alert.ID)
	past := time.Now().Add(-2 * time.Hour)
	saved.LastNotifiedAt = &past
	if err := app.Alerts.Save(saved); err != nil {
		t.Fatal(err)
	}
	if err := app.observeAlert(alert); err != nil {
		t.Fatal(err)
	}
	if notifier.Count() != 2 {
		t.Fatalf("reminder after the cooldown not sent (count %d)", notifier.Count())
	}
}

// TestFireNotifyIsAsyncAndCloseFlushes: webhook delivery (8s timeout per
// target) ran synchronously inside the metrics poller; now fireNotify returns
// at once, events are delivered in order, and Close delivers what is queued.
func TestFireNotifyIsAsyncAndCloseFlushes(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		mu.Lock()
		got = append(got, buf.String())
		mu.Unlock()
	}))
	defer srv.Close()

	app := newFanoutTestApp(t, 0)
	if err := NewNotifyStore(app.ConfigDir).Add(NotifyTarget{Kind: NotifyKindWebhook, URL: srv.URL, Events: []string{NotifyEventOffline, NotifyEventOnline}, AllowInternal: true}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 3; i++ {
		app.fireNotify(NotifyEventOffline, fmt.Sprintf("event-%d", i))
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("fireNotify blocked the caller for %s", d)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("delivered %d notifications before Close returned, want 3", len(got))
	}
	for i, body := range got {
		if !strings.Contains(body, fmt.Sprintf("event-%d", i)) {
			t.Fatalf("delivery %d out of order: %s", i, body)
		}
	}
}

func TestNotifyDispatcherDropsOldestWhenFull(t *testing.T) {
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	origLog := notifyLog
	notifyLog = writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return logBuf.Write(p) })
	defer func() { notifyLog = origLog }()

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var delivered []string
	var mu sync.Mutex
	d := newNotifyDispatcher(func(job notifyJob) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		delivered = append(delivered, job.message)
		mu.Unlock()
	})
	total := notifyQueueCapacity + 6 // one goes in flight, then the queue overflows
	d.enqueue(notifyJob{event: "offline", message: "0", at: time.Now()})
	<-started // job 0 is in flight
	for i := 1; i < total; i++ {
		d.enqueue(notifyJob{event: "offline", message: fmt.Sprint(i), at: time.Now()})
	}
	close(release)
	if !d.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != notifyQueueCapacity+1 {
		t.Fatalf("delivered %d, want %d (in-flight + full queue)", len(delivered), notifyQueueCapacity+1)
	}
	if delivered[len(delivered)-1] != fmt.Sprint(total-1) {
		t.Fatalf("newest notification was not kept: last delivered %s", delivered[len(delivered)-1])
	}
	logMu.Lock()
	defer logMu.Unlock()
	if strings.Count(logBuf.String(), "dropped the oldest") != 5 {
		t.Fatalf("drop log lines:\n%s", logBuf.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestPruneMetricHistoryRemovesOldSnapshots covers the daemon's retention step.
func TestPruneMetricHistoryRemovesOldSnapshots(t *testing.T) {
	t.Parallel()
	app := newFanoutTestApp(t, 0)
	now := time.Now().UTC()
	for i, age := range []time.Duration{40 * 24 * time.Hour, 31 * 24 * time.Hour, time.Hour} {
		if err := app.MetricsDB.RecordMetricSnapshot("latest.web", "web", now.Add(-age), fmt.Sprintf(`{"i":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	app.pruneMetricHistory(context.Background())
	rows, err := app.MetricsDB.ListMetricSnapshots("web", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("remaining snapshots = %d (%v), want 1", len(rows), err)
	}
	entries, _ := app.AuditLog.Tail(1)
	if len(entries) != 1 || entries[0].Action != "metrics.retention.pruned" || !strings.Contains(entries[0].Details, "removed 2") {
		t.Fatalf("retention not audited: %+v", entries)
	}
}
