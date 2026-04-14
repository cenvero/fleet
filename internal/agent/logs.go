// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/cenvero/fleet/pkg/proto"
)

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
	if payload.Follow {
		return proto.LogReadResult{}, &RPCError{
			Code:    "unsupported_capability",
			Message: "follow mode is not implemented yet",
		}
	}

	file, err := os.Open(payload.Path)
	if err != nil {
		return proto.LogReadResult{}, &RPCError{
			Code:    "log_open_failed",
			Message: err.Error(),
		}
	}
	defer file.Close()

	search := strings.ToLower(strings.TrimSpace(payload.Search))
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lines := make([]proto.LogLine, 0, 128)
	lineNumber := 0
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
		return proto.LogReadResult{}, &RPCError{
			Code:    "log_read_failed",
			Message: err.Error(),
		}
	}

	tailLines := payload.TailLines
	if tailLines <= 0 {
		tailLines = 200
	}

	result := proto.LogReadResult{
		Path:  payload.Path,
		Lines: lines,
	}
	if len(lines) > tailLines {
		result.Truncated = true
		result.Lines = append([]proto.LogLine(nil), lines[len(lines)-tailLines:]...)
	}
	return result, nil
}
