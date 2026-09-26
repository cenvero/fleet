// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

func enqueueN(t *testing.T, q *fileMetricsQueue, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if err := q.Enqueue(proto.MetricsSnapshot{Timestamp: time.Unix(int64(i), 0).UTC(), ProcessCount: uint64(i)}); err != nil {
			t.Fatalf("Enqueue(%d): %v", i, err)
		}
	}
}

func TestMetricsQueuePagesBacklog(t *testing.T) {
	q := NewFileMetricsQueue(filepath.Join(t.TempDir(), "metrics.jsonl")).(*fileMetricsQueue)
	enqueueN(t, q, 1, 25)

	var got []uint64
	for page := 0; page < 10; page++ {
		batch, err := q.PeekPage(10)
		if err != nil {
			t.Fatalf("PeekPage: %v", err)
		}
		if len(batch.Snapshots) == 0 {
			break
		}
		if len(batch.Snapshots) > 10 {
			t.Fatalf("page of %d snapshots exceeds the requested 10", len(batch.Snapshots))
		}
		// Repeating the peek before acknowledging returns the same page.
		again, err := q.PeekPage(3)
		if err != nil || again.ID != batch.ID || len(again.Snapshots) != len(batch.Snapshots) {
			t.Fatalf("repeated PeekPage changed the in-flight batch: %v %+v", err, again)
		}
		for _, s := range batch.Snapshots {
			got = append(got, s.ProcessCount)
		}
		wantMore := len(got) < 25
		if batch.More != wantMore {
			t.Fatalf("page %d More = %v, want %v", page, batch.More, wantMore)
		}
		if err := q.Acknowledge(batch.ID); err != nil {
			t.Fatalf("Acknowledge: %v", err)
		}
	}
	if len(got) != 25 {
		t.Fatalf("paged %d snapshots, want 25", len(got))
	}
	for i, v := range got {
		if v != uint64(i+1) {
			t.Fatalf("snapshots out of order or lost: %v", got)
		}
	}
}

// Peek (the whole queue) is what older controllers call; it must still return
// everything in one batch.
func TestMetricsQueuePeekStillReturnsWholeQueue(t *testing.T) {
	q := NewFileMetricsQueue(filepath.Join(t.TempDir(), "metrics.jsonl")).(*fileMetricsQueue)
	enqueueN(t, q, 1, 12)
	batch, err := q.Peek()
	if err != nil || len(batch.Snapshots) != 12 || batch.More {
		t.Fatalf("Peek = %d snapshots, more=%v, err=%v; want all 12", len(batch.Snapshots), batch.More, err)
	}
}

func TestMetricsQueueAcknowledgementIsIdempotent(t *testing.T) {
	q := NewFileMetricsQueue(filepath.Join(t.TempDir(), "metrics.jsonl")).(*fileMetricsQueue)
	enqueueN(t, q, 1, 4)
	first, err := q.PeekPage(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Acknowledge(first.ID); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	// The controller never saw the answer and retries: success, nothing lost.
	if err := q.Acknowledge(first.ID); err != nil {
		t.Fatalf("repeated Acknowledge of the same batch = %v, want success", err)
	}
	second, err := q.PeekPage(10)
	if err != nil || len(second.Snapshots) != 2 || second.Snapshots[0].ProcessCount != 3 {
		t.Fatalf("remaining queue after a repeated ack = %+v, err=%v", second, err)
	}
	// A late duplicate of the first ack must not acknowledge the second batch.
	if err := q.Acknowledge(first.ID); err != nil {
		t.Fatalf("late duplicate ack = %v", err)
	}
	if again, _ := q.PeekPage(10); again.ID != second.ID {
		t.Fatal("a duplicate ack of an older batch disturbed the in-flight batch")
	}
	if err := q.Acknowledge("not-a-batch"); err == nil {
		t.Fatal("an unknown batch must still be refused")
	}
}

func TestMetricsQueueThinsOlderHalfAtCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	q := NewFileMetricsQueue(path).(*fileMetricsQueue)
	q.maxSnapshots = 100
	enqueueN(t, q, 0, 300)

	batch, err := q.Peek()
	if err != nil {
		t.Fatal(err)
	}
	n := len(batch.Snapshots)
	if n > 100 || n < 50 {
		t.Fatalf("queue holds %d snapshots, want it capped at <=100 (and not emptied)", n)
	}
	// Coverage of the whole outage survives: the oldest snapshot is kept and
	// the newest ones are intact.
	if first := batch.Snapshots[0].ProcessCount; first != 0 {
		t.Fatalf("oldest queued snapshot = %d, want 0 (thinning keeps the start of the outage)", first)
	}
	if last := batch.Snapshots[n-1].ProcessCount; last != 299 {
		t.Fatalf("newest queued snapshot = %d, want 299", last)
	}
	for i := 1; i < n; i++ {
		if batch.Snapshots[i].ProcessCount <= batch.Snapshots[i-1].ProcessCount {
			t.Fatal("thinning reordered the queue")
		}
	}
	// Older entries are sparser than newer ones.
	olderGap := batch.Snapshots[1].ProcessCount - batch.Snapshots[0].ProcessCount
	newerGap := batch.Snapshots[n-1].ProcessCount - batch.Snapshots[n-2].ProcessCount
	if olderGap <= newerGap {
		t.Fatalf("older gap %d should exceed newer gap %d after thinning", olderGap, newerGap)
	}
}

func TestMetricsQueueByteCapBoundsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	q := NewFileMetricsQueue(path).(*fileMetricsQueue)
	q.maxBytes = 8 << 10
	enqueueN(t, q, 0, 400)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > int64(q.maxBytes) {
		t.Fatalf("queue file is %d bytes, cap %d", info.Size(), q.maxBytes)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("compacted queue mode = %v, want 0600", info.Mode().Perm())
	}
}

// Compaction while a batch is in flight invalidates that batch (its prefix no
// longer exists) instead of letting its acknowledgement delete the wrong data.
func TestMetricsQueueCompactionInvalidatesInFlightBatch(t *testing.T) {
	q := NewFileMetricsQueue(filepath.Join(t.TempDir(), "metrics.jsonl")).(*fileMetricsQueue)
	q.maxSnapshots = 20
	enqueueN(t, q, 0, 20)
	batch, err := q.PeekPage(5)
	if err != nil {
		t.Fatal(err)
	}
	enqueueN(t, q, 20, 1) // crosses the cap: compacts
	if err := q.Acknowledge(batch.ID); err == nil {
		t.Fatal("acknowledging a batch invalidated by compaction must fail")
	}
	after, err := q.Peek()
	if err != nil || len(after.Snapshots) == 0 || after.Snapshots[0].ProcessCount != 0 {
		t.Fatalf("queue after refused ack = %+v, err=%v; nothing may be lost", after, err)
	}
}

// A queue written by an older agent (plain JSON lines, possibly far beyond
// the cap) is read as is and capped on the next append.
func TestMetricsQueueReadsAndCapsOldQueueFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	var old bytes.Buffer
	for i := 0; i < 50; i++ {
		line, _ := json.Marshal(proto.MetricsSnapshot{Timestamp: time.Unix(int64(i), 0).UTC(), ProcessCount: uint64(i)})
		old.Write(line)
		old.WriteByte('\n')
	}
	if err := os.WriteFile(path, old.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	q := NewFileMetricsQueue(path).(*fileMetricsQueue)
	q.maxSnapshots = 30
	batch, err := q.Peek()
	if err != nil || len(batch.Snapshots) != 50 {
		t.Fatalf("old queue read as %d snapshots, err=%v; want 50", len(batch.Snapshots), err)
	}
	if err := q.Acknowledge(batch.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, old.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	enqueueN(t, q, 50, 1)
	batch, err = q.Peek()
	if err != nil || len(batch.Snapshots) > 30 {
		t.Fatalf("old oversized queue was not capped on append: %d snapshots, err=%v", len(batch.Snapshots), err)
	}
	if last := batch.Snapshots[len(batch.Snapshots)-1].ProcessCount; last != 50 {
		t.Fatalf("newest snapshot = %d, want 50", last)
	}
}

func TestThinMetricsLinesTerminates(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 100)
	lines := [][]byte{append(big, '\n'), append(big, '\n'), append(big, '\n')}
	got := thinMetricsLines(lines, 0, 150)
	if len(got) != 1 {
		t.Fatalf("thinning to a byte cap below two lines kept %d lines, want the newest 1", len(got))
	}
}
