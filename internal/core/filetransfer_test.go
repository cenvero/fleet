// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestMain(m *testing.M) {
	// Keep per-chunk retry backoff tiny so the resume test (which deliberately
	// fails writes) doesn't sleep through the production 5s delay.
	transferRetryDelay = 5 * time.Millisecond
	os.Exit(m.Run())
}

func TestBuildChunks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		total, chunk int64
		want         int
	}{
		{0, 10, 0},
		{10, 10, 1},
		{11, 10, 2},
		{100, 10, 10},
		{101, 10, 11},
	}
	for _, tc := range cases {
		got := buildChunks(tc.total, tc.chunk)
		if len(got) != tc.want {
			t.Fatalf("buildChunks(%d,%d) => %d chunks, want %d", tc.total, tc.chunk, len(got), tc.want)
		}
		var sum int64
		for _, c := range got {
			sum += c.length
		}
		if sum != tc.total {
			t.Fatalf("buildChunks(%d,%d) covers %d bytes, want %d", tc.total, tc.chunk, sum, tc.total)
		}
	}
}

func TestEffectiveFileTransferDefaultsMerge(t *testing.T) {
	t.Parallel()
	app := &App{Config: Config{Runtime: RuntimeConfig{FileTransfer: FileTransferDefaults{
		ParallelStreams: 6, ChunkSizeBytes: 2 << 20, RemoteDir: "/srv",
	}}}}

	// Global only.
	d := app.effectiveFileTransferDefaults(ServerRecord{})
	if d.ParallelStreams != 6 || d.ChunkSizeBytes != 2<<20 || d.RemoteDir != "/srv" {
		t.Fatalf("global merge wrong: %+v", d)
	}
	// Per-server override wins.
	d = app.effectiveFileTransferDefaults(ServerRecord{FileTransfer: FileTransferDefaults{ParallelStreams: 2, RemoteDir: "/data"}})
	if d.ParallelStreams != 2 || d.RemoteDir != "/data" || d.ChunkSizeBytes != 2<<20 {
		t.Fatalf("per-server merge wrong: %+v", d)
	}
	// Chunk clamp.
	app.Config.Runtime.FileTransfer.ChunkSizeBytes = proto.MaxRawChunkBytes * 4
	d = app.effectiveFileTransferDefaults(ServerRecord{})
	if d.ChunkSizeBytes != proto.MaxRawChunkBytes {
		t.Fatalf("chunk not clamped: %d", d.ChunkSizeBytes)
	}
}

// transferTestRig stands up an in-memory agent serving a real file manager and
// a controller App wired to dial it over a buffered conn pair.
type transferTestRig struct {
	app       *App
	fileMgr   *instrumentedFileManager
	errCh     chan error
	dialCount int
}

func newTransferRig(t *testing.T) *transferTestRig {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { app.Close() })

	rig := &transferTestRig{
		app:     app,
		fileMgr: &instrumentedFileManager{FileManager: agent.NewFileManager()},
		errCh:   make(chan error, 16),
	}
	rig.app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		server := agent.Server{
			Mode:               transport.ModeDirect,
			HostKeyPath:        filepath.Join(configDir, "agent_host_key"),
			AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
			FileManager:        rig.fileMgr,
		}
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40000", "127.0.0.1:2222")
		go func() { rig.errCh <- server.ServeConn(serverConn) }()
		return clientConn, nil
	}
	if err := app.AddServer(ServerRecord{
		Name: "loopback", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent",
	}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	return rig
}

func TestUploadDownloadRoundTripParallel(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)

	// ~1 MiB of deterministic pseudo-random data so multiple small chunks span
	// several parallel streams.
	src := make([]byte, 1<<20)
	for i := range src {
		src[i] = byte((i*2654435761 + 7) % 251)
	}
	srcSum := sha256.Sum256(src)

	localDir := t.TempDir()
	localPath := filepath.Join(localDir, "payload.bin")
	if err := os.WriteFile(localPath, src, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}
	remotePath := filepath.Join(remoteDir, "payload.bin")

	opts := FileTransferOptions{Parallel: 4, ChunkSize: 64 * 1024}
	res, err := rig.app.UploadFile("loopback", localPath, remotePath, opts, nil)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if res.SHA256 != hex.EncodeToString(srcSum[:]) {
		t.Fatalf("uploaded checksum mismatch")
	}
	uploaded, err := os.ReadFile(remotePath)
	if err != nil {
		t.Fatalf("read uploaded: %v", err)
	}
	if sha256.Sum256(uploaded) != srcSum {
		t.Fatalf("uploaded file content mismatch")
	}

	// Download it back to a new local path.
	backPath := filepath.Join(localDir, "back.bin")
	if _, err := rig.app.DownloadFile("loopback", remotePath, backPath, opts, nil); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	back, err := os.ReadFile(backPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if sha256.Sum256(back) != srcSum {
		t.Fatalf("round-trip content mismatch")
	}
}

func TestUploadResumesAfterDrop(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)

	src := make([]byte, 512*1024)
	for i := range src {
		src[i] = byte((i*40503 + 13) % 251)
	}
	srcSum := sha256.Sum256(src)
	localPath := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(localPath, src, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}
	remotePath := filepath.Join(remoteDir, "payload.bin")

	// Single stream so chunks land in order; fail after the first 3 writes to
	// simulate a mid-transfer drop. 512 KiB / 64 KiB = 8 chunks.
	chunkSize := int64(64 * 1024)
	totalChunks := 8
	rig.fileMgr.setFailAfter(3)

	opts := FileTransferOptions{Parallel: 1, ChunkSize: chunkSize}
	if _, err := rig.app.UploadFile("loopback", localPath, remotePath, opts, nil); err == nil {
		t.Fatalf("expected first upload to fail mid-transfer")
	}
	firstWrites := rig.fileMgr.writeCount()
	if firstWrites < 3 {
		t.Fatalf("expected at least 3 writes before failure, got %d", firstWrites)
	}

	// "Restart": fresh file manager (empty in-memory state), temp file persists.
	rig.fileMgr = &instrumentedFileManager{FileManager: agent.NewFileManager()}

	if _, err := rig.app.UploadFile("loopback", localPath, remotePath, opts, nil); err != nil {
		t.Fatalf("resume upload failed: %v", err)
	}
	resumeWrites := rig.fileMgr.writeCount()

	// Resume must skip the chunks already committed before the drop.
	if resumeWrites >= totalChunks {
		t.Fatalf("resume re-sent everything (%d writes); expected fewer than %d", resumeWrites, totalChunks)
	}
	if resumeWrites == 0 {
		t.Fatalf("resume wrote nothing; transfer did not complete the remainder")
	}

	got, err := os.ReadFile(remotePath)
	if err != nil {
		t.Fatalf("read resumed file: %v", err)
	}
	if sha256.Sum256(got) != srcSum {
		t.Fatalf("resumed file content mismatch")
	}
}

// instrumentedFileManager wraps a real agent file manager to count writes and
// optionally inject a failure after N successful writes. It can also corrupt
// reads by truncating the returned chunk (while keeping its SHA consistent with
// the truncated bytes) so the download path's chunk-length guard is exercised.
type instrumentedFileManager struct {
	agent.FileManager
	mu             sync.Mutex
	writes         int
	failAfter      int
	truncateReadBy int64
}

func (m *instrumentedFileManager) setFailAfter(n int) {
	m.mu.Lock()
	m.failAfter = n
	m.mu.Unlock()
}

// setTruncateReadBy makes every Read drop the last n bytes of the chunk it would
// otherwise return, then re-checksum the shortened payload. The SHA therefore
// still matches the (short) Data, so only an explicit length check can catch it.
func (m *instrumentedFileManager) setTruncateReadBy(n int64) {
	m.mu.Lock()
	m.truncateReadBy = n
	m.mu.Unlock()
}

func (m *instrumentedFileManager) writeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes
}

func (m *instrumentedFileManager) Write(ctx context.Context, p proto.FileWritePayload) (proto.FileWriteResult, error) {
	m.mu.Lock()
	m.writes++
	n := m.writes
	fail := m.failAfter
	m.mu.Unlock()
	if fail > 0 && n > fail {
		return proto.FileWriteResult{}, &agent.RPCError{Code: "injected_drop", Message: "simulated connection drop"}
	}
	return m.FileManager.Write(ctx, p)
}

func (m *instrumentedFileManager) Read(ctx context.Context, p proto.FileReadPayload) (proto.FileReadResult, error) {
	res, err := m.FileManager.Read(ctx, p)
	if err != nil {
		return res, err
	}
	m.mu.Lock()
	trunc := m.truncateReadBy
	m.mu.Unlock()
	if trunc > 0 && int64(len(res.Data)) > trunc {
		res.Data = res.Data[:int64(len(res.Data))-trunc]
		sum := sha256.Sum256(res.Data)
		res.SHA256 = hex.EncodeToString(sum[:]) // keep SHA consistent with short data
		res.Length = int64(len(res.Data))
	}
	return res, nil
}

func TestValidateTransferSize(t *testing.T) {
	t.Parallel()
	const chunk = int64(4 * 1024 * 1024)
	if err := validateTransferSize(0, chunk); err != nil {
		t.Fatalf("zero size should be allowed: %v", err)
	}
	if err := validateTransferSize(1<<30, chunk); err != nil {
		t.Fatalf("1 GiB should be allowed: %v", err)
	}
	if err := validateTransferSize(-1, chunk); err == nil {
		t.Fatal("negative size must be rejected")
	}
	if err := validateTransferSize(maxTransferFileBytes+1, chunk); err == nil {
		t.Fatal("absurd size must be rejected")
	}
	// Tiny chunk size + huge size must trip the chunk-count cap even though the
	// byte ceiling alone might not.
	if err := validateTransferSize(maxTransferFileBytes, 1); err == nil {
		t.Fatal("excessive chunk count must be rejected")
	}
}

func TestDownloadRejectsChunkLengthMismatch(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)

	src := make([]byte, 256*1024)
	for i := range src {
		src[i] = byte((i*2654435761 + 7) % 251)
	}
	remoteDir := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}
	remotePath := filepath.Join(remoteDir, "payload.bin")
	if err := os.WriteFile(remotePath, src, 0o644); err != nil {
		t.Fatalf("write remote source: %v", err)
	}

	// Force every chunk to come back one byte short (with a matching SHA). The
	// length guard must reject it even though the checksum "passes".
	rig.fileMgr.setTruncateReadBy(1)

	backPath := filepath.Join(t.TempDir(), "back.bin")
	opts := FileTransferOptions{Parallel: 1, ChunkSize: 64 * 1024}
	_, err := rig.app.DownloadFile("loopback", remotePath, backPath, opts, nil)
	if err == nil {
		t.Fatal("expected download to fail on a length-mismatched chunk")
	}
	if !strings.Contains(err.Error(), "length mismatch") {
		t.Fatalf("expected a chunk length-mismatch error, got: %v", err)
	}
}

func TestDownloadSurfacesWholeFileHash(t *testing.T) {
	t.Parallel()
	rig := newTransferRig(t)

	src := make([]byte, 300*1024)
	for i := range src {
		src[i] = byte((i*40503 + 13) % 251)
	}
	wantSum := sha256.Sum256(src)

	remoteDir := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}
	remotePath := filepath.Join(remoteDir, "payload.bin")
	if err := os.WriteFile(remotePath, src, 0o644); err != nil {
		t.Fatalf("write remote source: %v", err)
	}

	backPath := filepath.Join(t.TempDir(), "back.bin")
	opts := FileTransferOptions{Parallel: 2, ChunkSize: 64 * 1024}
	if _, err := rig.app.DownloadFile("loopback", remotePath, backPath, opts, nil); err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	// The whole-file digest is recorded in the audit trail; confirm it matches
	// the source so a caller comparing against a known hash would succeed.
	if !auditDetailsContain(t, rig.app, "sha256="+hex.EncodeToString(wantSum[:])) {
		t.Fatal("download did not surface the whole-file sha256 in the audit trail")
	}
}

// auditDetailsContain reports whether any audit entry's Details contains sub.
func auditDetailsContain(t *testing.T, app *App, sub string) bool {
	t.Helper()
	entries, err := app.AuditLog.ReadAll()
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Details, sub) {
			return true
		}
	}
	return false
}
