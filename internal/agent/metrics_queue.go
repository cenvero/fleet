// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/cenvero/fleet/pkg/proto"
)

// MetricsBatch is an in-flight prefix of the offline queue. Records remain on
// disk until Acknowledge is called with the matching ID.
type MetricsBatch struct {
	ID        string
	Snapshots []proto.MetricsSnapshot
}

type MetricsQueue interface {
	Enqueue(proto.MetricsSnapshot) error
	Peek() (MetricsBatch, error)
	Acknowledge(batchID string) error
}

type metricsInFlight struct {
	id   string
	size int
}

type fileMetricsQueue struct {
	path string
	mu   sync.Mutex

	inFlight *metricsInFlight
	// rename, syncFile, and syncDir are injected by fault/durability tests.
	rename   func(string, string) error
	syncFile func(*os.File) error
	syncDir  func(string) error
}

func NewFileMetricsQueue(path string) MetricsQueue {
	if path == "" {
		return noopMetricsQueue{}
	}
	return &fileMetricsQueue{
		path: path, rename: os.Rename,
		syncFile: func(file *os.File) error { return file.Sync() },
		syncDir:  syncMetricsQueueDir,
	}
}

type noopMetricsQueue struct{}

func (noopMetricsQueue) Enqueue(proto.MetricsSnapshot) error {
	return nil
}

func (noopMetricsQueue) Peek() (MetricsBatch, error) {
	return MetricsBatch{}, nil
}

func (noopMetricsQueue) Acknowledge(string) error {
	return nil
}

func (q *fileMetricsQueue) Enqueue(snapshot proto.MetricsSnapshot) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	dir := filepath.Dir(q.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create metrics queue directory: %w", err)
	}
	_, statErr := os.Stat(q.path)
	created := os.IsNotExist(statErr)
	if statErr != nil && !created {
		return fmt.Errorf("inspect metrics queue: %w", statErr)
	}
	file, err := os.OpenFile(q.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open metrics queue: %w", err)
	}
	closeWithError := func(opErr error) error {
		if closeErr := file.Close(); opErr == nil && closeErr != nil {
			return fmt.Errorf("close metrics queue: %w", closeErr)
		}
		return opErr
	}
	if err := file.Chmod(0o600); err != nil {
		return closeWithError(fmt.Errorf("secure metrics queue: %w", err))
	}

	payload, err := json.Marshal(snapshot)
	if err != nil {
		return closeWithError(fmt.Errorf("marshal queued metrics: %w", err))
	}
	payload = append(payload, '\n')
	if n, err := file.Write(payload); err != nil {
		return closeWithError(fmt.Errorf("append queued metrics: %w", err))
	} else if n != len(payload) {
		return closeWithError(fmt.Errorf("append queued metrics: %w", io.ErrShortWrite))
	}
	syncFile := q.syncFile
	if syncFile == nil {
		syncFile = func(file *os.File) error { return file.Sync() }
	}
	if err := syncFile(file); err != nil {
		return closeWithError(fmt.Errorf("sync metrics queue: %w", err))
	}
	if err := closeWithError(nil); err != nil {
		return err
	}
	if created {
		syncDir := q.syncDir
		if syncDir == nil {
			syncDir = syncMetricsQueueDir
		}
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("sync metrics queue directory after creation: %w", err)
		}
	}
	return nil
}

// Peek returns a stable in-flight prefix without deleting it. New Enqueue calls
// may append records while a batch is in flight; repeated Peek calls return the
// same prefix until it is acknowledged.
func (q *fileMetricsQueue) Peek() (MetricsBatch, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	data, err := os.ReadFile(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			q.inFlight = nil
			return MetricsBatch{}, nil
		}
		return MetricsBatch{}, fmt.Errorf("read metrics queue: %w", err)
	}
	if len(data) == 0 {
		q.inFlight = nil
		return MetricsBatch{}, nil
	}

	prefix := data
	if q.inFlight != nil {
		if len(data) < q.inFlight.size {
			return MetricsBatch{}, fmt.Errorf("metrics queue changed while batch %s was in flight", q.inFlight.id)
		}
		prefix = data[:q.inFlight.size]
		if metricsBatchID(prefix) != q.inFlight.id {
			return MetricsBatch{}, fmt.Errorf("metrics queue prefix changed while batch %s was in flight", q.inFlight.id)
		}
	} else {
		q.inFlight = &metricsInFlight{id: metricsBatchID(prefix), size: len(prefix)}
	}

	snapshots, err := decodeMetricsSnapshots(prefix)
	if err != nil {
		return MetricsBatch{}, err
	}
	return MetricsBatch{ID: q.inFlight.id, Snapshots: snapshots}, nil
}

// Acknowledge atomically removes only the prefix returned by Peek. Appends that
// happened while the batch was in flight remain queued for the next replay.
func (q *fileMetricsQueue) Acknowledge(batchID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.inFlight == nil || batchID == "" || batchID != q.inFlight.id {
		return fmt.Errorf("metrics batch acknowledgement does not match the in-flight batch")
	}
	data, err := os.ReadFile(q.path)
	if err != nil {
		return fmt.Errorf("read metrics queue for acknowledgement: %w", err)
	}
	if len(data) < q.inFlight.size || metricsBatchID(data[:q.inFlight.size]) != q.inFlight.id {
		return fmt.Errorf("metrics queue prefix changed before acknowledgement")
	}
	remaining := data[q.inFlight.size:]
	if err := q.replace(remaining); err != nil {
		return err
	}
	q.inFlight = nil
	return nil
}

func (q *fileMetricsQueue) replace(data []byte) error {
	dir := filepath.Dir(q.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create metrics queue directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".metrics-queue-*")
	if err != nil {
		return fmt.Errorf("create replacement metrics queue: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure replacement metrics queue: %w", err)
	}
	if n, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write replacement metrics queue: %w", err)
	} else if n != len(data) {
		_ = tmp.Close()
		return fmt.Errorf("write replacement metrics queue: %w", io.ErrShortWrite)
	}
	syncFile := q.syncFile
	if syncFile == nil {
		syncFile = func(file *os.File) error { return file.Sync() }
	}
	if err := syncFile(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync replacement metrics queue: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close replacement metrics queue: %w", err)
	}
	rename := q.rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(tmpPath, q.path); err != nil {
		return fmt.Errorf("replace metrics queue: %w", err)
	}
	syncDir := q.syncDir
	if syncDir == nil {
		syncDir = syncMetricsQueueDir
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync metrics queue directory after replacement: %w", err)
	}
	return nil
}

func syncMetricsQueueDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir) // #nosec G304 -- directory is derived from the configured queue path
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func metricsBatchID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func decodeMetricsSnapshots(data []byte) ([]proto.MetricsSnapshot, error) {
	var snapshots []proto.MetricsSnapshot
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var snapshot proto.MetricsSnapshot
		if err := json.Unmarshal(scanner.Bytes(), &snapshot); err != nil {
			return nil, fmt.Errorf("decode queued metrics: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan metrics queue: %w", err)
	}
	return snapshots, nil
}
