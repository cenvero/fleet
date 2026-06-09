// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

func TestValidateTransferPathRejectsUnsafePaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{"relative", "etc/passwd"},
		{"empty", ""},
		{"proc", "/proc/self/mem"},
		{"sys", "/sys/kernel"},
		{"dev", "/dev/sda"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, rerr := validateTransferPath(tc.path); rerr == nil {
				t.Fatalf("expected %q to be rejected", tc.path)
			}
		})
	}
}

func TestValidateTransferPathRejectsSymlinkToBlocked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "sneaky")
	// /dev/null exists on Linux and macOS so EvalSymlinks resolves it; /dev/ is
	// a blocked prefix. (A symlink to /proc would only resolve on Linux.)
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, rerr := validateTransferPath(link); rerr == nil {
		t.Fatalf("expected symlink to /dev to be rejected")
	}
}

func TestFileManagerUploadRoundTrip(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")

	content := []byte("hello world, this is a chunked upload test payload")
	transferID := "tid-roundtrip"
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(content)), TransferID: transferID}); err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	// Two out-of-order chunks to exercise WriteAt offsets.
	split := 20
	if err := writeChunk(t, m, transferID, dest, int64(split), content[split:]); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	if err := writeChunk(t, m, transferID, dest, 0, content[:split]); err != nil {
		t.Fatalf("write head: %v", err)
	}
	whole := sha256.Sum256(content)
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{
		TransferID: transferID, Path: dest, WholeSHA256: hex.EncodeToString(whole[:]), TotalSize: int64(len(content)),
	}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch: %q", got)
	}
	if _, err := os.Stat(dest + ".fleetpart"); !os.IsNotExist(err) {
		t.Fatalf("temp file should be gone after finalize")
	}
}

func TestFileManagerWriteChecksumMismatch(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "out.bin")
	transferID := "tid-bad"
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: 4, TransferID: transferID}); err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	_, err := m.Write(ctx, proto.FileWritePayload{
		TransferID: transferID, Path: dest, Offset: 0, Data: []byte("data"), SHA256: "deadbeef",
	})
	if err == nil {
		t.Fatalf("expected checksum mismatch error")
	}
}

func TestFileManagerFinalizeRejectsWrongWholeChecksum(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "out.bin")
	transferID := "tid-whole"
	content := []byte("abcdef")
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(content)), TransferID: transferID}); err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if err := writeChunk(t, m, transferID, dest, 0, content); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := m.Finalize(ctx, proto.FileFinalizePayload{
		TransferID: transferID, Path: dest, WholeSHA256: "0000", TotalSize: int64(len(content)),
	})
	if err == nil {
		t.Fatalf("expected whole-checksum mismatch")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("final file must not exist after failed finalize")
	}
}

func TestFileManagerReadRange(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src.bin")
	content := []byte("0123456789abcdef")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	res, err := m.Read(ctx, proto.FileReadPayload{Path: src, Offset: 4, Length: 6})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(res.Data) != "456789" {
		t.Fatalf("unexpected data %q", res.Data)
	}
	want := sha256.Sum256([]byte("456789"))
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("checksum mismatch")
	}
	// Read past EOF.
	res, err = m.Read(ctx, proto.FileReadPayload{Path: src, Offset: 100, Length: 10})
	if err != nil {
		t.Fatalf("Read past EOF: %v", err)
	}
	if !res.EOF || res.Length != 0 {
		t.Fatalf("expected EOF empty read, got %+v", res)
	}
}

func TestFileManagerWriteRejectsOutOfRangeOffset(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "out.bin")
	// Declare a tiny file, then try to write far beyond it (sparse-file DoS).
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: 8, TransferID: "tid"}); err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if err := writeChunk(t, m, "tid", dest, 1<<40, []byte("evil")); err == nil {
		t.Fatalf("expected out-of-range offset to be rejected")
	}
	// A TotalSize=0 upload must reject any non-empty write.
	dest2 := filepath.Join(t.TempDir(), "zero.bin")
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest2, TotalSize: 0, TransferID: "z"}); err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if err := writeChunk(t, m, "z", dest2, 0, []byte("x")); err == nil {
		t.Fatalf("expected write to a zero-size upload to be rejected")
	}
}

// TestFileManagerSameDestDistinctTransfersIsolated proves two transfers with
// different content but the SAME destination path use separate temp files and
// don't corrupt each other (temp is keyed by transfer id, not path).
func TestFileManagerSameDestDistinctTransfersIsolated(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "shared.bin")
	a := []byte("aaaaaaaaaa")
	b := []byte("bbbbbbbbbbbbb")
	for _, tc := range []struct {
		id   string
		data []byte
	}{{"ta", a}, {"tb", b}} {
		if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(tc.data)), TransferID: tc.id}); err != nil {
			t.Fatalf("OpenWrite %s: %v", tc.id, err)
		}
		if err := writeChunk(t, m, tc.id, dest, 0, tc.data); err != nil {
			t.Fatalf("write %s: %v", tc.id, err)
		}
	}
	// Both finalizes must pass their own whole-file checksum — impossible if the
	// temps were shared/interleaved.
	ha := sha256.Sum256(a)
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: "ta", Path: dest, TotalSize: int64(len(a)), WholeSHA256: hex.EncodeToString(ha[:])}); err != nil {
		t.Fatalf("finalize ta: %v", err)
	}
	hb := sha256.Sum256(b)
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: "tb", Path: dest, TotalSize: int64(len(b)), WholeSHA256: hex.EncodeToString(hb[:])}); err != nil {
		t.Fatalf("finalize tb: %v", err)
	}
}

// TestFileManagerWriteFinalizeConcurrent stresses the Write/Finalize lock so the
// race detector can confirm a write never touches a closed file handle.
func TestFileManagerWriteFinalizeConcurrent(t *testing.T) {
	t.Parallel()
	for iter := range 25 {
		m := NewFileManager()
		ctx := context.Background()
		dest := filepath.Join(t.TempDir(), "c.bin")
		data := []byte("0123456789")
		if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: "t"}); err != nil {
			t.Fatalf("OpenWrite: %v", err)
		}
		sum := sha256.Sum256(data)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = m.Write(ctx, proto.FileWritePayload{TransferID: "t", Path: dest, Offset: 0, Data: data, SHA256: hex.EncodeToString(sum[:])})
		}()
		go func() {
			defer wg.Done()
			whole := sha256.Sum256(data)
			_, _ = m.Finalize(ctx, proto.FileFinalizePayload{TransferID: "t", Path: dest, TotalSize: int64(len(data)), WholeSHA256: hex.EncodeToString(whole[:])})
		}()
		wg.Wait()
		_ = iter
	}
}

func writeChunk(t *testing.T, m FileManager, transferID, dest string, offset int64, data []byte) error {
	t.Helper()
	sum := sha256.Sum256(data)
	_, err := m.Write(context.Background(), proto.FileWritePayload{
		TransferID: transferID, Path: dest, Offset: offset, Data: data, SHA256: hex.EncodeToString(sum[:]),
	})
	return err
}
