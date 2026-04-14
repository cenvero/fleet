// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

func TestServiceStoreAppendDedupesAndReads(t *testing.T) {
	t.Parallel()

	store := NewServiceStore(filepath.Join(t.TempDir(), "_aggregated"), 1024, 3, time.Hour)
	if err := store.Append("web-01", "nginx.service", []proto.LogLine{
		{Number: 10, Text: "first"},
		{Number: 11, Text: "second"},
	}); err != nil {
		t.Fatalf("Append(first) error = %v", err)
	}
	if err := store.Append("web-01", "nginx.service", []proto.LogLine{
		{Number: 10, Text: "first"},
		{Number: 11, Text: "second"},
		{Number: 12, Text: "third"},
	}); err != nil {
		t.Fatalf("Append(second) error = %v", err)
	}

	result, err := store.Read("web-01", "nginx.service", "", 10)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Lines) != 3 {
		t.Fatalf("expected 3 cached lines, got %d", len(result.Lines))
	}
	if result.Lines[2].Text != "third" {
		t.Fatalf("unexpected cached lines: %#v", result.Lines)
	}
}

func TestServiceStoreRotationRetainsBackups(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "_aggregated")
	store := NewServiceStore(root, 10, 2, time.Hour)

	appendBatch := func(number int, text string) {
		if err := store.Append("web-01", "nginx.service", []proto.LogLine{{Number: number, Text: text}}); err != nil {
			t.Fatalf("Append(%d) error = %v", number, err)
		}
	}

	appendBatch(1, "12345678901")
	appendBatch(2, "abcdefghijk")
	appendBatch(3, "ABCDEFGHIJK")

	if _, err := store.Read("web-01", "nginx.service", "", 20); err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	basePath := filepath.Join(root, "web-01", "nginx.service.log")
	if _, err := os.Stat(basePath + ".1"); err != nil {
		t.Fatalf("expected first rotated log to exist: %v", err)
	}
	if _, err := os.Stat(basePath + ".2"); err != nil {
		t.Fatalf("expected second rotated log to exist: %v", err)
	}
	if _, err := os.Stat(basePath + ".3"); !os.IsNotExist(err) {
		t.Fatalf("expected only two rotated backups, got err=%v", err)
	}
}

func TestServiceStoreReadExpiresStaleCacheByAge(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "_aggregated")
	store := NewServiceStore(root, 1024, 3, time.Hour)

	if err := store.Append("web-01", "nginx.service", []proto.LogLine{
		{Number: 1, Text: "first"},
		{Number: 2, Text: "second"},
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	basePath := filepath.Join(root, "web-01", "nginx.service.log")
	cursorPath := filepath.Join(root, "web-01", "nginx.service.cursor.json")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(basePath, old, old); err != nil {
		t.Fatalf("Chtimes(basePath) error = %v", err)
	}
	if err := os.Chtimes(cursorPath, old, old); err != nil {
		t.Fatalf("Chtimes(cursorPath) error = %v", err)
	}

	result, err := store.Read("web-01", "nginx.service", "", 20)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(result.Lines) != 0 {
		t.Fatalf("expected stale cache to expire, got %d cached lines", len(result.Lines))
	}
	if _, err := os.Stat(basePath); !os.IsNotExist(err) {
		t.Fatalf("expected expired base log to be removed, got err=%v", err)
	}
	if _, err := os.Stat(cursorPath); !os.IsNotExist(err) {
		t.Fatalf("expected expired cursor to be removed, got err=%v", err)
	}
}
