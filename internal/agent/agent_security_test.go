// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestLogReadRespectsFileRoot proves the log.read sandbox bypass is closed: with
// --file-root configured, reading a file inside a root succeeds while reading a
// file outside any root is rejected (previously fileLogReader.Read only blocked
// /proc,/sys,/dev and ignored the roots entirely).
func TestLogReadRespectsFileRoot(t *testing.T) {
	root := t.TempDir()
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)

	reader := defaultLogReader()
	ctx := context.Background()

	inside := filepath.Join(root, "app.log")
	if err := os.WriteFile(inside, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(ctx, proto.LogReadPayload{Path: inside}); err != nil {
		t.Fatalf("log inside the allowed root must be readable, got %v", err)
	}

	// A secret outside any allowed root (the classic /etc/shadow / host-key / SSH
	// key target) must now be denied even though it is not under /proc,/sys,/dev.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.key")
	if err := os.WriteFile(outside, []byte("BEGIN PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := reader.Read(ctx, proto.LogReadPayload{Path: outside})
	if err == nil {
		t.Fatalf("log.read of a path outside --file-root must be rejected")
	}
	if rpcErr, ok := err.(*RPCError); !ok || rpcErr.Code != "invalid_log_path" {
		t.Fatalf("expected invalid_log_path RPCError, got %T %v", err, err)
	}
}

// TestLogReadBlocksProcWithoutRoots confirms the pseudo-filesystem block still
// applies when NO --file-root is set (checkBlockedTransferPath enforces it).
func TestLogReadBlocksProcWithoutRoots(t *testing.T) {
	SetAllowedFileRoots(nil)
	reader := defaultLogReader()
	if _, err := reader.Read(context.Background(), proto.LogReadPayload{Path: "/proc/self/mem"}); err == nil {
		t.Fatalf("reading /proc must be rejected")
	}
}

// TestValidateWriteTargetRejectsPlantedSymlink proves a symlink pre-planted AT
// the write/mkdir/rename destination cannot escape the sandbox: even though the
// link itself sits inside an allowed root, it resolves outside, so the target is
// rejected.
func TestValidateWriteTargetRejectsPlantedSymlink(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	SetAllowedFileRoots([]string{root})
	defer SetAllowedFileRoots(nil)

	outsideTarget := filepath.Join(outsideDir, "escape.txt")
	if err := os.WriteFile(outsideTarget, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "planted")
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	// The link path is inside the root, but it resolves outside — must be rejected.
	if _, rerr := validateWriteTarget(link); rerr == nil {
		t.Fatalf("write target that is a symlink escaping the sandbox must be rejected")
	}

	// A plain (non-symlink) new path inside the root is still allowed.
	if _, rerr := validateWriteTarget(filepath.Join(root, "fresh.txt")); rerr != nil {
		t.Fatalf("a normal new path inside the root must be allowed, got %v", rerr)
	}
}

// TestProbeUsesCorrectTempSuffix proves the resume fallback now looks for the
// real temp name (<name>.fleet-<id>.part) that OpenWrite creates, so an
// interrupted upload's bytes are found and resume works. The old ".fleetpart"
// suffix never matched.
func TestProbeUsesCorrectTempSuffix(t *testing.T) {
	SetAllowedFileRoots(nil)
	m := NewFileManager()
	ctx := context.Background()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	transferID := "tid-resume"

	// Simulate a temp left behind by an interrupted transfer (agent restarted, so
	// it is NOT in the in-memory active map — exactly the resume path).
	partial := []byte("partial-bytes")
	temp := dest + ".fleet-" + transferID + ".part"
	if err := os.WriteFile(temp, partial, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := m.Probe(ctx, proto.FileProbePayload{Path: dest, TransferID: transferID})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Exists {
		t.Fatalf("probe should find the interrupted upload temp for resume")
	}
	if res.CurrentSize != int64(len(partial)) {
		t.Fatalf("probe should report the temp size %d, got %d", len(partial), res.CurrentSize)
	}
}
