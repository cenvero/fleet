// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cenvero/fleet/internal/logtail"
	"github.com/cenvero/fleet/pkg/proto"
)

// maxLogTailLines caps how many log lines are buffered/returned for a single
// read, bounding the agent's memory regardless of the requested tail or file
// size.
const maxLogTailLines = 100_000

// A cursor read (follow polling) is paged so that a burst of appended lines can
// neither build an oversized response envelope nor hold the RPC channel for
// long; the controller reads the next page straight away when More is set.
const (
	maxLogFollowBytes = 4 << 20  // returned text per cursor read
	maxLogFollowScan  = 64 << 20 // bytes scanned per cursor read
)

// logCursorSumBytes is how many bytes before a cursor's offset are
// fingerprinted to notice a file that was truncated and then regrew past it.
const logCursorSumBytes = 64

type LogReader interface {
	Read(context.Context, proto.LogReadPayload) (proto.LogReadResult, error)
}

type fileLogReader struct{}

func defaultLogReader() LogReader {
	return fileLogReader{}
}

func (fileLogReader) Read(_ context.Context, payload proto.LogReadPayload) (proto.LogReadResult, error) {
	if payload.Path == "" {
		return proto.LogReadResult{}, &RPCError{
			Code:    "missing_log_path",
			Message: "log path is required",
		}
	}
	if !filepath.IsAbs(payload.Path) {
		return proto.LogReadResult{}, &RPCError{
			Code:    "invalid_log_path",
			Message: "log path must be absolute",
		}
	}
	// Resolve symlinks so a symlink pointing to /proc/1/mem (or anywhere outside
	// --file-root) cannot bypass the checks below. Ignore resolution errors — the
	// open below will surface them. We then open realPath (not payload.Path) to
	// eliminate the TOCTOU window between the check and the open: if an attacker
	// swaps the symlink after we resolve it, we still open the originally
	// resolved target.
	realPath := filepath.Clean(payload.Path)
	if resolved, err := filepath.EvalSymlinks(realPath); err == nil {
		realPath = resolved
	}
	// log.read must honor the SAME sandbox as file.* operations: reject the OS
	// pseudo filesystems AND confine to the agent's allowed file roots
	// (--file-root). Without this an authenticated controller could read ANY
	// file the agent user can — /etc/shadow, ~/.ssh/id_*, the agent host key —
	// regardless of the configured roots. checkBlockedTransferPath covers both
	// the /proc,/sys,/dev block list and withinAllowedRoots, so it subsumes the
	// previous prefix-only check.
	if rerr := checkBlockedTransferPath(realPath); rerr != nil {
		// Surface under the log-path error code for callers that special-case it,
		// but keep the descriptive message from the shared validator.
		return proto.LogReadResult{}, &RPCError{Code: "invalid_log_path", Message: rerr.Message}
	}
	if payload.Follow {
		return proto.LogReadResult{}, &RPCError{
			Code:    "unsupported_capability",
			Message: "follow mode is not implemented yet",
		}
	}

	file, err := os.Open(realPath)
	if err != nil {
		return proto.LogReadResult{}, &RPCError{
			Code:    "log_open_failed",
			Message: err.Error(),
		}
	}
	defer file.Close()

	tailLines := payload.TailLines
	if tailLines <= 0 {
		tailLines = 200
	}
	tailLines = min(tailLines, maxLogTailLines)

	info, err := file.Stat()
	if err != nil {
		return proto.LogReadResult{}, logReadFailed(err)
	}
	if !info.Mode().IsRegular() {
		// Pipes, character devices, directories: no size to seek from, so keep
		// the original streaming scan (and its exact errors). No cursor.
		lines, truncated, err := scanLogStream(file, payload.Search, tailLines)
		if err != nil {
			return proto.LogReadResult{}, logReadFailed(err)
		}
		return proto.LogReadResult{Path: payload.Path, Lines: lines, Truncated: truncated}, nil
	}

	matcher := logtail.NewMatcher(payload.Search)
	for attempt := 1; ; attempt++ {
		result, err := readLogFile(file, info, payload, tailLines, matcher)
		if err == nil {
			return result, nil
		}
		// A file truncated while it is being read ends early: read it again
		// at its new size (a cursor then no longer matches and resets).
		if !errors.Is(err, io.ErrUnexpectedEOF) || attempt == maxLogReadAttempts {
			return proto.LogReadResult{}, logReadFailed(err)
		}
		if info, err = file.Stat(); err != nil {
			return proto.LogReadResult{}, logReadFailed(err)
		}
	}
}

// maxLogReadAttempts bounds re-reads of a log that keeps shrinking under us.
const maxLogReadAttempts = 3

// readLogFile answers a log.read of the regular file open as file, whose
// size is info.Size().
func readLogFile(file *os.File, info os.FileInfo, payload proto.LogReadPayload, tailLines int, matcher *logtail.Matcher) (proto.LogReadResult, error) {
	size := info.Size()
	reset := false
	if payload.Cursor != nil {
		from, ok := resumeLogCursor(file, info, payload.Cursor)
		if !ok && size <= maxLogFollowBytes {
			// Truncated, replaced or rotated since the cursor was issued, and
			// the file is small, as it is right after a copytruncate or a
			// rotation: everything in it is new, so read it from the start.
			from, reset = logtail.Position{}, true
		}
		if ok || reset {
			next, err := logtail.Forward(file, from, size, matcher, logtail.ForwardLimits{
				MaxLines: maxLogTailLines,
				MaxBytes: maxLogFollowBytes,
				MaxScan:  maxLogFollowScan,
			})
			if err != nil {
				return proto.LogReadResult{}, err
			}
			return proto.LogReadResult{
				Path:   payload.Path,
				Lines:  toProtoLines(next.Lines),
				Cursor: newLogCursor(file, info, next.Next),
				Reset:  reset,
				More:   next.More,
			}, nil
		}
		// Replaced by a large file: start over with its tail.
		reset = true
	}

	tail, err := logtail.Tail(file, size, tailLines, matcher)
	if err != nil {
		return proto.LogReadResult{}, err
	}
	return proto.LogReadResult{
		Path:      payload.Path,
		Lines:     toProtoLines(tail.Lines),
		Truncated: tail.Truncated,
		Cursor:    newLogCursor(file, info, tail.End),
		Reset:     reset,
	}, nil
}

func logReadFailed(err error) *RPCError {
	return &RPCError{Code: "log_read_failed", Message: err.Error()}
}

// toProtoLines converts to the wire type, always returning a non-nil slice so
// an empty result still encodes as "lines": [].
func toProtoLines(lines []logtail.Line) []proto.LogLine {
	out := make([]proto.LogLine, len(lines))
	for i, line := range lines {
		out[i] = proto.LogLine{Number: line.Number, Text: line.Text}
	}
	return out
}

// newLogCursor describes pos in the open file. It returns nil (the controller
// then falls back to re-reading the tail) if the fingerprint cannot be read.
func newLogCursor(file *os.File, info os.FileInfo, pos logtail.Position) *proto.LogCursor {
	sum, _, err := logCursorSum(file, pos.Offset)
	if err != nil {
		return nil
	}
	return &proto.LogCursor{Offset: pos.Offset, Line: pos.Lines, FileID: logFileID(info), Sum: sum}
}

// resumeLogCursor validates a cursor from the controller against the open
// file: same file identity, not shorter than the cursor, and the bytes just
// before the cursor unchanged (which also proves the offset still sits just
// past a '\n').
func resumeLogCursor(file *os.File, info os.FileInfo, c *proto.LogCursor) (logtail.Position, bool) {
	if c.Offset < 0 || c.Line < 0 || int64(c.Line) > c.Offset || c.Offset > info.Size() {
		return logtail.Position{}, false
	}
	if c.FileID != logFileID(info) {
		return logtail.Position{}, false
	}
	sum, atBoundary, err := logCursorSum(file, c.Offset)
	if err != nil || !atBoundary || sum != c.Sum {
		return logtail.Position{}, false
	}
	return logtail.Position{Offset: c.Offset, Lines: c.Line}, true
}

// logCursorSum fingerprints the logCursorSumBytes bytes before offset and
// reports whether offset is a line boundary (0, or just past a '\n').
func logCursorSum(file *os.File, offset int64) (string, bool, error) {
	start := max(offset-logCursorSumBytes, 0)
	buf := make([]byte, offset-start)
	if len(buf) > 0 {
		if n, err := file.ReadAt(buf, start); n != len(buf) {
			return "", false, err
		}
	}
	h := fnv.New64a()
	_, _ = h.Write(buf)
	atBoundary := len(buf) == 0 || buf[len(buf)-1] == '\n'
	return hex.EncodeToString(h.Sum(nil)), atBoundary, nil
}

// scanLogStream is the original forward scan, kept for files that cannot be
// read by offset. It returns the last tailLines lines matching search.
func scanLogStream(file *os.File, search string, tailLines int) ([]proto.LogLine, bool, error) {
	search = strings.ToLower(strings.TrimSpace(search))
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), logtail.MaxLineBytes)

	// Bounded ring buffer: keep only the last tailLines matching lines so that
	// reading a multi-gigabyte log cannot balloon the agent's memory.
	ring := make([]proto.LogLine, tailLines)
	matched := 0
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if search != "" && !strings.Contains(strings.ToLower(line), search) {
			continue
		}
		ring[matched%tailLines] = proto.LogLine{Number: lineNumber, Text: line}
		matched++
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}

	n := min(matched, tailLines)
	out := make([]proto.LogLine, n)
	start := 0
	if matched > tailLines {
		start = matched % tailLines
	}
	for i := range n {
		out[i] = ring[(start+i)%tailLines]
	}
	return out, matched > tailLines, nil
}
