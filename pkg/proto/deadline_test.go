// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"testing"
	"time"
)

func TestDeadlineMillisNeverEarlier(t *testing.T) {
	t.Parallel()
	base := time.UnixMilli(1_790_000_000_000)
	for _, d := range []time.Duration{0, time.Nanosecond, 999_999 * time.Nanosecond, time.Millisecond, time.Millisecond + 1} {
		deadline := base.Add(d)
		got := time.UnixMilli(DeadlineMillis(deadline))
		if got.Before(deadline) {
			t.Errorf("DeadlineMillis(%v) = %v, earlier than the deadline", d, got)
		}
		if got.Sub(deadline) >= time.Millisecond {
			t.Errorf("DeadlineMillis(%v) = %v, a whole millisecond or more late", d, got)
		}
	}
}
