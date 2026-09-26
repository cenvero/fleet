// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func rpcCode(err error) string {
	if rerr, ok := err.(*RPCError); ok {
		return rerr.Code
	}
	return ""
}

func assertNoPartFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".fleet-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestHasDotComponent(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"/a/..", "/a/../b", "/a/./b", "/..", "/.", "/a/b/../../..", "/a/b/.."} {
		if !hasDotComponent(p) {
			t.Errorf("hasDotComponent(%q) = false", p)
		}
	}
	for _, p := range []string{"/", "/a/b", "/a/.hidden", "/a/..b", "/a/b.", "/a//b/", "/a/...", "/srv/app.d/x"} {
		if hasDotComponent(p) {
			t.Errorf("hasDotComponent(%q) = true", p)
		}
	}
}

// TestMutatingOpsRejectDotDotBeforeCleaning is the regression test for
// `fleet file rm --recursive /a/b/../../..` deleting whatever the path
// resolved to: every create/replace/remove operation refuses "." and ".."
// components before the path is cleaned, so nothing is touched.
func TestMutatingOpsRejectDotDotBeforeCleaning(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	base := t.TempDir()
	victim := filepath.Join(base, "victim")
	if err := os.MkdirAll(filepath.Join(victim, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(victim, "a", "b") + "/../.."
	dot := filepath.Join(victim, "a") + "/./b"

	for _, p := range []string{escape, dot} {
		if _, err := m.Delete(ctx, proto.FileDeletePayload{Path: p, Recursive: true}); rpcCode(err) != "invalid_path" {
			t.Fatalf("Delete(%q) error = %v, want invalid_path", p, err)
		}
		if _, err := m.Rename(ctx, proto.FileRenamePayload{From: p, To: filepath.Join(base, "moved")}); rpcCode(err) != "invalid_path" {
			t.Fatalf("Rename from %q error = %v", p, err)
		}
		if _, err := m.Rename(ctx, proto.FileRenamePayload{From: filepath.Join(victim, "keep.txt"), To: p}); rpcCode(err) != "invalid_path" {
			t.Fatalf("Rename to %q error = %v", p, err)
		}
		if _, err := m.Mkdir(ctx, proto.FileMkdirPayload{Path: p + "/new"}); rpcCode(err) != "invalid_path" {
			t.Fatalf("Mkdir(%q) error = %v", p, err)
		}
		if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: p + "/f", TotalSize: 1, TransferID: "tid"}); rpcCode(err) != "invalid_path" {
			t.Fatalf("OpenWrite(%q) error = %v", p, err)
		}
		if _, err := m.Put(ctx, proto.FilePutPayload{Path: p + "/f", Data: []byte("x"), SHA256: sumHex([]byte("x"))}); rpcCode(err) != "invalid_path" {
			t.Fatalf("Put(%q) error = %v", p, err)
		}
		if _, err := m.Copy(ctx, proto.FileCopyPayload{From: filepath.Join(victim, "keep.txt"), To: p + "/f"}); rpcCode(err) != "invalid_path" {
			t.Fatalf("Copy to %q error = %v", p, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(victim, "keep.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("victim tree was modified: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(victim, "a", "b")); err != nil {
		t.Fatalf("victim tree was modified: %v", err)
	}
	// Legitimate absolute paths keep working.
	if _, err := m.Delete(ctx, proto.FileDeletePayload{Path: filepath.Join(victim, "a"), Recursive: true}); err != nil {
		t.Fatalf("Delete of a clean path: %v", err)
	}
}

func TestDeleteRefusesFileRootItself(t *testing.T) {
	root := t.TempDir()
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewFileManager()
	if _, err := m.Delete(context.Background(), proto.FileDeletePayload{Path: root, Recursive: true}); err == nil {
		t.Fatal("deleting the whole file root was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "f")); err != nil {
		t.Fatalf("file root contents were removed: %v", err)
	}
}

// TestUploadOntoDirectoryFailsEarly: an upload whose destination is an existing
// directory must be refused before any temp file is created (it used to fail
// only at the final rename, leaving the whole .part beside the directory).
func TestUploadOntoDirectoryFailsEarly(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	parent := t.TempDir()
	dir := filepath.Join(parent, "existing")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dir, TotalSize: 3, TransferID: "tid-dir"}); rpcCode(err) != "target_is_directory" {
		t.Fatalf("OpenWrite onto a directory error = %v", err)
	}
	if _, err := m.Put(ctx, proto.FilePutPayload{Path: dir, Data: []byte("abc"), SHA256: sumHex([]byte("abc"))}); rpcCode(err) != "target_is_directory" {
		t.Fatalf("Put onto a directory error = %v", err)
	}
	assertNoPartFiles(t, parent)
}

func TestFinalizeFailureRemovesTempFile(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	content := []byte("abcdef")
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(content)), TransferID: "tid-fail"}); err != nil {
		t.Fatal(err)
	}
	if err := writeChunk(t, m, "tid-fail", dest, 0, content); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: "tid-fail", Path: dest, TotalSize: int64(len(content)), WholeSHA256: "00"}); err == nil {
		t.Fatal("finalize with a wrong checksum succeeded")
	}
	assertNoPartFiles(t, dir)
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination created by a failed finalize: %v", err)
	}
}

func TestFinalizeVerifiesChunkDigestWithoutRereading(t *testing.T) {
	t.Parallel()
	m := NewFileManager()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	content := bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes, 4 chunks of 256
	const id = "tid-chunks"
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(content)), TransferID: id}); err != nil {
		t.Fatal(err)
	}
	var list []proto.FileRangeChecksum
	for off := 0; off < len(content); off += 256 {
		end := min(off+256, len(content))
		if err := writeChunk(t, m, id, dest, int64(off), content[off:end]); err != nil {
			t.Fatal(err)
		}
		list = append(list, proto.FileRangeChecksum{Offset: int64(off), Length: int64(end - off), SHA256: sumHex(content[off:end])})
	}
	digest, err := proto.ChunkListDigest(int64(len(content)), list)
	if err != nil {
		t.Fatal(err)
	}
	// Re-opening the same transfer (a controller resuming) reports the
	// verified chunks.
	ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(content)), TransferID: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(ow.Chunks) != len(list) || ow.Chunks[2] != list[2] {
		t.Fatalf("OpenWrite chunks = %+v, want %+v", ow.Chunks, list)
	}
	// WholeSHA256 is deliberately not the content's digest: finalize must
	// verify through the chunk records alone (and echo the declared value)
	// instead of re-reading and re-hashing the file.
	tempRel := dest + ".fleet-" + id + ".part"
	res, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: id, Path: dest, TotalSize: int64(len(content)), WholeSHA256: "not-rehashed", ChunkDigest: digest})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if res.ChunkDigest != digest || res.SHA256 != "not-rehashed" {
		t.Fatalf("Finalize result = %+v", res)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatal("content mismatch")
	}
	if _, err := os.Stat(tempRel); !os.IsNotExist(err) {
		t.Fatalf("temp remains: %v", err)
	}
}

func TestFinalizeRejectsWrongOrIncompleteChunkDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := []byte("abcdefgh")
	full := []proto.FileRangeChecksum{
		{Offset: 0, Length: 4, SHA256: sumHex(content[:4])},
		{Offset: 4, Length: 4, SHA256: sumHex(content[4:])},
	}
	good, err := proto.ChunkListDigest(8, full)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		write  []int // chunk indexes actually written
		digest string
	}{
		"gap":   {write: []int{0}, digest: good},
		"wrong": {write: []int{0, 1}, digest: sumHex([]byte("other"))},
	} {
		m := NewFileManager()
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")
		if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: 8, TransferID: "tid"}); err != nil {
			t.Fatal(err)
		}
		// Size the temp file fully so only the digest check can fail.
		if err := writeChunk(t, m, "tid", dest, 4, content[4:]); err != nil {
			t.Fatal(err)
		}
		m.(*fileManager).active["tid"].forget(4, 4)
		for _, i := range tc.write {
			if err := writeChunk(t, m, "tid", dest, int64(4*i), content[4*i:4*i+4]); err != nil {
				t.Fatal(err)
			}
		}
		_, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: "tid", Path: dest, TotalSize: 8, WholeSHA256: sumHex(content), ChunkDigest: tc.digest})
		if rpcCode(err) != "chunk_digest_mismatch" {
			t.Fatalf("%s: Finalize error = %v, want chunk_digest_mismatch", name, err)
		}
		assertNoPartFiles(t, dir)
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("%s: destination installed despite digest failure", name)
		}
	}
}

func TestProbeRecordsHashedRangesForActiveUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	content := []byte("resume-me-please")
	const id = "tid-probe"
	// A previous agent process left a complete temp file behind.
	if err := os.WriteFile(dest+".fleet-"+id+".part", content, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewFileManager()
	ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(content)), TransferID: id})
	if err != nil || ow.ResumeOffset != int64(len(content)) || len(ow.Chunks) != 0 {
		t.Fatalf("OpenWrite = %+v, %v", ow, err)
	}
	res, err := m.Probe(ctx, proto.FileProbePayload{Path: dest, TransferID: id, Ranges: []proto.FileRange{{Offset: 0, Length: 8}, {Offset: 8, Length: 8}}})
	if err != nil || len(res.RangeChecksums) != 2 || res.RangeChecksums[1].SHA256 != sumHex(content[8:]) {
		t.Fatalf("Probe = %+v, %v", res, err)
	}
	// The probed ranges now count as verified, so the chunk-digest finalize
	// succeeds without any re-sent bytes.
	digest, _ := proto.ChunkListDigest(int64(len(content)), res.RangeChecksums)
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: id, Path: dest, TotalSize: int64(len(content)), ChunkDigest: digest}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatalf("content = %q", got)
	}
}

// TestOpenWriteDuringFinalizeIsBusy is the regression test for a resumed upload
// failing with rename_failed: an open_write that arrived while a finalize of
// the same transfer was still running reopened the temp file just before the
// finalize renamed it away, so the second upload could never be installed.
func TestOpenWriteDuringFinalizeIsBusy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	data := []byte("finalize-race")
	const id = "tid-busy"
	m := NewFileManager().(*fileManager)
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id}); err != nil {
		t.Fatal(err)
	}
	if err := writeChunk(t, m, id, dest, 0, data); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	au := m.active[id]
	m.mu.Unlock()
	au.mu.RLock() // a write still in flight keeps the finalize waiting
	finalized := make(chan error, 1)
	go func() {
		_, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: id, Path: dest, TotalSize: int64(len(data)), WholeSHA256: sumHex(data)})
		finalized <- err
	}()
	for {
		m.mu.Lock()
		claimed := au.finalizing
		m.mu.Unlock()
		if claimed {
			break
		}
		runtime.Gosched()
	}
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id}); rpcCode(err) != "transfer_busy" {
		au.mu.RUnlock()
		t.Fatalf("OpenWrite during finalize error = %v, want transfer_busy", err)
	}
	au.mu.RUnlock()
	if err := <-finalized; err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	// Once finished, the same transfer can be uploaded again from scratch.
	ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id})
	if err != nil || ow.ResumeOffset != 0 {
		t.Fatalf("OpenWrite after finalize = %+v, %v", ow, err)
	}
	if err := writeChunk(t, m, id, dest, 0, data); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: id, Path: dest, TotalSize: int64(len(data)), WholeSHA256: sumHex(data)}); err != nil {
		t.Fatalf("second Finalize: %v", err)
	}
}

func TestFinalizeAbortDiscardsUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	m := NewFileManager()
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: 4, TransferID: "tid-abort"}); err != nil {
		t.Fatal(err)
	}
	if err := writeChunk(t, m, "tid-abort", dest, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: "tid-abort", Path: dest, TotalSize: -1, Abort: true}); err != nil {
		t.Fatalf("abort: %v", err)
	}
	assertNoPartFiles(t, dir)
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("aborted upload was installed")
	}
	if err := writeChunk(t, m, "tid-abort", dest, 0, []byte("abcd")); err == nil {
		t.Fatal("write accepted after abort")
	}
}

func TestPutWritesAtomicallyAndVerifies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "small.txt")
	m := NewFileManager()
	data := []byte("small file")
	if _, err := m.Put(ctx, proto.FilePutPayload{Path: dest, Data: data, SHA256: sumHex([]byte("other"))}); rpcCode(err) != "checksum_mismatch" {
		t.Fatalf("Put with wrong checksum error = %v", err)
	}
	if _, err := m.Put(ctx, proto.FilePutPayload{Path: dest, Data: data}); rpcCode(err) != "checksum_mismatch" {
		t.Fatalf("Put without checksum error = %v", err)
	}
	assertNoPartFiles(t, dir)
	res, err := m.Put(ctx, proto.FilePutPayload{Path: dest, Data: data, Mode: 0o600, SHA256: sumHex(data)})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.SHA256 != sumHex(data) || res.Size != int64(len(data)) || res.Path != dest {
		t.Fatalf("Put result = %+v", res)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v err=%v", info.Mode(), err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatalf("content = %q", got)
	}
	assertNoPartFiles(t, dir)
}

func TestPutAndCopyReplaceFinalSymlinkAndRespectSandbox(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewFileManager()
	ctx := context.Background()
	src := filepath.Join(root, "src.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	for i, op := range []func(dest string) error{
		func(dest string) error {
			_, err := m.Put(ctx, proto.FilePutPayload{Path: dest, Data: []byte("payload"), SHA256: sumHex([]byte("payload"))})
			return err
		},
		func(dest string) error {
			_, err := m.Copy(ctx, proto.FileCopyPayload{From: src, To: dest})
			return err
		},
	} {
		dest := filepath.Join(root, "link"+string(rune('a'+i)))
		if err := os.Symlink(victim, dest); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if err := op(dest); err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
		if got, _ := os.ReadFile(victim); string(got) != "original" {
			t.Fatalf("op %d followed the destination symlink: %q", i, got)
		}
		if info, err := os.Lstat(dest); err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("op %d did not replace the link entry", i)
		}
		// Outside the file root is refused, in either direction.
		if err := op(filepath.Join(outside, "escape")); rpcCode(err) != "invalid_path" {
			t.Fatalf("op %d outside the sandbox error = %v", i, err)
		}
	}
	if _, err := m.Copy(ctx, proto.FileCopyPayload{From: victim, To: filepath.Join(root, "in.txt")}); rpcCode(err) != "invalid_path" {
		t.Fatalf("Copy from outside the sandbox error = %v", err)
	}
}

func TestCopyOnAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	data := bytes.Repeat([]byte("copy!"), 300_000) // 1.5 MB, several copy buffers
	if err := os.WriteFile(src, data, 0o640); err != nil {
		t.Fatal(err)
	}
	m := NewFileManager()
	dst := filepath.Join(dir, "dst.bin")
	res, err := m.Copy(ctx, proto.FileCopyPayload{From: src, To: dst})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if res.SHA256 != sumHex(data) || res.Size != int64(len(data)) {
		t.Fatalf("Copy result = %+v", res)
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0o640 {
		t.Fatalf("copy did not keep the source mode: %v", info.Mode())
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("copied content differs")
	}
	// Overwriting an existing file is atomic and allowed.
	if err := os.WriteFile(src, []byte("v2"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Copy(ctx, proto.FileCopyPayload{From: src, To: dst}); err != nil {
		t.Fatalf("Copy over existing: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "v2" {
		t.Fatalf("overwrite content = %q", got)
	}
	if _, err := m.Copy(ctx, proto.FileCopyPayload{From: src, To: dir}); rpcCode(err) != "target_is_directory" {
		t.Fatalf("Copy onto a directory error = %v", err)
	}
	if _, err := m.Copy(ctx, proto.FileCopyPayload{From: src, To: src}); rpcCode(err) != "same_file" {
		t.Fatalf("Copy onto itself error = %v", err)
	}
	if _, err := m.Copy(ctx, proto.FileCopyPayload{From: dir, To: filepath.Join(dir, "x")}); rpcCode(err) != "not_regular" {
		t.Fatalf("Copy of a directory error = %v", err)
	}
	assertNoPartFiles(t, dir)
}

func TestTreeListsRecursivelyWithoutFollowingSymlinks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	for _, p := range []string{"a/b/c.txt", "a/d.txt", ".hidden/x", "top.txt"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a", "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	m := NewFileManager()
	res, err := m.Tree(ctx, proto.FileTreePayload{Path: root, ShowHidden: true})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	got := map[string]string{}
	for _, e := range res.Entries {
		rel, _ := filepath.Rel(root, e.Path)
		got[filepath.ToSlash(rel)] = e.Type
	}
	want := map[string]string{
		"a": proto.FileEntryTypeDirectory, "a/b": proto.FileEntryTypeDirectory, "a/b/c.txt": proto.FileEntryTypeRegular,
		"a/d.txt": proto.FileEntryTypeRegular, "a/link": proto.FileEntryTypeSymlink,
		".hidden": proto.FileEntryTypeDirectory, ".hidden/x": proto.FileEntryTypeRegular, "top.txt": proto.FileEntryTypeRegular,
	}
	if len(got) != len(want) {
		t.Fatalf("Tree entries = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("entry %q = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	res, err = m.Tree(ctx, proto.FileTreePayload{Path: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Entries {
		if strings.Contains(e.Path, ".hidden") {
			t.Fatalf("hidden entry listed without ShowHidden: %s", e.Path)
		}
	}
	res, err = m.Tree(ctx, proto.FileTreePayload{Path: root, ShowHidden: true, MaxEntries: 3})
	if err != nil || !res.Truncated || len(res.Entries) != 0 {
		t.Fatalf("bounded Tree = %+v, %v; want truncated", res, err)
	}
}

func TestTreeRespectsSandbox(t *testing.T) {
	root := t.TempDir()
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)
	m := NewFileManager()
	if _, err := m.Tree(context.Background(), proto.FileTreePayload{Path: t.TempDir()}); rpcCode(err) != "invalid_path" {
		t.Fatalf("Tree outside the sandbox error = %v", err)
	}
}

func TestReadWithStat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewFileManager()
	res, err := m.Read(ctx, proto.FileReadPayload{Path: file, Length: 1024, Stat: true})
	if err != nil || res.Entry == nil || res.Entry.Type != proto.FileEntryTypeRegular || res.Entry.Size != 5 || string(res.Data) != "hello" || !res.EOF {
		t.Fatalf("Read with stat = %+v, %v", res, err)
	}
	res, err = m.Read(ctx, proto.FileReadPayload{Path: dir, Length: 1024, Stat: true})
	if err != nil || res.Entry == nil || res.Entry.Type != proto.FileEntryTypeDirectory || len(res.Data) != 0 {
		t.Fatalf("Read of a directory with stat = %+v, %v", res, err)
	}
	res, err = m.Read(ctx, proto.FileReadPayload{Path: file, Length: 1024})
	if err != nil || res.Entry != nil {
		t.Fatalf("Read without stat returned an entry: %+v, %v", res, err)
	}
}

// TestPooledReadServesIndependentBuffers checks recycled read buffers are only
// reused after release.
func TestPooledReadServesIndependentBuffers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	a := bytes.Repeat([]byte("A"), 128*1024)
	b := bytes.Repeat([]byte("B"), 128*1024)
	if err := os.WriteFile(filepath.Join(dir, "a"), a, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewFileManager().(*fileManager)
	ra, relA, err := m.readPooled(ctx, proto.FileReadPayload{Path: filepath.Join(dir, "a"), Length: int64(len(a))})
	if err != nil {
		t.Fatal(err)
	}
	rb, relB, err := m.readPooled(ctx, proto.FileReadPayload{Path: filepath.Join(dir, "b"), Length: int64(len(b))})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ra.Data, a) || !bytes.Equal(rb.Data, b) || ra.SHA256 != sumHex(a) {
		t.Fatal("pooled reads share or corrupt buffers")
	}
	relA()
	relB()
}

// assertRecordsMatchDisk checks every recorded chunk checksum against the bytes
// actually in the upload's temp file.
func assertRecordsMatchDisk(t *testing.T, au *activeUpload) {
	t.Helper()
	for _, rc := range au.sortedRecords(-1) {
		sum, err := hashRange(au.f, rc.Offset, rc.Length)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(sum[:]) != rc.SHA256 {
			t.Fatalf("record at %d+%d claims %s but the file holds other bytes", rc.Offset, rc.Length, rc.SHA256)
		}
	}
}

// TestOverlappingWritesKeepRecordsConsistent is the regression test for two
// concurrent writes to the same bytes leaving a record that describes the bytes
// the other write then put on disk: overlapping writes are now serialised, so
// the second waits until the first has written and recorded.
// Not parallel: it installs testHookAfterWriteAt.
func TestOverlappingWritesKeepRecordsConsistent(t *testing.T) {
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "out.bin")
	const id = "tid-overlap"
	m := NewFileManager().(*fileManager)
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: 12, TransferID: id}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	au := m.active[id]
	m.mu.Unlock()

	aWrote := make(chan struct{})
	releaseA := make(chan struct{})
	var first atomic.Bool
	testHookAfterWriteAt = func(int64) {
		if first.CompareAndSwap(false, true) { // only the first write (A) pauses
			close(aWrote)
			<-releaseA
		}
	}
	defer func() { testHookAfterWriteAt = nil }()

	aDone := make(chan error, 1)
	go func() { aDone <- writeChunk(t, m, id, dest, 0, []byte("AAAAAAAA")) }()
	<-aWrote
	bDone := make(chan error, 1)
	go func() { bDone <- writeChunk(t, m, id, dest, 4, []byte("BBBBBBBB")) }() // overlaps A at 4..8
	select {
	case err := <-bDone:
		close(releaseA)
		<-aDone
		t.Fatalf("overlapping write ran while another was between its write and its record (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	if err := <-bDone; err != nil {
		t.Fatal(err)
	}
	assertRecordsMatchDisk(t, au)
}

// TestConcurrentOverlappingWritesStress hammers one range from many writers and
// checks the records still describe the bytes on disk.
func TestConcurrentOverlappingWritesStress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for iter := range 50 {
		dest := filepath.Join(t.TempDir(), "out.bin")
		id := fmt.Sprintf("tid-stress-%d", iter)
		m := NewFileManager().(*fileManager)
		if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: 96 << 10, TransferID: id}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for w := range 8 {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				data := bytes.Repeat([]byte{byte('a' + w)}, 64<<10)
				off := int64(w%3) * (16 << 10)
				if err := writeChunk(t, m, id, dest, off, data); err != nil {
					t.Error(err)
				}
			}(w)
		}
		wg.Wait()
		m.mu.Lock()
		au := m.active[id]
		m.mu.Unlock()
		assertRecordsMatchDisk(t, au)
	}
}

// TestRecordOverlapWhenFirstRecordIsUnaligned is the regression test for the
// fast overlap check trusting a grid whose very first record was not aligned
// to it, which let a stale record survive a write over half its bytes.
func TestRecordOverlapWhenFirstRecordIsUnaligned(t *testing.T) {
	t.Parallel()
	au := &activeUpload{}
	var sum [sha256.Size]byte
	au.record(32<<10, 64<<10, sum)
	au.forget(0, 64<<10) // overlaps [32K, 64K) of the first record
	if got := au.sortedRecords(-1); len(got) != 0 {
		t.Fatalf("stale record survived an overlapping write: %+v", got)
	}
	au.record(0, 64<<10, sum)
	au.record(64<<10, 64<<10, sum)
	au.forget(96<<10, 8)
	if got := au.sortedRecords(-1); len(got) != 1 || got[0].Offset != 0 {
		t.Fatalf("records after an overlapping write = %+v", got)
	}
}
