// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeBenchStoreFile writes ~sizeBytes of realistic log lines to path.
func writeBenchStoreFile(tb testing.TB, path string, sizeBytes int64, seed int) {
	tb.Helper()
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	var written int64
	for i := seed; written < sizeBytes; i++ {
		n, _ := fmt.Fprintf(w, "2026-09-26T12:%02d:%02d.%03dZ INFO [worker-%02d] request id=%08x path=/api/v1/items/%d status=%d latency=%dms\n",
			(i/60000)%60, (i/1000)%60, i%1000, i%32, i*2654435761, i%100000, 200+(i%7)*50, i%250)
		written += int64(n)
	}
	if err := w.Flush(); err != nil {
		tb.Fatal(err)
	}
	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}
}

// benchStore lays out a cached log with the current file plus `rotated` full
// backups, each ~5MB (the default rotation size).
func benchStore(b *testing.B, rotated int) *ServiceStore {
	b.Helper()
	root := b.TempDir()
	store := NewServiceStore(root, DefaultAggregatedLogMaxSize, DefaultAggregatedLogMaxFiles, time.Hour)
	base := store.basePath("web-01", "nginx.service")
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		b.Fatal(err)
	}
	for idx := rotated; idx >= 1; idx-- {
		writeBenchStoreFile(b, base+"."+strconv.Itoa(idx), DefaultAggregatedLogMaxSize, idx*1_000_000)
	}
	writeBenchStoreFile(b, base, DefaultAggregatedLogMaxSize, 0)
	return store
}

func benchmarkStoreRead(b *testing.B, rotated int, search string, tail int) {
	benchmarkStoreReadCache(b, rotated, search, tail, true)
}

// benchmarkStoreReadCache with warm=false forgets the remembered line counts
// before every read (as after the files changed), so every file is counted.
func benchmarkStoreReadCache(b *testing.B, rotated int, search string, tail int, warm bool) {
	store := benchStore(b, rotated)
	if _, err := store.Read("web-01", "nginx.service", search, tail); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if !warm {
			lineCounts.Lock()
			clear(lineCounts.entries)
			lineCounts.Unlock()
		}
		if _, err := store.Read("web-01", "nginx.service", search, tail); err != nil {
			b.Fatal(err)
		}
	}
}

// The dashboard reads the last 12 cached lines of every tracked service.
func BenchmarkServiceStoreReadTail12_5MB(b *testing.B)  { benchmarkStoreRead(b, 0, "", 12) }
func BenchmarkServiceStoreReadTail12_25MB(b *testing.B) { benchmarkStoreRead(b, 4, "", 12) }

// Same, with nothing remembered from earlier reads.
func BenchmarkServiceStoreReadTail12_5MB_Cold(b *testing.B) {
	benchmarkStoreReadCache(b, 0, "", 12, false)
}
func BenchmarkServiceStoreReadTail12_25MB_Cold(b *testing.B) {
	benchmarkStoreReadCache(b, 4, "", 12, false)
}

// `fleet logs --cached` defaults to 200 lines.
func BenchmarkServiceStoreReadTail200_25MB(b *testing.B) { benchmarkStoreRead(b, 4, "", 200) }

// A search that never matches has to look at every cached line.
func BenchmarkServiceStoreReadSearchMiss_25MB(b *testing.B) {
	benchmarkStoreRead(b, 4, "Deadlock-Detected", 200)
}
