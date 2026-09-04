// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/update"
)

func TestDetectInstallManager(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want InstallManager
	}{
		{name: "apple silicon homebrew", path: "/opt/homebrew/bin/fleet", want: InstallManagerHomebrew},
		{name: "intel homebrew cellar", path: "/usr/local/Cellar/cenvero-fleet/2.4.1/bin/fleet", want: InstallManagerHomebrew},
		{name: "linuxbrew", path: "/home/linuxbrew/.linuxbrew/bin/fleet", want: InstallManagerHomebrew},
		{name: "winget user package", path: `C:\Users\operator\AppData\Local\Microsoft\WinGet\Packages\Cenvero.Fleet_Microsoft.Winget.Source_8wekyb3d8bbwe\fleet.exe`, want: InstallManagerWinGet},
		{name: "winget user alias", path: `C:\Users\operator\AppData\Local\Microsoft\WinGet\Links\fleet.exe`, want: InstallManagerWinGet},
		{name: "winget machine package", path: `C:\Program Files\WinGet\Packages\Cenvero.Fleet\fleet.exe`, want: InstallManagerWinGet},
		{name: "powershell installer", path: `C:\Users\operator\.local\bin\fleet.exe`, want: InstallManagerSelfManaged},
		{name: "unix self managed", path: "/usr/local/bin/fleet", want: InstallManagerSelfManaged},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DetectInstallManager(tt.path); got != tt.want {
				t.Fatalf("DetectInstallManager(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestInstallManagerCommands(t *testing.T) {
	t.Parallel()

	if got := InstallManagerHomebrew.UpgradeCommand(); got != "brew update && brew upgrade cenvero-fleet" {
		t.Fatalf("Homebrew upgrade command = %q", got)
	}
	if got := InstallManagerWinGet.UpgradeCommand(); !strings.Contains(got, "winget upgrade") || !strings.Contains(got, WinGetPackageIdentifier) {
		t.Fatalf("WinGet upgrade command = %q", got)
	}
	if got := InstallManagerWinGet.UninstallCommand(); !strings.Contains(got, "winget uninstall") || !strings.Contains(got, WinGetPackageIdentifier) {
		t.Fatalf("WinGet uninstall command = %q", got)
	}
	if got := InstallManagerSelfManaged.UpgradeCommand(); got != "fleet update apply" {
		t.Fatalf("self-managed upgrade command = %q", got)
	}
	if InstallManagerSelfManaged.ManagesController() {
		t.Fatal("self-managed install unexpectedly reports external controller ownership")
	}
}

func TestWindowsPathDetectionIsHostIndependent(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("covered by Windows path semantics directly")
	}
	path := `C:\Users\operator\AppData\Local\Microsoft\WinGet\Packages\Cenvero.Fleet\fleet.exe`
	if got := DetectInstallManager(path); got != InstallManagerWinGet {
		t.Fatalf("Windows path detection on %s host = %q", runtime.GOOS, got)
	}
}

func TestPackageManagerAllowsManagedAgentAutoUpdatePolicy(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig(t.TempDir())
	cfg.Updates.Channel = "stable"
	cfg.Updates.Policy = update.PolicyAutoUpdate
	for _, manager := range []InstallManager{InstallManagerHomebrew, InstallManagerWinGet} {
		for _, issue := range configStructuralIssues(&cfg, manager) {
			if issue.Field == "updates.policy" {
				t.Fatalf("%s install incorrectly rejected managed-agent auto-update: %s", manager, issue.Description)
			}
		}
	}
}

func TestDetectInstallManagerResolvesHomebrewSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "usr", "local", "Cellar", "cenvero-fleet", "2.4.1", "bin", "fleet")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "usr", "local", "bin", "fleet")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := DetectInstallManager(link); got != InstallManagerHomebrew {
		t.Fatalf("DetectInstallManager(Homebrew symlink) = %q, want %q", got, InstallManagerHomebrew)
	}
}
