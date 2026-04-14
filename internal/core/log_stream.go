// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/pkg/proto"
)

const DefaultLogFollowInterval = time.Second

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

	var lastNumber int
	for {
		result, err := a.readServiceLogs(serverName, serviceName, search, tailLines, false, false)
		if err != nil {
			return err
		}

		startNumber := lastNumber + 1
		if len(result.Lines) > 0 && result.Lines[len(result.Lines)-1].Number < lastNumber {
			startNumber = 1
		}
		for _, line := range result.Lines {
			if line.Number < startNumber {
				continue
			}
			if err := emit(line); err != nil {
				return err
			}
			if line.Number > lastNumber {
				lastNumber = line.Number
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
