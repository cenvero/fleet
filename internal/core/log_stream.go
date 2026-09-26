// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/pkg/proto"
)

const DefaultLogFollowInterval = time.Second

// maxLogFollowCatchUpPages bounds how many "More" pages are read back to back
// before the follow loop yields to its poll interval again.
const maxLogFollowCatchUpPages = 64

// maxLogFollowOpenRetries is how many consecutive polls may find the log
// missing (log_open_failed) before following gives up. It bridges the moment
// between a rotation's rename and the creation of the new file.
const maxLogFollowOpenRetries = 5

// FollowServiceLogs prints the tail of a tracked service log and then every
// line appended to it until ctx is cancelled.
//
// Agents that support log cursors return one with every read; the loop then
// asks only for the bytes after it, so an idle poll costs the agent a stat
// and a 64-byte read instead of a full tail, and bursts of more than
// tailLines lines per interval are no longer dropped. Older agents return a
// plain tail without a cursor and are followed as before: re-read the tail
// every interval and de-duplicate by line number.
func (a *App) FollowServiceLogs(ctx context.Context, serverName, serviceName, search string, tailLines int, interval time.Duration, emit func(proto.LogLine) error) error {
	if interval <= 0 {
		interval = DefaultLogFollowInterval
	}
	if emit == nil {
		return fmt.Errorf("log follow callback is required")
	}

	if err := a.AuditLog.Append(logs.AuditEntry{
		Action:   "service.logs.follow",
		Target:   serverName + "/" + serviceName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("search=%q interval=%s", search, interval),
	}); err != nil {
		return err
	}

	cache := strings.TrimSpace(search) == ""
	follower := logFollower{emit: emit}
	var cursor *proto.LogCursor
	pages, openFailures, polled := 0, 0, false
	for {
		result, code, err := a.readServiceLogPage(serverName, serviceName, search, tailLines, cursor)
		if err != nil {
			if !polled || code != "log_open_failed" || openFailures >= maxLogFollowOpenRetries {
				return err
			}
			// Mid-rotation (renamed away, not yet recreated): try again.
			openFailures++
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(interval):
			}
			continue
		}
		polled, openFailures = true, 0
		if result.Cursor == nil {
			// Agent without cursor support (or it could not issue one): the
			// original tail-and-dedupe path, caching the whole tail as before.
			if cache {
				if err := a.aggregatedLogs().Append(serverName, serviceName, result.Lines); err != nil {
					return err
				}
			}
			cursor = nil
			if err := follower.applyTail(result); err != nil {
				return err
			}
		} else {
			emitted, err := follower.applyCursor(result)
			if cache && len(emitted) > 0 {
				if cerr := a.aggregatedLogs().Append(serverName, serviceName, emitted); cerr != nil && err == nil {
					err = cerr
				}
			}
			if err != nil {
				return err
			}
			cursor = result.Cursor
		}

		if result.More && cursor != nil && pages < maxLogFollowCatchUpPages {
			// The agent paged a burst: fetch the rest without waiting.
			pages++
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		pages = 0
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// readServiceLogPage is one log.read for a follow poll: no audit entry, no
// caching (the follow loop caches what it emits), optional cursor. On an
// agent-side failure it also returns the agent's error code.
func (a *App) readServiceLogPage(serverName, serviceName, search string, tailLines int, cursor *proto.LogCursor) (proto.LogReadResult, string, error) {
	server, service, err := a.trackedService(serverName, serviceName)
	if err != nil {
		return proto.LogReadResult{}, "", err
	}
	if strings.TrimSpace(service.LogPath) == "" {
		return proto.LogReadResult{}, "", fmt.Errorf("service %q on %q does not have a tracked log path", serviceName, serverName)
	}
	response, err := a.callRPC(server, proto.Envelope{
		Action: "log.read",
		Payload: proto.LogReadPayload{
			Server:    serverName,
			Service:   serviceName,
			Path:      service.LogPath,
			Search:    search,
			TailLines: tailLines,
			Cursor:    cursor,
		},
	})
	if err != nil {
		return proto.LogReadResult{}, "", err
	}
	if response.Error != nil {
		return proto.LogReadResult{}, response.Error.Code, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}
	result, err := proto.DecodePayload[proto.LogReadResult](response.Payload)
	return result, "", err
}

// logFollower decides which lines of each poll are new.
type logFollower struct {
	emit       func(proto.LogLine) error
	lastNumber int
	// pending is an unterminated last line seen on the previous cursor poll.
	// It is printed once it is complete, or once it has stayed unchanged for
	// a whole poll interval (a writer that never ends its last line), so a
	// line caught mid-write is not printed truncated.
	pending *proto.LogLine
}

// applyTail handles a plain tail (no cursor): print lines numbered after the
// last printed one. When the tail now ends below that number the file was
// truncated or replaced, so numbering restarted.
func (f *logFollower) applyTail(result proto.LogReadResult) error {
	f.pending = nil
	if len(result.Lines) > 0 && result.Lines[len(result.Lines)-1].Number < f.lastNumber {
		// Reset the high-water mark too; keeping the old one re-printed the
		// whole new tail on every poll until the new file outgrew it.
		f.lastNumber = 0
	}
	for _, line := range result.Lines {
		if line.Number <= f.lastNumber {
			continue
		}
		if err := f.emit(line); err != nil {
			return err
		}
		f.lastNumber = line.Number
	}
	return nil
}

// applyCursor handles a read that carries a cursor: its lines are exactly
// the ones after the previous cursor (or a fresh tail on the first poll and
// after Reset). It returns the lines it printed.
func (f *logFollower) applyCursor(result proto.LogReadResult) ([]proto.LogLine, error) {
	if result.Reset {
		f.lastNumber = 0
		f.pending = nil
	}
	var emitted []proto.LogLine
	var unterminated *proto.LogLine
	for _, line := range result.Lines {
		if line.Number <= f.lastNumber {
			continue // already printed (e.g. a line held back and then released)
		}
		if line.Number > result.Cursor.Line {
			// The file's unterminated last line: not consumed by the cursor.
			if f.pending == nil || f.pending.Number != line.Number || f.pending.Text != line.Text {
				held := line
				unterminated = &held
				continue
			}
		}
		if err := f.emit(line); err != nil {
			return emitted, err
		}
		emitted = append(emitted, line)
		f.lastNumber = line.Number
	}
	f.pending = unterminated
	return emitted, nil
}
