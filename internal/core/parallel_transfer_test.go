// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"sync"
	"testing"
)

// TestRunParallelTransfersProgressRaceFree drives many concurrent progress
// callbacks per file (as the real per-file sender goroutines do) and asserts the
// aggregate is race-free and accounts correctly. Run with -race.
func TestRunParallelTransfersProgressRaceFree(t *testing.T) {
	rels := []string{"a", "b", "c", "d"}
	const perFile = 1000
	var mu sync.Mutex
	var lastReported int64
	n, err := runParallelTransfers(rels, int64(len(rels)*perFile),
		func(u ProgressUpdate) {
			mu.Lock()
			lastReported = u.BytesDone
			mu.Unlock()
		},
		func(rel string, fp ProgressFunc) error {
			// Simulate N concurrent senders reporting this file's cumulative bytes.
			var wg sync.WaitGroup
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for b := int64(0); b <= perFile; b += 250 {
						fp(ProgressUpdate{BytesDone: b})
					}
				}()
			}
			wg.Wait()
			fp(ProgressUpdate{BytesDone: perFile}) // settle to the file's full size
			return nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(rels) {
		t.Fatalf("transferred %d, want %d", n, len(rels))
	}
	mu.Lock()
	defer mu.Unlock()
	if lastReported != int64(len(rels)*perFile) {
		t.Fatalf("final aggregate = %d, want %d", lastReported, len(rels)*perFile)
	}
}
