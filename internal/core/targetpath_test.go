// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTargetPathStyleOperationsAreHostIndependent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		style    TargetPathStyle
		input    string
		clean    string
		dir      string
		base     string
		absolute bool
		root     bool
		joinBase string
		joinName string
		joined   string
	}{
		{name: "posix file", style: TargetPathPOSIX, input: "/var/lib/../log/app.log", clean: "/var/log/app.log", dir: "/var/log", base: "app.log", absolute: true, joinBase: "/var/log", joinName: "sub/file", joined: "/var/log/sub/file"},
		{name: "posix root", style: TargetPathPOSIX, input: "/", clean: "/", dir: "/", base: "/", absolute: true, root: true, joinBase: "/", joinName: "tmp", joined: "/tmp"},
		{name: "windows drive file", style: TargetPathWindows, input: `C:/Users/Mona/../Public/file.txt`, clean: `C:\Users\Public\file.txt`, dir: `C:\Users\Public`, base: "file.txt", absolute: true, joinBase: `C:\Users\Public`, joinName: `sub/file.txt`, joined: `C:\Users\Public\sub\file.txt`},
		{name: "windows drive root", style: TargetPathWindows, input: `C:\`, clean: `C:\`, dir: `C:\`, base: `\`, absolute: true, root: true, joinBase: `C:\`, joinName: "Temp", joined: `C:\Temp`},
		{name: "windows UNC file", style: TargetPathWindows, input: `\\host\share\dir\..\file.txt`, clean: `\\host\share\file.txt`, dir: `\\host\share\`, base: "file.txt", absolute: true, joinBase: `\\host\share\`, joinName: `folder/file.txt`, joined: `\\host\share\folder\file.txt`},
		{name: "windows UNC root", style: TargetPathWindows, input: `\\host\share`, clean: `\\host\share\`, dir: `\\host\share\`, base: `\`, absolute: true, root: true, joinBase: `\\host\share`, joinName: "folder", joined: `\\host\share\folder`},
		{name: "windows drive relative", style: TargetPathWindows, input: `C:folder\file.txt`, clean: `C:folder\file.txt`, dir: `C:folder`, base: "file.txt", absolute: false, joinBase: `C:folder`, joinName: "child", joined: `C:folder\child`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.style.Clean(tt.input); got != tt.clean {
				t.Fatalf("Clean(%q) = %q, want %q", tt.input, got, tt.clean)
			}
			if got := tt.style.Dir(tt.input); got != tt.dir {
				t.Fatalf("Dir(%q) = %q, want %q", tt.input, got, tt.dir)
			}
			if got := tt.style.Base(tt.input); got != tt.base {
				t.Fatalf("Base(%q) = %q, want %q", tt.input, got, tt.base)
			}
			if got := tt.style.IsAbs(tt.input); got != tt.absolute {
				t.Fatalf("IsAbs(%q) = %v, want %v", tt.input, got, tt.absolute)
			}
			if got := tt.style.IsRoot(tt.input); got != tt.root {
				t.Fatalf("IsRoot(%q) = %v, want %v", tt.input, got, tt.root)
			}
			if got := tt.style.Join(tt.joinBase, tt.joinName); got != tt.joined {
				t.Fatalf("Join(%q, %q) = %q, want %q", tt.joinBase, tt.joinName, got, tt.joined)
			}
		})
	}
}

func TestTargetPathRelativeRejectsEscapesAndDifferentVolumes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		style  TargetPathStyle
		root   string
		target string
		want   string
		bad    bool
	}{
		{name: "posix descendant", style: TargetPathPOSIX, root: "/srv/root", target: "/srv/root/sub/file", want: "sub/file"},
		{name: "posix same", style: TargetPathPOSIX, root: "/srv/root", target: "/srv/root", want: "."},
		{name: "posix sibling prefix", style: TargetPathPOSIX, root: "/srv/root", target: "/srv/rooted/file", bad: true},
		{name: "windows descendant", style: TargetPathWindows, root: `C:\Root`, target: `c:\root\Sub\file.txt`, want: "Sub/file.txt"},
		{name: "windows sibling prefix", style: TargetPathWindows, root: `C:\Root`, target: `C:\Rooted\file.txt`, bad: true},
		{name: "windows other drive", style: TargetPathWindows, root: `C:\Root`, target: `D:\Root\file.txt`, bad: true},
		{name: "windows UNC descendant", style: TargetPathWindows, root: `\\host\share\root`, target: `\\HOST\SHARE\root\a\b`, want: "a/b"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.style.Relative(tt.root, tt.target)
			if tt.bad {
				if err == nil {
					t.Fatalf("Relative(%q, %q) = %q, want error", tt.root, tt.target, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Relative(%q, %q): %v", tt.root, tt.target, err)
			}
			if got != tt.want {
				t.Fatalf("Relative(%q, %q) = %q, want %q", tt.root, tt.target, got, tt.want)
			}
		})
	}
}

func TestInitialRemotePathUsesTargetMetadata(t *testing.T) {
	t.Parallel()
	windows := ServerRecord{Observed: ServerObservation{OS: "windows"}}
	if got := InitialRemotePath(windows); got != `C:\` {
		t.Fatalf("Windows default root = %q, want C:\\", got)
	}
	windows.Metrics.DiskPath = `D:\`
	if got := InitialRemotePath(windows); got != `D:\` {
		t.Fatalf("Windows observed metrics root = %q, want D:\\", got)
	}
	windows.Observed.FileRoot = `E:\`
	if got := InitialRemotePath(windows); got != `E:\` {
		t.Fatalf("Windows advertised root = %q, want E:\\", got)
	}
	windows.FileTransfer.RemoteDir = `E:/Data`
	if got := InitialRemotePath(windows); got != `E:\Data` {
		t.Fatalf("Windows configured path = %q, want E:\\Data", got)
	}
	windows.FileTransfer.RemoteDir = `F:/Outside`
	if got := InitialRemotePath(windows); got != `E:\` {
		t.Fatalf("Windows out-of-bound configured path = %q, want advertised E:\\", got)
	}
	linux := ServerRecord{Observed: ServerObservation{OS: "linux"}}
	if got := InitialRemotePath(linux); got != "/" {
		t.Fatalf("Linux default root = %q, want /", got)
	}
	linux.Observed.FileRoot = "/srv/confined"
	if got := InitialRemotePath(linux); got != "/srv/confined" {
		t.Fatalf("Linux advertised root = %q, want /srv/confined", got)
	}
	linux.FileTransfer.RemoteDir = "/outside"
	if got := InitialRemotePath(linux); got != "/srv/confined" {
		t.Fatalf("Linux out-of-bound configured path = %q, want /srv/confined", got)
	}
	if got := RemotePathBoundary(linux); got != "/srv/confined" {
		t.Fatalf("Linux browse boundary = %q, want /srv/confined", got)
	}
	if got := RemotePathBoundary(windows); got != `E:\` {
		t.Fatalf("Windows browse boundary = %q, want E:\\", got)
	}
}

func TestTargetPathStyleForServerFallsBackToConfiguredPath(t *testing.T) {
	t.Parallel()
	server := ServerRecord{FileTransfer: FileTransferDefaults{RemoteDir: `D:\Data`}}
	if got := TargetPathStyleForServer(server); got != TargetPathWindows {
		t.Fatalf("style = %q, want windows", got)
	}
	if got := RemotePathBoundary(server); got != `D:\` {
		t.Fatalf("configured Windows boundary = %q, want D:\\", got)
	}
}

func TestValidateTargetPathComponent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"file.txt", "README", "résumé 2026.txt", ".hidden"} {
		if err := ValidateTargetPathComponent(TargetPathPOSIX, name); err != nil {
			t.Errorf("POSIX component %q rejected: %v", name, err)
		}
		if err := ValidateTargetPathComponent(TargetPathWindows, name); err != nil {
			t.Errorf("Windows component %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "line\nfeed"} {
		if err := ValidateTargetPathComponent(TargetPathPOSIX, name); err == nil {
			t.Errorf("unsafe portable component %q accepted", name)
		}
	}
	for _, name := range []string{"CON", "con.txt", "COM1", "LPT9.log", `x:stream`, "trail.", "trail ", "wild*card", "question?", `quote"`} {
		if err := ValidateTargetPathComponent(TargetPathWindows, name); err == nil {
			t.Errorf("Windows-invalid component %q accepted", name)
		}
	}
	for _, name := range []string{"CON", `x:stream`, "trail.", "wild*card"} {
		if err := ValidateTargetPathComponent(TargetPathPOSIX, name); err != nil {
			t.Errorf("valid POSIX component %q rejected: %v", name, err)
		}
	}
}

func TestValidateTargetPathChecksAllDestinationComponents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		style TargetPathStyle
		path  string
	}{
		{TargetPathPOSIX, "/srv/data/CON/x:stream"},
		{TargetPathWindows, `C:\Data\valid.txt`},
		{TargetPathWindows, `\\server\share\folder\valid.txt`},
	} {
		if err := ValidateTargetPath(tc.style, tc.path); err != nil {
			t.Errorf("ValidateTargetPath(%q, %q): %v", tc.style, tc.path, err)
		}
	}
	for _, p := range []string{`C:\CON`, `D:\Data\x:stream`, `\\server\share\trailing.`, `C:\Data\wild*card`} {
		if err := ValidateTargetPath(TargetPathWindows, p); err == nil {
			t.Errorf("Windows-invalid destination %q accepted", p)
		}
	}
	if err := ValidateTargetPath(TargetPathPOSIX, `/srv/bad\name`); err == nil {
		t.Fatal("ambiguous POSIX backslash component accepted")
	}
}

func TestLocalPathCaseInsensitiveMatchesFilesystem(t *testing.T) {
	dir := t.TempDir()
	lower := filepath.Join(dir, "fleet-case-observation")
	if err := os.WriteFile(lower, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(dir, "FLEET-CASE-OBSERVATION"))
	observed := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	got, err := LocalPathCaseInsensitive(filepath.Join(dir, "future", "destination"))
	if err != nil {
		t.Fatal(err)
	}
	if got != observed {
		t.Fatalf("LocalPathCaseInsensitive = %v, observed aliasing = %v", got, observed)
	}
}
