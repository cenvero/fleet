// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func TestNewPaneSourceUsesTargetMetadataAndConfiguredRemoteDir(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := core.Initialize(core.InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	app, err := core.Open(configDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	if err := app.AddServer(core.ServerRecord{
		Name:    "windows-node",
		Address: "192.0.2.10",
		Mode:    transport.ModeDirect,
		Observed: core.ServerObservation{
			OS: "windows",
		},
		FileTransfer: core.FileTransferDefaults{RemoteDir: `D:\Fleet Data`},
	}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	remote := newPaneSource(app, "windows-node")
	if remote.pathStyle != core.TargetPathWindows {
		t.Fatalf("remote path style = %q, want windows", remote.pathStyle)
	}
	if remote.cwd != `D:\Fleet Data` {
		t.Fatalf("remote cwd = %q, want configured Windows directory", remote.cwd)
	}

	local := newPaneSource(app, "")
	if local.pathStyle != core.NativePathStyle() {
		t.Fatalf("local path style = %q, want controller-native %q", local.pathStyle, core.NativePathStyle())
	}
}

func TestWindowsDriveNavigationAndRootEntries(t *testing.T) {
	m := filesModel{right: paneState{
		source: "win", remote: true, pathStyle: core.TargetPathWindows,
		cwd: `C:\Users\operator`, selected: map[int]bool{},
	}}

	model, _ := m.enterParent(1)
	m = model.(filesModel)
	if m.right.cwd != `C:\Users` {
		t.Fatalf("first parent = %q, want C:\\Users", m.right.cwd)
	}
	model, _ = m.enterParent(1)
	m = model.(filesModel)
	if m.right.cwd != `C:\` {
		t.Fatalf("drive root = %q, want C:\\", m.right.cwd)
	}

	m.right.allItems = []fileItem{{name: "Windows", isDir: true}}
	m.reapplyPane(1)
	if len(m.right.entries) == 0 || m.right.entries[0].name == ".." {
		t.Fatalf("drive root must not contain parent entry: %#v", m.right.entries)
	}

	model, _ = m.enterParent(1)
	if got := model.(filesModel).right.cwd; got != `C:\` {
		t.Fatalf("parent of drive root = %q, want unchanged", got)
	}
}

func TestWindowsUNCNavigationAndRootEntries(t *testing.T) {
	const root = `\\server\share\`
	m := filesModel{left: paneState{
		source: "win", remote: true, pathStyle: core.TargetPathWindows,
		cwd: root + `folder\nested`, selected: map[int]bool{},
	}}

	model, _ := m.enterParent(0)
	m = model.(filesModel)
	if m.left.cwd != root+"folder" {
		t.Fatalf("UNC first parent = %q, want %q", m.left.cwd, root+"folder")
	}
	model, _ = m.enterParent(0)
	m = model.(filesModel)
	if m.left.cwd != root {
		t.Fatalf("UNC root = %q, want %q", m.left.cwd, root)
	}

	m.left.allItems = []fileItem{{name: "folder", isDir: true}}
	m.reapplyPane(0)
	if len(m.left.entries) == 0 || m.left.entries[0].name == ".." {
		t.Fatalf("UNC root must not contain parent entry: %#v", m.left.entries)
	}
}

func TestTargetAwareBreadcrumbs(t *testing.T) {
	tests := []struct {
		name string
		path string
		want []string
	}{
		{name: "drive", path: `C:\Users\operator\Documents`, want: []string{`C:\`, "Users", "operator", "Documents"}},
		{name: "unc", path: `\\server\share\folder\file`, want: []string{`\\server\share\`, "folder", "file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := breadcrumbSegments(tt.path, core.TargetPathWindows)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("breadcrumbSegments(%q) = %#v, want %#v", tt.path, got, tt.want)
			}
			plain := stripANSI(renderBreadcrumb(tt.path, core.TargetPathWindows, 120))
			for _, segment := range tt.want {
				if !strings.Contains(plain, segment) {
					t.Fatalf("rendered breadcrumb %q missing segment %q", plain, segment)
				}
			}
		})
	}
}

func TestMixedTargetPanesUseIndependentPathStyles(t *testing.T) {
	m := filesModel{
		left: paneState{
			source: "linux", remote: true, pathStyle: core.TargetPathPOSIX,
			cwd: "/srv/source", selected: map[int]bool{},
		},
		right: paneState{
			source: "windows", remote: true, pathStyle: core.TargetPathWindows,
			cwd: `D:\drop`, entries: []fileItem{{name: "incoming", isDir: true}},
			selected: map[int]bool{},
		},
	}

	srcPath := joinPath(m.left.cwd, "artifact.zip", m.left.pathStyle)
	destDir := m.destDir(1, 0)
	dstPath := joinPath(destDir, "artifact.zip", m.right.pathStyle)
	if srcPath != "/srv/source/artifact.zip" {
		t.Fatalf("POSIX source path = %q", srcPath)
	}
	if destDir != `D:\drop\incoming` {
		t.Fatalf("Windows destination directory = %q", destDir)
	}
	if dstPath != `D:\drop\incoming\artifact.zip` {
		t.Fatalf("Windows destination path = %q", dstPath)
	}
}

func TestPaneNavigationClampsAtAdvertisedBrowseBoundary(t *testing.T) {
	m := filesModel{left: paneState{
		pathStyle: core.TargetPathPOSIX,
		root:      "/srv/allowed",
		cwd:       "/srv/allowed/sub",
		selected:  map[int]bool{},
	}}
	model, _ := m.enterParent(0)
	m = model.(filesModel)
	if m.left.cwd != "/srv/allowed" {
		t.Fatalf("boundary parent = %q, want /srv/allowed", m.left.cwd)
	}
	m.left.allItems = []fileItem{{name: "child", isDir: true}}
	m.reapplyPane(0)
	if len(m.left.entries) == 0 || m.left.entries[0].name == ".." {
		t.Fatalf("browse boundary exposed parent entry: %#v", m.left.entries)
	}
	model, _ = m.enterParent(0)
	if got := model.(filesModel).left.cwd; got != "/srv/allowed" {
		t.Fatalf("parent escaped browse boundary: %q", got)
	}
	got := breadcrumbSegmentsWithin("/srv/allowed/sub/deep", "/srv/allowed", core.TargetPathPOSIX)
	want := []string{"/srv/allowed", "sub", "deep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("boundary breadcrumbs = %#v, want %#v", got, want)
	}
}

func TestStartBatchRejectsWindowsTopLevelCollisionsBeforeTransfer(t *testing.T) {
	m := filesModel{
		left:  paneState{pathStyle: core.TargetPathPOSIX, cwd: "/src", selected: map[int]bool{}},
		right: paneState{pathStyle: core.TargetPathWindows, root: `C:\Data`, cwd: `C:\Data`, selected: map[int]bool{}},
		chans: make(map[int]*transferChans),
	}
	items := []fileItem{{name: "README"}, {name: "Readme"}}
	model, cmd := m.startBatch(0, 1, items, dtCopy, -1)
	got := model.(filesModel)
	if cmd != nil {
		t.Fatal("case-colliding batch started a transfer command")
	}
	if len(got.chans) != 0 || !strings.Contains(got.status, "collision") {
		t.Fatalf("batch preflight status=%q channels=%d", got.status, len(got.chans))
	}

	model, cmd = m.startBatch(0, 1, []fileItem{{name: "CON"}}, dtCopy, -1)
	got = model.(filesModel)
	if cmd != nil || !strings.Contains(got.status, "cannot represent") {
		t.Fatalf("reserved-name batch preflight status=%q cmd=%v", got.status, cmd)
	}
}
