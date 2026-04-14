// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cenvero/fleet/pkg/proto"
)

type MetricsQueue interface {
	Enqueue(proto.MetricsSnapshot) error
	Flush() ([]proto.MetricsSnapshot, error)
}

type fileMetricsQueue struct {
	path string
	mu   sync.Mutex
}

func NewFileMetricsQueue(path string) MetricsQueue {
	if path == "" {
		return noopMetricsQueue{}
	}
	return &fileMetricsQueue{path: path}
}

type noopMetricsQueue struct{}

func (noopMetricsQueue) Enqueue(proto.MetricsSnapshot) error {
	return nil
}

func (noopMetricsQueue) Flush() ([]proto.MetricsSnapshot, error) {
	return nil, nil
}

func (q *fileMetricsQueue) Enqueue(snapshot proto.MetricsSnapshot) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
		return fmt.Errorf("create metrics queue directory: %w", err)
	}
	file, err := os.OpenFile(q.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open metrics queue: %w", err)
	}
	defer file.Close()

	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal queued metrics: %w", err)
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("append queued metrics: %w", err)
	}
	return nil
}

func (q *fileMetricsQueue) Flush() ([]proto.MetricsSnapshot, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	file, err := os.Open(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open metrics queue: %w", err)
	}

	var snapshots []proto.MetricsSnapshot
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var snapshot proto.MetricsSnapshot
		if err := json.Unmarshal(scanner.Bytes(), &snapshot); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("decode queued metrics: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := scanner.Err(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("scan metrics queue: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close metrics queue: %w", err)
	}
	if err := os.Remove(q.path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clear metrics queue: %w", err)
	}
	return snapshots, nil
}
