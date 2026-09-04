// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestRegisteredWinGetLocationOwnsOnlyRegisteredExecutable(t *testing.T) {
	name := fmt.Sprintf("%s_Test_%d_%d", WinGetPackageIdentifier, os.Getpid(), time.Now().UnixNano())
	keyPath := uninstallRegistryPath + `\` + name
	key, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("create test uninstall registration: %v", err)
	}
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, keyPath)
	})

	installDir := filepath.Join(t.TempDir(), "custom-winget-location")
	registeredExecutable := filepath.Join(installDir, "fleet.exe")
	separateExecutable := filepath.Join(t.TempDir(), ".local", "bin", "fleet.exe")
	if err := key.SetStringValue("WinGetPackageIdentifier", WinGetPackageIdentifier); err != nil {
		_ = key.Close()
		t.Fatal(err)
	}
	if err := key.SetStringValue("InstallLocation", installDir); err != nil {
		_ = key.Close()
		t.Fatal(err)
	}
	if err := key.Close(); err != nil {
		t.Fatal(err)
	}

	root := uninstallRegistryRoot{
		key:    registry.CURRENT_USER,
		access: registry.QUERY_VALUE | registry.ENUMERATE_SUB_KEYS,
	}
	if !registeredWinGetLocationOwns(root, name, registeredExecutable) {
		t.Fatalf("custom WinGet location did not own %s", registeredExecutable)
	}
	if registeredWinGetLocationOwns(root, name, separateExecutable) {
		t.Fatalf("WinGet registration incorrectly owned separate executable %s", separateExecutable)
	}
	if !isRegisteredWinGetExecutable(registeredExecutable) {
		t.Fatal("registration enumeration did not detect the custom WinGet executable")
	}
	if isRegisteredWinGetExecutable(separateExecutable) {
		t.Fatal("registration enumeration misclassified a separate self-managed executable")
	}
}
