// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadServiceLogsCachesAcrossRotation is the end-to-end B10 regression
// for plain `fleet service logs` reads through the real agent log reader: the
// remote log is cached, then rotated (rename or copytruncate) and grows past
// the old cursor before the next read. Every line read must reach the
// controller's cache exactly once.
func TestReadServiceLogsCachesAcrossRotation(t *testing.T) {
	for _, mode := range []string{"rename", "copytruncate"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			logPath := filepath.Join(dir, "app.log")
			writeTestLog(t, logPath, 1, 100, "old-")
			h := newLogHarness(t, nil, logPath)
			defer func() {
				h.app.DisconnectPooledSessions()
				drainAgentServeErrs(t, h.errCh)
				h.app.Close()
			}()
			read := func() {
				t.Helper()
				if _, err := h.app.ReadServiceLogs("loopback", "app.service", "", 200, false); err != nil {
					t.Fatalf("ReadServiceLogs() error = %v", err)
				}
			}
			cached := func() []string {
				t.Helper()
				res, err := h.app.ReadCachedServiceLogs("loopback", "app.service", "", 100_000)
				if err != nil {
					t.Fatalf("ReadCachedServiceLogs() error = %v", err)
				}
				out := make([]string, len(res.Lines))
				for i, l := range res.Lines {
					out[i] = l.Text
				}
				return out
			}
			lines := func(prefix string, from, to int) []string {
				var out []string
				for i := from; i <= to; i++ {
					out = append(out, fmt.Sprintf("%s%d", prefix, i))
				}
				return out
			}
			expect := func(want ...[]string) {
				t.Helper()
				var all []string
				for _, w := range want {
					all = append(all, w...)
				}
				if got := cached(); strings.Join(got, ",") != strings.Join(all, ",") {
					t.Fatalf("cache has %d lines, want %d (%v ... %v)", len(got), len(all), got[:min(3, len(got))], got[max(0, len(got)-3):])
				}
			}

			read()
			expect(lines("old-", 1, 100))
			writeTestLog(t, logPath, 101, 120, "old-")
			read()
			read() // nothing new: no duplicates
			expect(lines("old-", 1, 120))

			switch mode {
			case "rename":
				if err := os.Rename(logPath, logPath+".1"); err != nil {
					t.Fatal(err)
				}
			case "copytruncate":
				if err := os.Truncate(logPath, 0); err != nil {
					t.Fatal(err)
				}
			}
			writeTestLog(t, logPath, 1, 150, "new-") // already past the old cursor (120)
			read()
			expect(lines("old-", 1, 120), lines("new-", 1, 150))
			writeTestLog(t, logPath, 151, 160, "new-")
			read()
			expect(lines("old-", 1, 120), lines("new-", 1, 160))
		})
	}
}
