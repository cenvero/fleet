// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

const (
	DefaultAggregatedLogMaxSize  int64 = 5 * 1024 * 1024
	DefaultAggregatedLogMaxFiles       = 5
	DefaultAggregatedLogMaxAge         = 7 * 24 * time.Hour
)

type ServiceStore struct {
	rootDir      string
	maxSizeBytes int64
	maxFiles     int
	maxAge       time.Duration
}

type logCursor struct {
	LastRemoteLine int `json:"last_remote_line"`
}

func NewServiceStore(rootDir string, maxSizeBytes int64, maxFiles int, maxAge time.Duration) *ServiceStore {
	if maxSizeBytes <= 0 {
		maxSizeBytes = DefaultAggregatedLogMaxSize
	}
	if maxFiles <= 0 {
		maxFiles = DefaultAggregatedLogMaxFiles
	}
	if maxAge <= 0 {
		maxAge = DefaultAggregatedLogMaxAge
	}
	return &ServiceStore{
		rootDir:      rootDir,
		maxSizeBytes: maxSizeBytes,
		maxFiles:     maxFiles,
		maxAge:       maxAge,
	}
}

func (s *ServiceStore) Append(serverName, serviceName string, lines []proto.LogLine) error {
	if s == nil || s.rootDir == "" || len(lines) == 0 {
		return nil
	}
	basePath := s.basePath(serverName, serviceName)
	if err := os.MkdirAll(filepath.Dir(basePath), 0o750); err != nil {
		return fmt.Errorf("create aggregated log directory: %w", err)
	}
	if err := s.expireCache(basePath, s.cursorPath(serverName, serviceName)); err != nil {
		return err
	}

	cursor, err := s.readCursor(serverName, serviceName)
	if err != nil {
		return err
	}

	appendLines := append([]proto.LogLine(nil), lines...)
	if last := lines[len(lines)-1].Number; last >= cursor.LastRemoteLine {
		appendLines = make([]proto.LogLine, 0, len(lines))
		for _, line := range lines {
			if line.Number > cursor.LastRemoteLine {
				appendLines = append(appendLines, line)
			}
		}
	}
	if len(appendLines) == 0 {
		return nil
	}

	file, err := os.OpenFile(basePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open aggregated log: %w", err)
	}
	for _, line := range appendLines {
		if _, err := file.WriteString(line.Text + "\n"); err != nil {
			_ = file.Close()
			return fmt.Errorf("append aggregated log: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close aggregated log: %w", err)
	}

	cursor.LastRemoteLine = lines[len(lines)-1].Number
	if err := s.writeCursor(serverName, serviceName, cursor); err != nil {
		return err
	}
	return s.rotateAndPrune(basePath)
}

func (s *ServiceStore) Read(serverName, serviceName, search string, tailLines int) (proto.LogReadResult, error) {
	if s == nil || s.rootDir == "" {
		return proto.LogReadResult{}, fmt.Errorf("aggregated log store is not configured")
	}
	basePath := s.basePath(serverName, serviceName)
	if err := s.expireCache(basePath, s.cursorPath(serverName, serviceName)); err != nil {
		return proto.LogReadResult{}, err
	}
	paths := s.readPaths(basePath)

	search = strings.ToLower(strings.TrimSpace(search))
	lines := make([]proto.LogLine, 0, 128)
	lineNumber := 0
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return proto.LogReadResult{}, fmt.Errorf("open aggregated log %s: %w", path, err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			if search != "" && !strings.Contains(strings.ToLower(line), search) {
				continue
			}
			lines = append(lines, proto.LogLine{
				Number: lineNumber,
				Text:   line,
			})
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return proto.LogReadResult{}, fmt.Errorf("scan aggregated log %s: %w", path, err)
		}
		if err := file.Close(); err != nil {
			return proto.LogReadResult{}, fmt.Errorf("close aggregated log %s: %w", path, err)
		}
	}

	result := proto.LogReadResult{Path: basePath, Lines: lines}
	if tailLines <= 0 {
		tailLines = 200
	}
	if len(result.Lines) > tailLines {
		result.Truncated = true
		result.Lines = append([]proto.LogLine(nil), result.Lines[len(result.Lines)-tailLines:]...)
	}
	return result, nil
}

func (s *ServiceStore) basePath(serverName, serviceName string) string {
	return filepath.Join(s.rootDir, sanitizeSegment(serverName), sanitizeSegment(serviceName)+".log")
}

func (s *ServiceStore) cursorPath(serverName, serviceName string) string {
	return filepath.Join(s.rootDir, sanitizeSegment(serverName), sanitizeSegment(serviceName)+".cursor.json")
}

func (s *ServiceStore) readCursor(serverName, serviceName string) (logCursor, error) {
	path := s.cursorPath(serverName, serviceName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return logCursor{}, nil
		}
		return logCursor{}, fmt.Errorf("read log cursor: %w", err)
	}
	var cursor logCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return logCursor{}, fmt.Errorf("decode log cursor: %w", err)
	}
	return cursor, nil
}

func (s *ServiceStore) writeCursor(serverName, serviceName string, cursor logCursor) error {
	path := s.cursorPath(serverName, serviceName)
	data, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("marshal log cursor: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write log cursor: %w", err)
	}
	return nil
}

func (s *ServiceStore) rotateAndPrune(basePath string) error {
	info, err := os.Stat(basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat aggregated log: %w", err)
	}
	if info.Size() <= s.maxSizeBytes {
		return s.pruneBackups(basePath)
	}

	lastBackup := basePath + "." + strconv.Itoa(s.maxFiles)
	_ = os.Remove(lastBackup)
	for idx := s.maxFiles - 1; idx >= 1; idx-- {
		source := basePath + "." + strconv.Itoa(idx)
		target := basePath + "." + strconv.Itoa(idx+1)
		if _, err := os.Stat(source); err == nil {
			if err := os.Rename(source, target); err != nil {
				return fmt.Errorf("rotate aggregated log %s -> %s: %w", source, target, err)
			}
		}
	}
	if err := os.Rename(basePath, basePath+".1"); err != nil {
		return fmt.Errorf("rotate aggregated log %s: %w", basePath, err)
	}
	return s.pruneBackups(basePath)
}

func (s *ServiceStore) pruneBackups(basePath string) error {
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read aggregated log directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base+".") {
			continue
		}
		path := filepath.Join(dir, name)
		if s.maxAge > 0 {
			info, err := entry.Info()
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("stat aggregated backup %s: %w", name, err)
			}
			if err == nil && time.Since(info.ModTime()) > s.maxAge {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove expired aggregated backup %s: %w", name, err)
				}
				continue
			}
		}
		suffix := strings.TrimPrefix(name, base+".")
		index, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		if index > s.maxFiles {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove aggregated backup %s: %w", name, err)
			}
		}
	}
	return nil
}

func (s *ServiceStore) expireCache(basePath, cursorPath string) error {
	if s.maxAge <= 0 {
		return nil
	}
	paths := s.readPaths(basePath)
	expiredCurrent := false
	remaining := 0
	for _, path := range paths {
		info, err := os.Stat(path)
		switch {
		case err == nil:
		case os.IsNotExist(err):
			continue
		default:
			return fmt.Errorf("stat aggregated log %s: %w", path, err)
		}
		if time.Since(info.ModTime()) <= s.maxAge {
			remaining++
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove expired aggregated log %s: %w", path, err)
		}
		if path == basePath {
			expiredCurrent = true
		}
	}
	if expiredCurrent || remaining == 0 {
		if err := os.Remove(cursorPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove expired log cursor %s: %w", cursorPath, err)
		}
	}
	return nil
}

func (s *ServiceStore) readPaths(basePath string) []string {
	paths := []string{basePath}
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return paths
	}
	type rotated struct {
		index int
		path  string
	}
	var backups []rotated
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base+".") {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(name, base+"."))
		if err != nil {
			continue
		}
		backups = append(backups, rotated{
			index: index,
			path:  filepath.Join(dir, name),
		})
	}
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].index > backups[j].index
	})
	paths = paths[:0]
	for _, backup := range backups {
		paths = append(paths, backup.path)
	}
	paths = append(paths, basePath)
	return paths
}

func sanitizeSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(value)
}
