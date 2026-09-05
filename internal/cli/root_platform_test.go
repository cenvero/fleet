// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

func TestDeferredSelfManagedRemovalCommandWindows(t *testing.T) {
	t.Parallel()

	path := `C:\Users\O'Brien Operator\Fleet App\fleet.exe`
	command, deferred := deferredSelfManagedRemovalCommand("windows", path)
	if !deferred {
		t.Fatal("self-managed Windows removal must be deferred until after process exit")
	}
	want := `Remove-Item -LiteralPath 'C:\Users\O''Brien Operator\Fleet App\fleet.exe' -Force`
	if command != want {
		t.Fatalf("PowerShell removal command = %q, want %q", command, want)
	}
	if strings.Contains(strings.ToLower(command), "sudo") {
		t.Fatalf("Windows removal guidance contains sudo: %s", command)
	}
}

func TestDeferredSelfManagedRemovalCommandUnixNotDeferred(t *testing.T) {
	t.Parallel()

	for _, goos := range []string{"linux", "darwin"} {
		goos := goos
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			if command, deferred := deferredSelfManagedRemovalCommand(goos, "/usr/local/bin/fleet"); deferred || command != "" {
				t.Fatalf("%s removal unexpectedly deferred: command=%q deferred=%v", goos, command, deferred)
			}
		})
	}
}

func TestSelfManagedUninstallTargetIgnoresPATHFleet(t *testing.T) {
	base := t.TempDir()
	running := filepath.Join(base, "running-fleet")
	pathDir := filepath.Join(base, "path-bin")
	if err := os.Mkdir(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pathFleetName := "fleet"
	if runtime.GOOS == "windows" {
		pathFleetName += ".exe"
	}
	pathFleet := filepath.Join(pathDir, pathFleetName)
	for _, path := range []string{running, pathFleet} {
		if err := os.WriteFile(path, []byte("not a real executable"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", pathDir)
	if found, err := exec.LookPath("fleet"); err != nil {
		t.Fatalf("test PATH fleet was not found: %v", err)
	} else if found != pathFleet {
		t.Fatalf("PATH fleet = %q, want %q", found, pathFleet)
	}

	target, err := selfManagedUninstallTarget(running)
	if err != nil {
		t.Fatalf("selfManagedUninstallTarget: %v", err)
	}
	want, err := filepath.EvalSymlinks(running)
	if err != nil {
		t.Fatal(err)
	}
	if target != want {
		t.Fatalf("uninstall target = %q, want resolved running executable %q (PATH contains %q)", target, want, pathFleet)
	}
	if _, err := os.Stat(pathFleet); err != nil {
		t.Fatalf("PATH fleet must remain untouched: %v", err)
	}
}

func TestSelfManagedUninstallTargetResolvesExecutableSymlink(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	realPath := filepath.Join(base, "fleet-real")
	linkPath := filepath.Join(base, "fleet-link")
	if err := os.WriteFile(realPath, []byte("not a real executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	target, err := selfManagedUninstallTarget(linkPath)
	if err != nil {
		t.Fatalf("selfManagedUninstallTarget: %v", err)
	}
	want, err := filepath.EvalSymlinks(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if target != want {
		t.Fatalf("uninstall target = %q, want resolved executable %q", target, want)
	}
}

func TestNonInteractiveInitSavesActiveConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("FLEET_CONFIG_DIR", "")
	custom := filepath.Join(t.TempDir(), "custom-controller")

	cmd := NewRootCommand()
	cmd.SetArgs([]string{
		"init", "--non-interactive", "--init-config-dir", custom,
		"--mode", "direct", "--crypto", "ed25519", "--channel", "stable", "--policy", "notify_only",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("non-interactive init: %v", err)
	}
	if got := core.ResolveConfigDir(""); got != custom {
		t.Fatalf("resolved config after init = %q, want %q", got, custom)
	}
}
