// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestEditHistoryRecordListPruneAndRemove(t *testing.T) {
	t.Parallel()
	h := editHistory{dir: filepath.Join(t.TempDir(), "data", "edit-history")}
	entry := func(old, new string) editHistoryEntry {
		return editHistoryEntry{Server: "web-01", Path: "/etc/link.conf", ResolvedPath: "/etc/real.conf", OldSHA256: sha256Hex([]byte(old)), NewSHA256: sha256Hex([]byte(new)), OldSize: int64(len(old))}
	}
	for i, c := range []string{"v1", "v2", "v3"} {
		if _, err := h.record(entry(c, c+"'"), []byte(c), 2); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond) // distinct, ordered timestamps
	}
	// Only the 2 newest are kept, and they can be found by either name.
	for _, name := range []string{"/etc/real.conf", "/etc/link.conf"} {
		got, err := h.list("web-01", name)
		if err != nil || len(got) != 2 {
			t.Fatalf("list(%s) = %d entries, %v", name, len(got), err)
		}
		if got[0].OldSHA256 != sha256Hex([]byte("v3")) || got[1].OldSHA256 != sha256Hex([]byte("v2")) {
			t.Fatalf("list(%s) order/prune wrong: %+v", name, got)
		}
	}
	if got, _ := h.list("web-02", "/etc/real.conf"); len(got) != 0 {
		t.Fatal("history must be per server")
	}
	latest, _ := h.list("web-01", "/etc/real.conf")
	content, err := h.content(latest[0])
	if err != nil || string(content) != "v3" {
		t.Fatalf("content = %q, %v", content, err)
	}
	if err := h.remove(latest[0]); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.list("web-01", "/etc/real.conf"); len(got) != 1 {
		t.Fatalf("after remove: %d entries", len(got))
	}
	// Files are private to the operator.
	err = filepath.Walk(h.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEditHistoryRefusesDamagedContent(t *testing.T) {
	t.Parallel()
	h := editHistory{dir: t.TempDir()}
	if _, err := h.record(editHistoryEntry{Server: "s", Path: "/f", ResolvedPath: "/f", OldSHA256: sha256Hex([]byte("good"))}, []byte("good"), 5); err != nil {
		t.Fatal(err)
	}
	es, _ := h.list("s", "/f")
	if err := os.WriteFile(filepath.Join(h.dir, es[0].dirRel, es[0].ID+".orig"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.content(es[0]); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("damaged content should be refused, got %v", err)
	}
}

func TestFileEditSettings(t *testing.T) {
	t.Parallel()
	var s FileEditSettings
	if s.EffectiveBackups() != DefaultEditBackups || s.EffectiveMaxBytes() != proto.MaxEditFileBytes {
		t.Fatalf("defaults: %d %d", s.EffectiveBackups(), s.EffectiveMaxBytes())
	}
	zero := 0
	s.Backups = &zero
	s.MaxBytes = 1024
	if s.EffectiveBackups() != 0 || s.EffectiveMaxBytes() != 1024 || s.Validate() != nil {
		t.Fatalf("explicit: %d %d %v", s.EffectiveBackups(), s.EffectiveMaxBytes(), s.Validate())
	}
	for _, bad := range []FileEditSettings{{MaxBytes: proto.MaxEditFileBytes + 1}, {MaxBytes: -1}, {Backups: ptr(-1)}, {Backups: ptr(101)}} {
		if bad.Validate() == nil {
			t.Errorf("%+v should be invalid", bad)
		}
	}
}

func ptr(n int) *int { return &n }

func TestPermBits(t *testing.T) {
	t.Parallel()
	if got := permBits(uint32(0o644)); got != 0o644 {
		t.Fatalf("permBits(0644) = %o", got)
	}
	if got := permBits(uint32(os.FileMode(0o755) | os.ModeSetuid | os.ModeSticky)); got != 0o5755 {
		t.Fatalf("permBits(setuid|sticky 0755) = %o", got)
	}
}

// newEditRig is newTransferRig with the plain agent file manager, which
// implements file.edit (the instrumented one embeds only FileManager).
func newEditRig(t *testing.T, fm agent.FileManager) *App {
	t.Helper()
	rig := newTransferRig(t)
	configDir := rig.app.ConfigDir
	rig.app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		server := agent.Server{
			Mode:               transport.ModeDirect,
			HostKeyPath:        filepath.Join(configDir, "agent_host_key"),
			AuthorizedKeysPath: filepath.Join(configDir, "keys", "id_ed25519.pub"),
			FileManager:        fm,
		}
		clientConn, serverConn := testutil.NewBufferedConnPair("127.0.0.1:40001", "127.0.0.1:2222")
		go func() { _ = server.ServeConn(serverConn) }()
		return clientConn, nil
	}
	return rig.app
}

func TestEditRemoteFileViewEditUndo(t *testing.T) {
	t.Parallel()
	app := newEditRig(t, agent.NewFileManager())
	remote := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(remote, []byte("port=80\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(remote, 0o640); err != nil {
		t.Fatal(err)
	}
	view, err := app.ViewRemoteFile("loopback", remote)
	if err != nil || view.SHA256 != sha256Hex([]byte("port=80\n")) || view.Lines != 1 || view.ModeOctal != "0640" {
		t.Fatalf("view = %+v, %v", view, err)
	}
	res, err := app.EditRemoteFile("loopback", EditRequest{
		Path: remote, BaseSHA256: view.SHA256,
		Ops: []proto.FileEditOp{{Kind: proto.FileEditOpReplace, Old: "port=80", New: "port=8080"}},
	})
	if err != nil || !res.Changed || res.BackupID == "" || res.ModeOctal != "0640" || res.Original != nil {
		t.Fatalf("edit = %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(remote); string(got) != "port=8080\n" {
		t.Fatalf("remote = %q", got)
	}
	// The same base hash is now stale.
	_, err = app.EditRemoteFile("loopback", EditRequest{Path: remote, BaseSHA256: view.SHA256, Ops: []proto.FileEditOp{{Old: "8080", New: "9090"}}})
	if EditErrorCode(err) != "edit_conflict" {
		t.Fatalf("stale base: %v", err)
	}
	if _, err := app.UndoRemoteEdit("loopback", remote); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(remote); string(got) != "port=80\n" {
		t.Fatalf("after undo = %q", got)
	}
	if fi, _ := os.Stat(remote); fi.Mode().Perm() != 0o640 {
		t.Fatalf("undo changed the mode to %v", fi.Mode().Perm())
	}
	if _, err := app.UndoRemoteEdit("loopback", remote); err == nil {
		t.Fatal("nothing is left to undo")
	}
	// An edit made outside Fleet after a Fleet edit blocks its undo.
	if _, err := app.EditRemoteFile("loopback", EditRequest{Path: remote, Ops: []proto.FileEditOp{{Old: "80", New: "81"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remote, []byte("changed by hand\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := app.UndoRemoteEdit("loopback", remote); err == nil || !strings.Contains(err.Error(), "changed after that edit") {
		t.Fatalf("undo over a newer change must be refused: %v", err)
	}
	if got, _ := os.ReadFile(remote); string(got) != "changed by hand\n" {
		t.Fatalf("undo clobbered a newer change: %q", got)
	}
}

func TestEditRemoteFileOldAgentIsReported(t *testing.T) {
	t.Parallel()
	// A file manager without Edit behaves like an agent from before file.edit.
	app := newEditRig(t, &instrumentedFileManager{FileManager: agent.NewFileManager()})
	remote := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(remote, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := app.EditRemoteFile("loopback", EditRequest{Path: remote, Ops: []proto.FileEditOp{{Old: "a", New: "b"}}})
	if !errors.Is(err, ErrEditUnsupported) {
		t.Fatalf("err = %v, want ErrEditUnsupported", err)
	}
}

func TestEditRemoteFileRequireHash(t *testing.T) {
	t.Parallel()
	app := newEditRig(t, agent.NewFileManager())
	app.Config.Runtime.FileEdit.RequireHash = true
	_, err := app.EditRemoteFile("loopback", EditRequest{Path: "/tmp/x", Ops: []proto.FileEditOp{{Old: "a", New: "b"}}})
	if err == nil || !strings.Contains(err.Error(), "expected sha256") {
		t.Fatalf("err = %v", err)
	}
}
