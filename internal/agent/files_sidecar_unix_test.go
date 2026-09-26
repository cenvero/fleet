// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// nobodyUID stands in for another local user on the managed server.
const nobodyUID = 65534

func fileUID(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Uid
}

// uploadWhole writes data as one verified chunk and finalizes through the
// chunk-digest path (the path that no longer re-reads the file).
func uploadWhole(t *testing.T, m FileManager, id, dest string, data []byte) (proto.FileFinalizeResult, error) {
	t.Helper()
	if err := writeChunk(t, m, id, dest, 0, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	digest, err := proto.ChunkListDigest(int64(len(data)), []proto.FileRangeChecksum{{Offset: 0, Length: int64(len(data)), SHA256: sumHex(data)}})
	if err != nil {
		t.Fatal(err)
	}
	return m.Finalize(context.Background(), proto.FileFinalizePayload{
		TransferID: id, Path: dest, TotalSize: int64(len(data)), WholeSHA256: sumHex(data), ChunkDigest: digest,
	})
}

// TestOpenWriteIgnoresSidecarOwnedByAnotherUser: a local user who pre-creates
// the predictable <dest>.fleet-<id>.part in a shared directory must neither own
// the installed file nor get their bytes installed; their file is left alone.
func TestOpenWriteIgnoresSidecarOwnedByAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a file owned by another user")
	}
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o1777); err != nil { // a shared, world-writable directory
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.bin")
	const id = "tid-planted"
	planted := dest + ".fleet-" + id + ".part"
	evil := []byte("EVILEVIL")
	if err := os.WriteFile(planted, evil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(planted, nobodyUID, nobodyUID); err != nil {
		t.Fatal(err)
	}

	m := NewFileManager()
	data := []byte("realdata")
	ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id})
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if ow.ResumeOffset != 0 || ow.TempPath == planted {
		t.Fatalf("resumed into another user's sidecar: %+v", ow)
	}
	if fileUID(t, ow.TempPath) != 0 {
		t.Fatalf("fresh temp file owned by uid %d", fileUID(t, ow.TempPath))
	}
	// A probe never reports the planted file's bytes.
	res, err := m.Probe(ctx, proto.FileProbePayload{Path: dest, TransferID: id, Ranges: []proto.FileRange{{Offset: 0, Length: int64(len(data))}}})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for _, rc := range res.RangeChecksums {
		if rc.SHA256 == sumHex(evil) {
			t.Fatal("probe hashed the planted sidecar")
		}
	}
	if _, err := uploadWhole(t, m, id, dest, data); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatalf("installed %q", got)
	}
	if uid := fileUID(t, dest); uid != 0 {
		t.Fatalf("installed file is owned by uid %d, want the agent's", uid)
	}
	if got, err := os.ReadFile(planted); err != nil || !bytes.Equal(got, evil) || fileUID(t, planted) != nobodyUID {
		t.Fatalf("the other user's file was modified: %q %v", got, err)
	}
	// Aborting by the predictable name must not delete their file either.
	if _, err := m.Finalize(ctx, proto.FileFinalizePayload{TransferID: id, Path: dest, TotalSize: -1, Abort: true}); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if _, err := os.Stat(planted); err != nil {
		t.Fatalf("abort removed another user's file: %v", err)
	}
}

// TestOpenWriteReusesOnlyPrivateSidecars covers the non-root variants: only a
// regular file owned by the agent, with one link and mode 0600 or tighter, is
// resumed; a hard-linked, loose or symlinked one is ignored and left intact.
func TestOpenWriteReusesOnlyPrivateSidecars(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := []byte("fresh-bytes!")
	for name, prep := range map[string]func(t *testing.T, planted string){
		"hard link": func(t *testing.T, planted string) {
			if err := os.WriteFile(planted, []byte("linkedbytes!"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(planted, planted+".other"); err != nil {
				t.Skipf("hard links unsupported: %v", err)
			}
		},
		"group readable": func(t *testing.T, planted string) {
			if err := os.WriteFile(planted, []byte("loosebytes!!"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(planted, 0o640); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, planted string) {
			target := filepath.Join(filepath.Dir(planted), "target")
			if err := os.WriteFile(target, []byte("targetbytes!"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, planted); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "out.bin")
			const id = "tid-variant"
			planted := dest + ".fleet-" + id + ".part"
			prep(t, planted)
			before, _ := os.ReadFile(planted)
			m := NewFileManager()
			ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id})
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			if ow.ResumeOffset != 0 || ow.TempPath == planted {
				t.Fatalf("reused an untrusted sidecar: %+v", ow)
			}
			if _, err := uploadWhole(t, m, id, dest, data); err != nil {
				t.Fatalf("Finalize: %v", err)
			}
			if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
				t.Fatalf("installed %q", got)
			}
			if after, _ := os.ReadFile(planted); !bytes.Equal(after, before) {
				t.Fatalf("untrusted sidecar was modified: %q -> %q", before, after)
			}
		})
	}

	t.Run("private sidecar resumes", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")
		const id = "tid-own"
		own := dest + ".fleet-" + id + ".part"
		if err := os.WriteFile(own, data[:6], 0o600); err != nil {
			t.Fatal(err)
		}
		m := NewFileManager()
		ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id})
		if err != nil || ow.ResumeOffset != 6 || ow.TempPath != own {
			t.Fatalf("own sidecar not resumed: %+v %v", ow, err)
		}
	})
}

// TestOpenWriteDropsTamperedInMemoryUpload: when a resumed attempt finds that
// the temp file of the upload still open in this process was unlinked or
// replaced meanwhile, it starts afresh instead of resuming into a file that
// could never be installed, and leaves whatever now has the name alone.
func TestOpenWriteDropsTamperedInMemoryUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	const id = "tid-tampered"
	data := []byte("0123456789ab")
	m := NewFileManager()
	ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeChunk(t, m, id, dest, 0, data[:6]); err != nil {
		t.Fatal(err)
	}
	// Another user replaces the temp file's name while the controller is away.
	if err := os.Remove(ow.TempPath); err != nil {
		t.Fatal(err)
	}
	theirs := []byte("xxxxxxxxxxxx")
	if err := os.WriteFile(ow.TempPath, theirs, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: id})
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if again.ResumeOffset != 0 || len(again.Chunks) != 0 || again.TempPath == ow.TempPath {
		t.Fatalf("resumed into a tampered upload: %+v", again)
	}
	if _, err := uploadWhole(t, m, id, dest, data); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatalf("installed %q", got)
	}
	if got, _ := os.ReadFile(ow.TempPath); !bytes.Equal(got, theirs) {
		t.Fatal("the file that took the temp name was modified")
	}
}

// TestProbeHashesTheUploadDescriptor: after the temp file's name is swapped for
// another file, a resume probe still reports the upload's own bytes.
func TestProbeHashesTheUploadDescriptor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	const id = "tid-swap-probe"
	ours := []byte("our-bytes-12")
	temp := dest + ".fleet-" + id + ".part"
	if err := os.WriteFile(temp, ours, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewFileManager()
	if _, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(ours)), TransferID: id}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, filepath.Join(dir, "moved-away")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("their-bytes!"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := m.Probe(ctx, proto.FileProbePayload{Path: dest, TransferID: id, Ranges: []proto.FileRange{{Offset: 0, Length: int64(len(ours))}}})
	if err != nil || len(res.RangeChecksums) != 1 || res.RangeChecksums[0].SHA256 != sumHex(ours) {
		t.Fatalf("Probe = %+v, %v; want the digest of the upload's own bytes", res, err)
	}
}

// TestFinalizeRefusesTempThatIsNoLongerPrivate: a hard link made to the temp
// file during the upload, or its name swapped for another file, stops the
// install — the chunk-digest path no longer re-reads the file, so this is
// checked explicitly.
func TestFinalizeRefusesTempThatIsNoLongerPrivate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := []byte("payload-data")

	t.Run("hard link", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")
		m := NewFileManager()
		ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: "tid-link"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Link(ow.TempPath, filepath.Join(dir, "sneaky")); err != nil {
			t.Skipf("hard links unsupported: %v", err)
		}
		if _, err := uploadWhole(t, m, "tid-link", dest, data); rpcCode(err) != "temp_not_private" {
			t.Fatalf("Finalize error = %v, want temp_not_private", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("a hard-linked temp file was installed")
		}
	})

	t.Run("swapped name", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "out.bin")
		m := NewFileManager()
		ow, err := m.OpenWrite(ctx, proto.FileOpenWritePayload{Path: dest, TotalSize: int64(len(data)), TransferID: "tid-swap"})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeChunk(t, m, "tid-swap", dest, 0, data); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(ow.TempPath, filepath.Join(dir, "moved-away")); err != nil {
			t.Fatal(err)
		}
		theirs := []byte("THEIR-BYTES!")
		if err := os.WriteFile(ow.TempPath, theirs, 0o600); err != nil {
			t.Fatal(err)
		}
		digest, _ := proto.ChunkListDigest(int64(len(data)), []proto.FileRangeChecksum{{Offset: 0, Length: int64(len(data)), SHA256: sumHex(data)}})
		_, err = m.Finalize(ctx, proto.FileFinalizePayload{TransferID: "tid-swap", Path: dest, TotalSize: int64(len(data)), ChunkDigest: digest})
		if rpcCode(err) != "temp_replaced" {
			t.Fatalf("Finalize error = %v, want temp_replaced", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("a swapped-in file was installed")
		}
		if got, _ := os.ReadFile(ow.TempPath); !bytes.Equal(got, theirs) {
			t.Fatal("cleanup removed or changed the file that was swapped in")
		}
	})
}
