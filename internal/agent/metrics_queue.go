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
	// More reports that further snapshots are queued after this batch.
	More bool
}

type MetricsQueue interface {
	Enqueue(proto.MetricsSnapshot) error
	Peek() (MetricsBatch, error)
	Acknowledge(batchID string) error
}

// MetricsPager is implemented by queues that can hand out their backlog in
// bounded pages. Peek returns the whole queue as one batch, which after a long
// outage did not fit in one protocol message (16 MiB, ~43k snapshots): the
// encode failed, the error was dropped, and the backlog never replayed.
type MetricsPager interface {
	PeekPage(maxSnapshots int) (MetricsBatch, error)
}

const (
	// maxQueuedMetricsSnapshots and maxQueuedMetricsBytes cap the offline
	// queue (~7 days at the default one-minute interval). When a new snapshot
	// pushes the queue past either cap, the older half is thinned to every
	// other snapshot, so a long outage keeps coverage of its whole span at
	// progressively coarser resolution instead of growing without bound.
	maxQueuedMetricsSnapshots = 10000
	maxQueuedMetricsBytes     = 4 << 20
)

type metricsInFlight struct {
	id   string
	size int
}

type fileMetricsQueue struct {
	path string
	mu   sync.Mutex

	inFlight *metricsInFlight
	// lastAcked is the most recently acknowledged batch, so a repeated
	// acknowledgement of it (a retry whose first answer was lost) succeeds
	// without removing anything else.
	lastAcked string
	// count is the number of queued snapshots, or -1 when not yet known.
	count int
	// maxSnapshots and maxBytes are the queue caps; tests lower them.
	maxSnapshots int
	maxBytes     int
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
		count:        -1,
		maxSnapshots: maxQueuedMetricsSnapshots,
		maxBytes:     maxQueuedMetricsBytes,
		syncFile:     func(file *os.File) error { return file.Sync() },
		syncDir:      syncMetricsQueueDir,
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
	if q.count >= 0 {
		q.count++
	}
	return q.enforceCapLocked()
}

// enforceCapLocked thins the queue once it exceeds its caps: the older half is
// reduced to every other snapshot, repeatedly if needed. Older entries end up
// progressively sparser, so the queue keeps covering the whole outage within a
// fixed size. q.mu must be held.
func (q *fileMetricsQueue) enforceCapLocked() error {
	if q.maxSnapshots <= 0 && q.maxBytes <= 0 {
		return nil
	}
	info, err := os.Stat(q.path)
	if err != nil {
		return nil // nothing queued, or it cannot be inspected: nothing to cap
	}
	overBytes := q.maxBytes > 0 && info.Size() > int64(q.maxBytes)
	if !overBytes && q.count >= 0 && (q.maxSnapshots <= 0 || q.count <= q.maxSnapshots) {
		return nil
	}
	data, err := os.ReadFile(q.path)
	if err != nil {
		return fmt.Errorf("read metrics queue: %w", err)
	}
	lines := splitMetricsLines(data)
	q.count = len(lines)
	if !overBytes && (q.maxSnapshots <= 0 || len(lines) <= q.maxSnapshots) {
		return nil
	}
	thinned := thinMetricsLines(lines, q.maxSnapshots, q.maxBytes)
	if err := q.replace(bytes.Join(thinned, nil)); err != nil {
		return fmt.Errorf("compact metrics queue: %w", err)
	}
	// Any batch handed out before now named a prefix that no longer exists.
	// Its acknowledgement will be refused; the controller's save is
	// idempotent, so re-sending those snapshots later duplicates nothing.
	q.inFlight = nil
	q.count = len(thinned)
	return nil
}

// splitMetricsLines splits queue data into lines, each keeping its newline. A
// final line without one (a torn append) is kept as is.
func splitMetricsLines(data []byte) [][]byte {
	var lines [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			lines = append(lines, data)
			break
		}
		lines = append(lines, data[:i+1])
		data = data[i+1:]
	}
	return lines
}

// thinMetricsLines drops every other line from the older half until the lines
// fit both caps (maxLines, maxBytes; zero means no cap). The newest half is
// never thinned in a pass, and the oldest line is always kept.
func thinMetricsLines(lines [][]byte, maxLines, maxBytes int) [][]byte {
	size := func(ls [][]byte) int {
		n := 0
		for _, l := range ls {
			n += len(l)
		}
		return n
	}
	for len(lines) > 1 && ((maxLines > 0 && len(lines) > maxLines) || (maxBytes > 0 && size(lines) > maxBytes)) {
		older := len(lines) / 2
		if older < 2 {
			// Too few lines to thin (only a byte cap can get here): drop the oldest.
			lines = lines[1:]
			continue
		}
		kept := make([][]byte, 0, len(lines)-older/2)
		for i := 0; i < older; i += 2 {
			kept = append(kept, lines[i])
		}
		lines = append(kept, lines[older:]...)
	}
	return lines
}

// metricsPrefixLen returns the length of the first n lines of data (all of it
// when it holds fewer).
func metricsPrefixLen(data []byte, n int) int {
	if n <= 0 {
		return len(data)
	}
	offset := 0
	for i := 0; i < n; i++ {
		j := bytes.IndexByte(data[offset:], '\n')
		if j < 0 {
			return len(data)
		}
		offset += j + 1
	}
	return offset
}

// Peek returns a stable in-flight prefix without deleting it. New Enqueue calls
// may append records while a batch is in flight; repeated Peek calls return the
// same prefix until it is acknowledged.
func (q *fileMetricsQueue) Peek() (MetricsBatch, error) {
	return q.PeekPage(0)
}

// PeekPage is Peek limited to the first maxSnapshots queued snapshots (the
// whole queue when maxSnapshots is zero). While a batch is in flight the same
// batch is returned whatever the limit, until it is acknowledged.
func (q *fileMetricsQueue) PeekPage(maxSnapshots int) (MetricsBatch, error) {
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
		prefix = data[:metricsPrefixLen(data, maxSnapshots)]
		q.inFlight = &metricsInFlight{id: metricsBatchID(prefix), size: len(prefix)}
	}

	snapshots, err := decodeMetricsSnapshots(prefix)
	if err != nil {
		return MetricsBatch{}, err
	}
	return MetricsBatch{ID: q.inFlight.id, Snapshots: snapshots, More: len(data) > len(prefix)}, nil
}

// Acknowledge atomically removes only the prefix returned by Peek. Appends that
// happened while the batch was in flight remain queued for the next replay.
// Acknowledging the batch that was just acknowledged again succeeds without
// removing anything, so a controller may safely retry an acknowledgement whose
// answer it never received.
func (q *fileMetricsQueue) Acknowledge(batchID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if batchID != "" && batchID == q.lastAcked && (q.inFlight == nil || q.inFlight.id != batchID) {
		return nil
	}
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
	q.lastAcked = q.inFlight.id
	q.inFlight = nil
	q.count = -1 // recounted lazily when the cap is next checked
	return nil
}

// peekMetricsQueue serves metrics.peek_queue: one page when the controller
// asks for a bounded batch and the queue supports paging, else the whole queue
// as older controllers expect.
func (s Server) peekMetricsQueue(payload any) (MetricsBatch, error) {
	queue := s.metricsQueue()
	request, err := proto.DecodePayload[proto.MetricsPeekPayload](payload)
	if err == nil && request.MaxSnapshots > 0 {
		if pager, ok := queue.(MetricsPager); ok {
			return pager.PeekPage(request.MaxSnapshots)
		}
	}
	return queue.Peek()
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
