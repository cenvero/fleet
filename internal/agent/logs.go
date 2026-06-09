// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cenvero/fleet/pkg/proto"
)

// blockedLogPrefixes are OS virtual filesystems that must never be read as log
// files. Reading from these can expose arbitrary process memory (/proc/N/mem),
// kernel data structures, or raw device data — even to an authenticated client.
var blockedLogPrefixes = []string{"/proc/", "/sys/", "/dev/"}

// maxLogTailLines caps how many log lines are buffered/returned for a single
// read, bounding the agent's memory regardless of the requested tail or file
// size.
const maxLogTailLines = 100_000

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
	// Resolve symlinks so a symlink pointing to /proc/1/mem cannot bypass the
	// block below. Ignore resolution errors — the open below will surface them.
	// We then open realPath (not payload.Path) to eliminate the TOCTOU window
	// between the EvalSymlinks check and the open: if an attacker swaps the
	// symlink after we resolve it, we still open the originally resolved target.
	realPath := payload.Path
	if resolved, err := filepath.EvalSymlinks(payload.Path); err == nil {
		realPath = resolved
	}
	for _, blocked := range blockedLogPrefixes {
		if strings.HasPrefix(realPath, blocked) {
			return proto.LogReadResult{}, &RPCError{
				Code:    "invalid_log_path",
				Message: fmt.Sprintf("reading from %s is not permitted", blocked),
			}
		}
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

	search := strings.ToLower(strings.TrimSpace(payload.Search))
	tailLines := payload.TailLines
	if tailLines <= 0 {
		tailLines = 200
	}
	tailLines = min(tailLines, maxLogTailLines)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

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
		return proto.LogReadResult{}, &RPCError{
			Code:    "log_read_failed",
			Message: err.Error(),
		}
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
	return proto.LogReadResult{
		Path:      payload.Path,
		Lines:     out,
		Truncated: matched > tailLines,
	}, nil
}
