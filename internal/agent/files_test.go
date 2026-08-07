// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strings"
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

// TestFileManagerWriteRejectsOverflowOffset proves the bound check cannot be
// bypassed by an Offset so large that Offset+len(data) overflows int64 and wraps
// negative. A naive `Offset+int64(len(data)) > totalSize` check would let such a
// write through (wrapped sum is < totalSize) and sparse-allocate near the top of
// the address space; the overflow-safe comparison must still reject it.
func TestFileManagerWriteRejectsOverflowOffset(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "ovf.bin")
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: 16, TransferID: "ovf"}); err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	// math.MaxInt64 + len("evil") wraps to a negative int64; the write must be
	// rejected rather than slipping past the size bound.
	if err := writeChunk(t, m, "ovf", dest, math.MaxInt64, []byte("evil")); err == nil {
		t.Fatalf("expected overflow offset to be rejected")
	}
	if rpcErr, ok := lastWriteErr(m, ctx, dest, math.MaxInt64, []byte("evil")).(*RPCError); !ok || rpcErr.Code != "offset_out_of_range" {
		t.Fatalf("expected offset_out_of_range RPCError for overflow write")
	}
	// Exactly filling the declared size still succeeds (boundary: Offset == total-len).
	if err := writeChunk(t, m, "ovf", dest, 0, []byte("0123456789abcdef")); err != nil {
		t.Fatalf("a write that exactly fills the declared size must succeed, got %v", err)
	}
	// One byte past the end must be rejected.
	if err := writeChunk(t, m, "ovf", dest, 16, []byte("x")); err == nil {
		t.Fatalf("a write one byte past the declared size must be rejected")
	}
}

// lastWriteErr re-runs a write and returns its error, so a test can assert on the
// concrete RPCError code without duplicating the Write payload construction.
func lastWriteErr(m FileManager, ctx context.Context, dest string, offset int64, data []byte) error {
	sum := sha256.Sum256(data)
	_, err := m.Write(ctx, proto.FileWritePayload{
		TransferID: "ovf", Path: dest, Offset: offset, Data: data, SHA256: hex.EncodeToString(sum[:]),
	})
	return err
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

// TestMkdirAndLinkEntrySemantics proves mkdir refuses an escaping final link,
// while delete and rename act on symlink entries without dereferencing targets.
func TestMkdirAndLinkEntrySemantics(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // outside the sandbox
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)

	m := NewFileManager()
	ctx := context.Background()

	// Plant a symlink inside the sandbox whose target escapes it. Its own path is
	// inside `root` (so validateWriteTarget's parent-resolve passes), but the link
	// resolves to `outside`.
	escapeTarget := filepath.Join(outside, "victim")
	if err := os.MkdirAll(escapeTarget, 0o755); err != nil {
		t.Fatal(err)
	}

	// Mkdir onto a planted symlink-to-outside must be refused.
	mkLink := filepath.Join(root, "mkdir-link")
	if err := os.Symlink(escapeTarget, mkLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := m.Mkdir(ctx, proto.FileMkdirPayload{Path: mkLink}); err == nil {
		t.Fatalf("Mkdir onto an escaping symlink must be refused")
	}

	// Rename onto a symlink replaces the link entry itself; it must not operate on
	// or create anything at the symlink target outside the sandbox.
	src := filepath.Join(root, "src.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideRenameTarget := filepath.Join(outside, "renamed")
	renLink := filepath.Join(root, "rename-link")
	if err := os.Symlink(outsideRenameTarget, renLink); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rename(ctx, proto.FileRenamePayload{From: src, To: renLink}); err != nil {
		t.Fatalf("Rename should replace the symlink entry: %v", err)
	}
	if got, err := os.ReadFile(renLink); err != nil || string(got) != "x" {
		t.Fatalf("renamed entry mismatch: %q err=%v", got, err)
	}
	if _, err := os.Stat(outsideRenameTarget); !os.IsNotExist(err) {
		t.Fatalf("rename followed the symlink target: %v", err)
	}

	// Renaming a symlink source moves the link entry itself and preserves its
	// target text; it must not move or modify the outside target.
	outsideSourceTarget := filepath.Join(outside, "source-target.txt")
	if err := os.WriteFile(outsideSourceTarget, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceLink := filepath.Join(root, "source-link")
	movedLink := filepath.Join(root, "moved-link")
	if err := os.Symlink(outsideSourceTarget, sourceLink); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rename(ctx, proto.FileRenamePayload{From: sourceLink, To: movedLink}); err != nil {
		t.Fatalf("Rename symlink source: %v", err)
	}
	if _, err := os.Lstat(sourceLink); !os.IsNotExist(err) {
		t.Fatalf("source link still exists: %v", err)
	}
	if target, err := os.Readlink(movedLink); err != nil || target != outsideSourceTarget {
		t.Fatalf("moved link target = %q err=%v", target, err)
	}
	if got, _ := os.ReadFile(outsideSourceTarget); string(got) != "outside" {
		t.Fatalf("rename modified outside source target: %q", got)
	}

	// Deleting a symlink removes the link entry, not its outside target.
	deleteLink := filepath.Join(root, "delete-link")
	if err := os.Symlink(escapeTarget, deleteLink); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete(ctx, proto.FileDeletePayload{Path: deleteLink, Recursive: true}); err != nil {
		t.Fatalf("Delete should remove the symlink entry: %v", err)
	}
	if _, err := os.Lstat(deleteLink); !os.IsNotExist(err) {
		t.Fatalf("delete link entry remains: %v", err)
	}
	if _, err := os.Stat(escapeTarget); err != nil {
		t.Fatalf("escape target was modified/removed: %v", err)
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

func TestFileManagerRejectsUnsafeTransferIDs(t *testing.T) {
	m := NewFileManager()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "out.bin")
	bad := []string{"../escape", "a/b", `a\\b`, ".", strings.Repeat("a", 129)}
	for _, id := range bad {
		if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: 1, TransferID: id}); err == nil {
			t.Errorf("OpenWrite accepted unsafe transfer ID %q", id)
		}
		if _, err := m.Probe(ctx, proto.FileProbePayload{Path: dest, TransferID: id}); err == nil {
			t.Errorf("Probe accepted unsafe transfer ID %q", id)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsafe IDs created filesystem entries: %v", entries)
	}
}

func TestFileManagerFinalizeReplacesFinalSymlinkEntry(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "out.bin")
	if err := os.Symlink(victim, dest); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	m := NewFileManager()
	ctx := context.Background()
	data := []byte("uploaded")
	const id = "safe-transfer-id"
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id}); err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if err := writeChunk(t, m, id, dest, 0, data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sum := sha256.Sum256(data)
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id, WholeSHA256: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "original" {
		t.Fatalf("symlink target was clobbered: %q", got)
	}
	if info, err := os.Lstat(dest); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("destination link entry was not replaced: err=%v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "uploaded" {
		t.Fatalf("uploaded content mismatch: %q", got)
	}
}
