// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

func randomBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func quietRig(t *testing.T) *transferTestRig {
	t.Helper()
	rig := newTransferRig(t)
	go func() {
		for range rig.errCh {
		}
	}()
	return rig
}

func addSecondServer(t *testing.T, rig *transferTestRig) {
	t.Helper()
	if err := rig.app.AddServer(ServerRecord{
		Name: "loopback2", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer loopback2: %v", err)
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".fleet-") {
			t.Fatalf("temp file left behind in %s: %s", dir, e.Name())
		}
	}
}

// legacyFileManager answers like an agent from before the negotiated file
// features: it knows none of the new RPCs and ignores the new optional fields.
// Hello capabilities still claim them, which is exactly the stale-capability
// case (a reverse agent downgraded since it registered) the controller must
// survive.
type legacyFileManager struct{ agent.FileManager }

func unsupported() error {
	return &agent.RPCError{Code: "unsupported_action", Message: "action is not supported by the agent yet"}
}

func (legacyFileManager) Put(context.Context, proto.FilePutPayload) (proto.FileFinalizeResult, error) {
	return proto.FileFinalizeResult{}, unsupported()
}

func (legacyFileManager) Copy(context.Context, proto.FileCopyPayload) (proto.FileFinalizeResult, error) {
	return proto.FileFinalizeResult{}, unsupported()
}

func (legacyFileManager) Tree(context.Context, proto.FileTreePayload) (proto.FileTreeResult, error) {
	return proto.FileTreeResult{}, unsupported()
}

func (m legacyFileManager) Read(ctx context.Context, p proto.FileReadPayload) (proto.FileReadResult, error) {
	p.Stat = false
	return m.FileManager.Read(ctx, p)
}

func (m legacyFileManager) OpenWrite(ctx context.Context, p proto.FileOpenWritePayload) (proto.FileOpenWriteResult, error) {
	res, err := m.FileManager.OpenWrite(ctx, p)
	res.Chunks = nil
	return res, err
}

func (m legacyFileManager) Finalize(ctx context.Context, p proto.FileFinalizePayload) (proto.FileFinalizeResult, error) {
	p.ChunkDigest = ""
	return m.FileManager.Finalize(ctx, p)
}

// renameRaceFileManager reproduces an older agent's finalize race once: the
// temp file vanishes (renamed by a concurrent finalize) before this finalize
// renames it.
type renameRaceFileManager struct {
	agent.FileManager
	once sync.Once
}

func (m *renameRaceFileManager) Finalize(ctx context.Context, p proto.FileFinalizePayload) (proto.FileFinalizeResult, error) {
	var raced bool
	m.once.Do(func() { raced = true })
	if raced {
		// Let the real finalize tidy up, then report what the race produced.
		_, _ = m.FileManager.Finalize(ctx, proto.FileFinalizePayload{TransferID: p.TransferID, Path: p.Path, TotalSize: -1, Abort: true})
		return proto.FileFinalizeResult{}, &agent.RPCError{Code: "rename_failed", Message: "renameat: no such file or directory"}
	}
	return m.FileManager.Finalize(ctx, p)
}

func TestUploadRetriesOnceAfterRenameRace(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	rig.fileMgr.FileManager = &renameRaceFileManager{FileManager: agent.NewFileManager()}
	data := randomBytes(t, 300<<10)
	src := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "f.bin")
	if _, err := rig.app.UploadFile("loopback", src, remote, FileTransferOptions{ChunkSize: 64 << 10}, nil); err != nil {
		t.Fatalf("UploadFile after a lost rename race: %v", err)
	}
	if got, _ := os.ReadFile(remote); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

func TestSmallUploadAndDownloadTakeOneRoundTrip(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	dir := t.TempDir()
	data := randomBytes(t, 4096)
	src := filepath.Join(dir, "small.bin")
	if err := os.WriteFile(src, data, 0o640); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "small.bin")
	res, err := rig.app.UploadFile("loopback", src, remote, FileTransferOptions{}, nil)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if res.SHA256 != hexSum(data) {
		t.Fatalf("reported sha256 %s, want the real content digest %s", res.SHA256, hexSum(data))
	}
	if got := rig.fileMgr.callCount("put"); got != 1 || rig.fileMgr.writeCount() != 0 || rig.fileMgr.callCount("open_write") != 0 {
		t.Fatalf("small upload used put=%d write=%d open_write=%d; want a single file.put", got, rig.fileMgr.writeCount(), rig.fileMgr.callCount("open_write"))
	}
	if got, _ := os.ReadFile(remote); !bytes.Equal(got, data) {
		t.Fatal("uploaded content differs")
	}
	if info, _ := os.Stat(remote); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode())
	}
	if !auditDetailsContain(t, rig.app, "sha256="+hexSum(data)) {
		t.Fatal("upload audit entry lacks the real sha256")
	}

	rig.fileMgr.resetCalls()
	back := filepath.Join(t.TempDir(), "back.bin")
	if _, err := rig.app.DownloadFile("loopback", remote, back, FileTransferOptions{}, nil); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if rig.fileMgr.callCount("read") != 1 || rig.fileMgr.callCount("stat") != 0 {
		t.Fatalf("small download used read=%d stat=%d; want one read-with-stat", rig.fileMgr.callCount("read"), rig.fileMgr.callCount("stat"))
	}
	if got, _ := os.ReadFile(back); !bytes.Equal(got, data) {
		t.Fatal("downloaded content differs")
	}
	if !auditDetailsContain(t, rig.app, "back.bin (4096 bytes, sha256="+hexSum(data)) {
		t.Fatal("download audit entry lacks the real sha256")
	}
}

func TestMultiChunkUploadVerifiesThroughChunkDigests(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	data := randomBytes(t, 1<<20)
	src := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "big.bin")
	res, err := rig.app.UploadFile("loopback", src, remote, FileTransferOptions{ChunkSize: 64 << 10, Parallel: 4}, nil)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if res.ChunkDigest == "" {
		t.Fatal("agent did not verify through the chunk digest")
	}
	if res.SHA256 != hexSum(data) {
		t.Fatalf("reported sha256 %s is not the content digest", res.SHA256)
	}
	if got, _ := os.ReadFile(remote); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

// TestUploadResumeTrustsAgentRecords: a controller that lost its connection
// mid-upload resumes against the same agent process without anyone re-reading
// the prefix on the agent — the agent vouches for the chunks it verified.
func TestUploadResumeTrustsAgentRecords(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	data := randomBytes(t, 512<<10)
	src := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "payload.bin")
	opts := FileTransferOptions{Parallel: 1, ChunkSize: 64 << 10} // 8 chunks
	rig.fileMgr.setFailAfter(3)
	if _, err := rig.app.UploadFile("loopback", src, remote, opts, nil); err == nil {
		t.Fatal("expected the first upload to fail")
	}
	rig.fileMgr.setFailAfter(0)
	before := rig.fileMgr.writeCount()
	if _, err := rig.app.UploadFile("loopback", src, remote, opts, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if sent := rig.fileMgr.writeCount() - before; sent != 5 {
		t.Fatalf("resume sent %d chunks, want the 5 missing ones", sent)
	}
	if got := rig.fileMgr.callCount("probe"); got != 0 {
		t.Fatalf("resume probed %d times; the agent's own records should suffice", got)
	}
	if got, _ := os.ReadFile(remote); !bytes.Equal(got, data) {
		t.Fatal("resumed content differs")
	}
}

// TestUploadIntoExistingDirectory is the regression test for uploading onto an
// existing directory: the file lands inside it (cp/scp semantics) and nothing
// is left beside it, for both the one-shot and the chunked path.
func TestUploadIntoExistingDirectory(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "existing")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"small.txt": 100, "large.bin": 300 << 10} {
		data := randomBytes(t, size)
		src := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(src, data, 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := rig.app.UploadFile("loopback", src, dir, FileTransferOptions{ChunkSize: 64 << 10}, nil)
		if err != nil {
			t.Fatalf("%s: UploadFile onto a directory: %v", name, err)
		}
		if res.Path != filepath.Join(dir, name) {
			t.Fatalf("%s: landed at %s", name, res.Path)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, name)); !bytes.Equal(got, data) {
			t.Fatalf("%s: content differs", name)
		}
	}
	assertNoTempFiles(t, parent)
	assertNoTempFiles(t, dir)
}

func TestCopyIntoExistingDirectory(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	addSecondServer(t, rig)
	base := t.TempDir()
	src := filepath.Join(base, "src.txt")
	if err := os.WriteFile(src, []byte("copy me"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []string{"loopback", "loopback2"} {
		dir := filepath.Join(base, "into-"+dst)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.app.CopyFile("loopback", src, dst, dir, FileTransferOptions{}, nil); err != nil {
			t.Fatalf("CopyFile to %s: %v", dst, err)
		}
		syncAssertFile(t, filepath.Join(dir, "src.txt"), "copy me")
		assertNoTempFiles(t, base)
	}
}

func TestSameServerCopyStaysOnTheAgent(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	base := t.TempDir()
	data := randomBytes(t, 3<<20)
	src := filepath.Join(base, "src.bin")
	if err := os.WriteFile(src, data, 0o640); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, "dst.bin")
	res, err := rig.app.CopyFile("loopback", src, "loopback", dst, FileTransferOptions{}, nil)
	if err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if rig.fileMgr.callCount("copy") != 1 || rig.fileMgr.callCount("read") != 0 || rig.fileMgr.writeCount() != 0 {
		t.Fatalf("same-server copy relayed bytes: copy=%d read=%d write=%d", rig.fileMgr.callCount("copy"), rig.fileMgr.callCount("read"), rig.fileMgr.writeCount())
	}
	if res.SHA256 != hexSum(data) {
		t.Fatal("copy digest is not the content digest")
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("copied content differs")
	}
	if !auditDetailsContain(t, rig.app, "sha256="+hexSum(data)) {
		t.Fatal("copy audit entry lacks the sha256")
	}
	// Same-server directory copies use it for every file too.
	tree := filepath.Join(base, "tree")
	want := writeTree(t, tree, 3, 3)
	rig.fileMgr.resetCalls()
	if n, err := rig.app.CopyDir("loopback", tree, "loopback", filepath.Join(base, "tree2"), FileTransferOptions{}, nil); err != nil || n != len(want) {
		t.Fatalf("CopyDir n=%d err=%v", n, err)
	}
	if rig.fileMgr.callCount("copy") != len(want) || rig.fileMgr.callCount("read") != 0 {
		t.Fatalf("CopyDir used copy=%d read=%d", rig.fileMgr.callCount("copy"), rig.fileMgr.callCount("read"))
	}
	for rel, content := range want {
		syncAssertFile(t, filepath.Join(base, "tree2", filepath.FromSlash(rel)), content)
	}
}

func TestCrossServerRelayStreamsWithoutTempFile(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	addSecondServer(t, rig)
	base := t.TempDir()
	data := randomBytes(t, 1<<20+123)
	src := filepath.Join(base, "src.bin")
	if err := os.WriteFile(src, data, 0o640); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, "dst.bin")
	res, err := rig.app.CopyFile("loopback", src, "loopback2", dst, FileTransferOptions{ChunkSize: 64 << 10, Parallel: 4}, nil)
	if err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if res.SHA256 != hexSum(data) || res.ChunkDigest == "" {
		t.Fatalf("relay result = %+v", res)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("relayed content differs")
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0o640 {
		t.Fatalf("relay did not keep the source mode: %v", info.Mode())
	}
	if !auditDetailsContain(t, rig.app, "sha256="+hexSum(data)) {
		t.Fatal("relay audit entry lacks the sha256")
	}
	relays, _ := filepath.Glob(filepath.Join(rig.app.ConfigDir, "tmp", "fleet-relay-*"))
	if len(relays) != 0 {
		t.Fatalf("relay left controller temp files: %v", relays)
	}
	assertNoTempFiles(t, base)
}

func TestControllerRejectsDotDotPathsBeforeConnecting(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)
	base := t.TempDir()
	syncWrite(t, filepath.Join(base, "a", "b", "keep.txt"), "keep")
	src := filepath.Join(base, "a", "b", "keep.txt")
	escape := filepath.Join(base, "a", "b") + "/../.."
	for name, run := range map[string]func() error{
		"delete": func() error { return rig.app.RemoteDelete("loopback", escape, true) },
		"rename from": func() error {
			return rig.app.RemoteRename("loopback", escape, filepath.Join(base, "x"))
		},
		"rename to": func() error { return rig.app.RemoteRename("loopback", src, escape) },
		"mkdir":     func() error { return rig.app.RemoteMkdir("loopback", escape+"/new") },
		"upload": func() error {
			_, err := rig.app.UploadFile("loopback", src, escape+"/f", FileTransferOptions{}, nil)
			return err
		},
		"upload dir": func() error {
			_, err := rig.app.UploadDir("loopback", filepath.Join(base, "a"), escape, FileTransferOptions{}, nil)
			return err
		},
		"copy": func() error {
			_, err := rig.app.CopyFile("loopback", src, "loopback", escape+"/f", FileTransferOptions{}, nil)
			return err
		},
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), `".." components`) {
			t.Fatalf("%s with a .. path: err = %v", name, err)
		}
	}
	select {
	case err := <-rig.errCh:
		t.Fatalf("a dot-dot request reached the agent: %v", err)
	default:
	}
	syncAssertFile(t, src, "keep")
}

func TestCatRemoteFileStreamsLargeFilesInOrder(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	data := randomBytes(t, 5*DefaultChunkSizeBytes+777)
	remote := filepath.Join(t.TempDir(), "cat.bin")
	if err := os.WriteFile(remote, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	n, err := rig.app.CatRemoteFile("loopback", remote, &out)
	if err != nil || n != int64(len(data)) || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("CatRemoteFile n=%d err=%v equal=%v", n, err, bytes.Equal(out.Bytes(), data))
	}
}

// TestLegacyAgentFallbacks drives every negotiated feature against an agent
// that implements none of them, as an older agent would.
func TestLegacyAgentFallbacks(t *testing.T) {
	t.Parallel()
	rig := quietRig(t)
	rig.fileMgr.FileManager = legacyFileManager{agent.NewFileManager()}
	addSecondServer(t, rig)
	base := t.TempDir()

	small := randomBytes(t, 1000)
	large := randomBytes(t, 700<<10)
	for name, data := range map[string][]byte{"small": small, "large": large} {
		src := filepath.Join(base, name+".src")
		if err := os.WriteFile(src, data, 0o600); err != nil {
			t.Fatal(err)
		}
		remote := filepath.Join(base, name+".remote")
		res, err := rig.app.UploadFile("loopback", src, remote, FileTransferOptions{ChunkSize: 64 << 10}, nil)
		if err != nil {
			t.Fatalf("%s upload: %v", name, err)
		}
		if res.SHA256 != hexSum(data) || res.ChunkDigest != "" {
			t.Fatalf("%s upload result = %+v", name, res)
		}
		back := filepath.Join(base, name+".back")
		if _, err := rig.app.DownloadFile("loopback", remote, back, FileTransferOptions{ChunkSize: 64 << 10}, nil); err != nil {
			t.Fatalf("%s download: %v", name, err)
		}
		var cat bytes.Buffer
		if _, err := rig.app.CatRemoteFile("loopback", remote, &cat); err != nil || !bytes.Equal(cat.Bytes(), data) {
			t.Fatalf("%s cat: err=%v equal=%v", name, err, bytes.Equal(cat.Bytes(), data))
		}
		for _, dst := range []string{"loopback", "loopback2"} {
			copied := filepath.Join(base, name+".copy."+dst)
			if _, err := rig.app.CopyFile("loopback", remote, dst, copied, FileTransferOptions{ChunkSize: 64 << 10}, nil); err != nil {
				t.Fatalf("%s copy to %s: %v", name, dst, err)
			}
			if got, _ := os.ReadFile(copied); !bytes.Equal(got, data) {
				t.Fatalf("%s copy to %s differs", name, dst)
			}
		}
		if got, _ := os.ReadFile(back); !bytes.Equal(got, data) {
			t.Fatalf("%s round trip differs", name)
		}
	}
	tree := filepath.Join(base, "tree")
	want := writeTree(t, tree, 4, 2)
	got, err := rig.app.scanRemoteDir("loopback", tree)
	if err != nil || len(got) != len(want)+4 {
		t.Fatalf("scanRemoteDir fallback: %d entries, err=%v", len(got), err)
	}
	assertNoTempFiles(t, base)
}

func TestTransferChunkSizeFitsTheChannelWindow(t *testing.T) {
	t.Parallel()
	if DefaultChunkSizeBytes+transferChunkHeadroom != sshChannelWindowBytes {
		t.Fatalf("default chunk %d does not leave the headroom below the window", DefaultChunkSizeBytes)
	}
	app := &App{}
	for configured, want := range map[int64]int64{
		0:                      DefaultChunkSizeBytes,
		64 << 10:               64 << 10,
		2 << 20:                DefaultChunkSizeBytes, // the previous default
		proto.MaxRawChunkBytes: DefaultChunkSizeBytes,
	} {
		if got := app.resolveTransferOptions(ServerRecord{}, FileTransferOptions{ChunkSize: configured}).ChunkSize; got != want {
			t.Errorf("chunk size %d resolved to %d, want %d", configured, got, want)
		}
	}
	app.Config.Runtime.FileTransfer.ChunkSizeBytes = 2 << 20
	if got := app.resolveTransferOptions(ServerRecord{}, FileTransferOptions{}).ChunkSize; got != DefaultChunkSizeBytes {
		t.Errorf("configured 2 MiB resolved to %d", got)
	}
}

func TestChannelBudget(t *testing.T) {
	t.Parallel()
	b := &channelBudget{free: 3}
	if got := b.acquire(8); got != 3 {
		t.Fatalf("acquire(8) = %d, want 3", got)
	}
	if got := b.tryAcquire(1); got != 0 {
		t.Fatalf("tryAcquire on an empty budget = %d", got)
	}
	// An exhausted budget still hands out one channel, without waiting, so no
	// transfer can deadlock on it.
	done := make(chan int, 1)
	go func() { done <- b.acquire(2) }()
	select {
	case got := <-done:
		if got != 1 {
			t.Fatalf("acquire on an exhausted budget = %d, want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire blocked on an exhausted budget")
	}
	if got := b.tryAcquire(4); got != 0 {
		t.Fatalf("tryAcquire while over budget = %d", got)
	}
	b.release(3)
	b.release(1)
	if got := b.tryAcquire(8); got != 3 {
		t.Fatalf("tryAcquire after release = %d, want 3", got)
	}
}
