// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

func TestMetricsQueuePeekRequiresAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	queue := NewFileMetricsQueue(path).(*fileMetricsQueue)
	first := proto.MetricsSnapshot{Timestamp: time.Unix(1, 0).UTC(), ProcessCount: 1}
	if err := queue.Enqueue(first); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	batch, err := queue.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if batch.ID == "" || len(batch.Snapshots) != 1 {
		t.Fatalf("unexpected batch: %#v", batch)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Fatalf("Peek removed queue data: data=%q err=%v", data, err)
	}

	repeated, err := queue.Peek()
	if err != nil {
		t.Fatalf("second Peek: %v", err)
	}
	if repeated.ID != batch.ID || len(repeated.Snapshots) != 1 {
		t.Fatalf("second Peek returned a different in-flight batch: %#v", repeated)
	}
	if err := queue.Acknowledge(batch.ID); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
		t.Fatalf("acknowledged queue = %q, err=%v; want empty", data, err)
	}
}

func TestMetricsQueueAcknowledgementPreservesConcurrentAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	queue := NewFileMetricsQueue(path).(*fileMetricsQueue)
	first := proto.MetricsSnapshot{Timestamp: time.Unix(1, 0).UTC(), ProcessCount: 1}
	second := proto.MetricsSnapshot{Timestamp: time.Unix(2, 0).UTC(), ProcessCount: 2}
	if err := queue.Enqueue(first); err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	batch, err := queue.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if err := queue.Enqueue(second); err != nil {
		t.Fatalf("Enqueue second: %v", err)
	}
	if err := queue.Acknowledge(batch.ID); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	next, err := queue.Peek()
	if err != nil {
		t.Fatalf("Peek next: %v", err)
	}
	if len(next.Snapshots) != 1 || next.Snapshots[0].ProcessCount != 2 {
		t.Fatalf("next batch = %#v, want only concurrent append", next)
	}
}

func TestMetricsQueueFailedAcknowledgementKeepsBatchPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	queue := NewFileMetricsQueue(path).(*fileMetricsQueue)
	if err := queue.Enqueue(proto.MetricsSnapshot{Timestamp: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	batch, err := queue.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}

	injected := errors.New("injected rename failure")
	queue.rename = func(string, string) error { return injected }
	if err := queue.Acknowledge(batch.ID); !errors.Is(err, injected) {
		t.Fatalf("Acknowledge error = %v, want injected failure", err)
	}
	retry, err := queue.Peek()
	if err != nil {
		t.Fatalf("Peek after failed acknowledgement: %v", err)
	}
	if retry.ID != batch.ID || len(retry.Snapshots) != 1 {
		t.Fatalf("failed acknowledgement lost or changed batch: %#v", retry)
	}

	queue.rename = os.Rename
	if err := queue.Acknowledge(batch.ID); err != nil {
		t.Fatalf("retry Acknowledge: %v", err)
	}
}

func TestMetricsQueueRejectsWrongAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	queue := NewFileMetricsQueue(path).(*fileMetricsQueue)
	if err := queue.Enqueue(proto.MetricsSnapshot{Timestamp: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := queue.Peek(); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if err := queue.Acknowledge("wrong-batch"); err == nil {
		t.Fatal("wrong batch acknowledgement succeeded")
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Fatalf("wrong acknowledgement removed data: data=%q err=%v", data, err)
	}
}

func TestMetricsQueueSyncsParentAfterCreationAndReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")
	queue := NewFileMetricsQueue(path).(*fileMetricsQueue)
	var synced []string
	fileSyncs := 0
	queue.syncFile = func(file *os.File) error {
		fileSyncs++
		return file.Sync()
	}
	queue.syncDir = func(path string) error {
		synced = append(synced, path)
		return nil
	}

	if err := queue.Enqueue(proto.MetricsSnapshot{Timestamp: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	if fileSyncs != 1 {
		t.Fatalf("file syncs after creation = %d, want 1", fileSyncs)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("creation directory syncs = %#v, want [%q]", synced, dir)
	}
	if err := queue.Enqueue(proto.MetricsSnapshot{Timestamp: time.Unix(2, 0).UTC()}); err != nil {
		t.Fatalf("Enqueue second: %v", err)
	}
	if fileSyncs != 2 {
		t.Fatalf("file syncs after append = %d, want 2", fileSyncs)
	}
	if len(synced) != 1 {
		t.Fatalf("append unexpectedly resynced existing directory: %#v", synced)
	}
	batch, err := queue.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if err := queue.Acknowledge(batch.ID); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if fileSyncs != 3 {
		t.Fatalf("file syncs after replacement = %d, want 3", fileSyncs)
	}
	if len(synced) != 2 || synced[1] != dir {
		t.Fatalf("replacement directory syncs = %#v, want creation and replacement", synced)
	}
}

func TestMetricsQueueCreationSyncFailureIsReportedWithDataRetained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	queue := NewFileMetricsQueue(path).(*fileMetricsQueue)
	injected := errors.New("injected directory fsync failure")
	queue.syncDir = func(string) error { return injected }
	if err := queue.Enqueue(proto.MetricsSnapshot{Timestamp: time.Unix(1, 0).UTC(), ProcessCount: 9}); !errors.Is(err, injected) {
		t.Fatalf("Enqueue error = %v, want injected fsync failure", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("fsync failure lost queue data: data=%q err=%v", data, err)
	}
	queue.syncDir = syncMetricsQueueDir
	batch, err := queue.Peek()
	if err != nil {
		t.Fatalf("Peek retained data: %v", err)
	}
	if len(batch.Snapshots) != 1 || batch.Snapshots[0].ProcessCount != 9 {
		t.Fatalf("retained batch = %#v", batch)
	}
}
