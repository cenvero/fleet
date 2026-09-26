// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cenvero/fleet/internal/logtail"
	"github.com/cenvero/fleet/pkg/proto"
)

// Benchmarks for log.read on generated 64MB and 256MB log files. Each size is
// generated once into $FLEET_LOG_BENCH_DIR (default $TMPDIR/fleet-log-bench)
// and reused by later runs, since writing 256MB takes a moment. Remove that
// directory when done benchmarking.

var (
	benchLogOnce  sync.Map // size -> *sync.Once
	benchLogPaths sync.Map // size -> string
)

func benchLogDir(b *testing.B) string {
	b.Helper()
	if dir := os.Getenv("FLEET_LOG_BENCH_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		return dir
	}
	return filepath.Join(os.TempDir(), "fleet-log-bench")
}

// benchLogLine renders a deterministic, realistic ~110 byte log line. Roughly
// one line in 100 is an ERROR.
func benchLogLine(w *bufio.Writer, i int) {
	level := "INFO"
	switch {
	case i%100 == 37:
		level = "ERROR"
	case i%17 == 3:
		level = "WARN"
	}
	fmt.Fprintf(w, "2026-09-26T12:%02d:%02d.%03dZ %-5s [worker-%02d] request id=%08x path=/api/v1/items/%d status=%d latency=%dms\n",
		(i/60000)%60, (i/1000)%60, i%1000, level, i%32, i*2654435761, i%100000, 200+(i%7)*50, i%250)
}

func benchLogFile(b *testing.B, sizeMB int) string {
	b.Helper()
	onceAny, _ := benchLogOnce.LoadOrStore(sizeMB, &sync.Once{})
	onceAny.(*sync.Once).Do(func() {
		dir := benchLogDir(b)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("app-%dmb.log", sizeMB))
		if info, err := os.Stat(path); err == nil && info.Size() >= int64(sizeMB)<<20 {
			benchLogPaths.Store(sizeMB, path)
			return
		}
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		w := bufio.NewWriterSize(f, 1<<20)
		target := int64(sizeMB) << 20
		var written int64
		for i := 0; written < target; i++ {
			before := w.Buffered()
			benchLogLine(w, i)
			written += int64(w.Buffered() - before)
			if w.Buffered() > 512<<10 {
				if err := w.Flush(); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := w.Flush(); err != nil {
			b.Fatal(err)
		}
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
		benchLogPaths.Store(sizeMB, path)
	})
	path, ok := benchLogPaths.Load(sizeMB)
	if !ok {
		b.Fatal("bench log file was not generated")
	}
	return path.(string)
}

func benchmarkLogRead(b *testing.B, sizeMB int, payload proto.LogReadPayload) {
	payload.Path = benchLogFile(b, sizeMB)
	reader := defaultLogReader()
	ctx := context.Background()
	// Warm the page cache so every iteration measures CPU, not the disk.
	if _, err := reader.Read(ctx, payload); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := reader.Read(ctx, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLogReadTail200_64MB(b *testing.B) {
	benchmarkLogRead(b, 64, proto.LogReadPayload{TailLines: 200})
}

func BenchmarkLogReadTail200_256MB(b *testing.B) {
	benchmarkLogRead(b, 256, proto.LogReadPayload{TailLines: 200})
}

// Search for a term that matches ~1% of lines (tail of matches found near EOF).
func BenchmarkLogReadSearchCommon_64MB(b *testing.B) {
	benchmarkLogRead(b, 64, proto.LogReadPayload{TailLines: 200, Search: "error"})
}

func BenchmarkLogReadSearchCommon_256MB(b *testing.B) {
	benchmarkLogRead(b, 256, proto.LogReadPayload{TailLines: 200, Search: "error"})
}

// Search for a term that never matches: the whole file must be scanned.
func BenchmarkLogReadSearchRare_64MB(b *testing.B) {
	benchmarkLogRead(b, 64, proto.LogReadPayload{TailLines: 200, Search: "Deadlock-Detected"})
}

func BenchmarkLogReadSearchRare_256MB(b *testing.B) {
	benchmarkLogRead(b, 256, proto.LogReadPayload{TailLines: 200, Search: "Deadlock-Detected"})
}

// benchmarkLogFollow measures one follow poll: a cursor read that finds
// newLines complete lines after the cursor (0 = nothing new since last poll).
// Before cursors existed every follow poll was a full tail read (Tail200).
func benchmarkLogFollow(b *testing.B, sizeMB, newLines int) {
	path := benchLogFile(b, sizeMB)
	file, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		b.Fatal(err)
	}
	// Place the cursor newLines lines before EOF.
	size := info.Size()
	window := min(size, int64(newLines+1)*512)
	buf := make([]byte, window)
	if _, err := file.ReadAt(buf, size-window); err != nil {
		b.Fatal(err)
	}
	cut := len(buf) // buf[:cut] precedes the cursor
	for range newLines {
		i := bytes.LastIndexByte(buf[:cut-1], '\n')
		if i < 0 {
			b.Fatal("bench log lines too long")
		}
		cut = i + 1
	}
	total, err := logtail.CountLines(file, size)
	if err != nil {
		b.Fatal(err)
	}
	offset := size - window + int64(cut)
	cursor := newLogCursor(file, info, logtail.Position{Offset: offset, Lines: total - newLines})
	payload := proto.LogReadPayload{Path: path, TailLines: 200, Cursor: cursor}

	reader := defaultLogReader()
	ctx := context.Background()
	res, err := reader.Read(ctx, payload)
	if err != nil {
		b.Fatal(err)
	}
	if res.Reset || len(res.Lines) != newLines || (newLines > 0 && res.Lines[0].Number != total-newLines+1) {
		b.Fatalf("unexpected follow result: reset=%v lines=%d", res.Reset, len(res.Lines))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := reader.Read(ctx, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLogFollowPollIdle_64MB(b *testing.B)     { benchmarkLogFollow(b, 64, 0) }
func BenchmarkLogFollowPollIdle_256MB(b *testing.B)    { benchmarkLogFollow(b, 256, 0) }
func BenchmarkLogFollowPoll1kLines_64MB(b *testing.B)  { benchmarkLogFollow(b, 64, 1000) }
func BenchmarkLogFollowPoll1kLines_256MB(b *testing.B) { benchmarkLogFollow(b, 256, 1000) }
