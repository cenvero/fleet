// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/logtail"
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

	file, err := os.OpenFile(basePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- path is derived from the controller log root and validated server/service components
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
	paths := s.readPaths(basePath) // oldest backup first, current file last
	if tailLines <= 0 {
		tailLines = 200
	}
	matcher := logtail.NewMatcher(search)

	// The files read as one log, oldest first, numbered continuously. Read
	// backwards from the end of the current file and open older files only
	// while more lines are needed (or, once there are enough, to learn
	// whether any older line matches too, which decides Truncated).
	tails := make([]logtail.TailResult, len(paths))
	have, truncated, first := 0, false, len(paths)
	for i := len(paths) - 1; i >= 0; i-- {
		first = i
		res, found, err := tailCachedLog(paths[i], tailLines-have, matcher)
		if err != nil {
			return proto.LogReadResult{}, err
		}
		if !found {
			continue
		}
		tails[i] = res
		have += len(res.Lines)
		if res.Truncated {
			truncated = true
			break
		}
	}
	// Lines in the files older than the ones read, for numbering.
	lineNumber := 0
	for _, path := range paths[:first] {
		n, err := countCachedLogLines(path)
		if err != nil {
			return proto.LogReadResult{}, err
		}
		lineNumber += n
	}
	lines := make([]proto.LogLine, 0, have)
	for _, res := range tails[first:] {
		for _, line := range res.Lines {
			lines = append(lines, proto.LogLine{Number: lineNumber + line.Number, Text: line.Text})
		}
		lineNumber += res.TotalLines
	}
	return proto.LogReadResult{Path: basePath, Lines: lines, Truncated: truncated}, nil
}

// tailCachedLog returns the last n lines of one cached log file matched by m
// (found is false when the file does not exist). Its line count is remembered
// so the next read of the unchanged file skips counting.
func tailCachedLog(path string, n int, m *logtail.Matcher) (logtail.TailResult, bool, error) {
	file, err := os.Open(path) // #nosec G304 -- path is derived from the controller log root and validated server/service components
	if err != nil {
		if os.IsNotExist(err) {
			return logtail.TailResult{}, false, nil
		}
		return logtail.TailResult{}, false, fmt.Errorf("open aggregated log %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return logtail.TailResult{}, false, fmt.Errorf("scan aggregated log %s: %w", path, err)
	}
	var res logtail.TailResult
	if known, ok := cachedLineCount(path, info); ok {
		res, err = logtail.TailKnown(file, info.Size(), n, m, known)
	} else {
		res, err = logtail.Tail(file, info.Size(), n, m)
	}
	if err != nil {
		return logtail.TailResult{}, false, fmt.Errorf("scan aggregated log %s: %w", path, err)
	}
	rememberLineCount(path, info, res.TotalLines)
	return res, true, nil
}

// countCachedLogLines returns how many lines a cached log file holds (0 if it
// does not exist), counting only when the file changed since last time.
// Rotated backups never change, so they are counted once per rotation.
func countCachedLogLines(path string) (int, error) {
	if info, err := os.Stat(path); err == nil {
		if n, ok := cachedLineCount(path, info); ok {
			return n, nil
		}
	}
	file, err := os.Open(path) // #nosec G304 -- path is derived from the controller log root and validated server/service components
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("open aggregated log %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("scan aggregated log %s: %w", path, err)
	}
	n, err := logtail.CountLines(file, info.Size())
	if err != nil {
		return 0, fmt.Errorf("scan aggregated log %s: %w", path, err)
	}
	rememberLineCount(path, info, n)
	return n, nil
}

// lineCounts remembers the line count of cached log files, keyed by path and
// valid only while the file is the same file (device/inode) with the same size
// and modification time. The files only ever grow by appends (which change
// size and mtime) or move by rotation (which changes the file at the path).
var lineCounts = struct {
	sync.Mutex
	entries map[string]lineCountEntry
}{entries: map[string]lineCountEntry{}}

type lineCountEntry struct {
	info  os.FileInfo
	lines int
}

// maxLineCountEntries bounds the cache (a handful of files per tracked
// service); it is simply cleared when full.
const maxLineCountEntries = 4096

func cachedLineCount(path string, info os.FileInfo) (int, bool) {
	lineCounts.Lock()
	entry, ok := lineCounts.entries[path]
	lineCounts.Unlock()
	if !ok || !os.SameFile(entry.info, info) || entry.info.Size() != info.Size() || !entry.info.ModTime().Equal(info.ModTime()) {
		return 0, false
	}
	return entry.lines, true
}

func rememberLineCount(path string, info os.FileInfo, lines int) {
	lineCounts.Lock()
	defer lineCounts.Unlock()
	if len(lineCounts.entries) >= maxLineCountEntries {
		clear(lineCounts.entries)
	}
	lineCounts.entries[path] = lineCountEntry{info: info, lines: lines}
}

func (s *ServiceStore) basePath(serverName, serviceName string) string {
	return filepath.Join(s.rootDir, sanitizeSegment(serverName), sanitizeSegment(serviceName)+".log")
}

func (s *ServiceStore) cursorPath(serverName, serviceName string) string {
	return filepath.Join(s.rootDir, sanitizeSegment(serverName), sanitizeSegment(serviceName)+".cursor.json")
}

func (s *ServiceStore) readCursor(serverName, serviceName string) (logCursor, error) {
	path := s.cursorPath(serverName, serviceName)
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from the controller log root and validated server/service components
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
