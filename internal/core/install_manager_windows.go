// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package core

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const uninstallRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Uninstall`

type uninstallRegistryRoot struct {
	key    registry.Key
	access uint32
}

// isRegisteredWinGetExecutable checks WinGet's Apps & Features registration and
// confirms that its InstallLocation owns executablePath. This avoids treating a
// separate self-managed Fleet binary as WinGet-owned merely because the package
// is also installed for the same account.
func isRegisteredWinGetExecutable(executablePath string) bool {
	roots := []uninstallRegistryRoot{
		{key: registry.CURRENT_USER, access: registry.QUERY_VALUE | registry.ENUMERATE_SUB_KEYS},
		{key: registry.LOCAL_MACHINE, access: registry.QUERY_VALUE | registry.ENUMERATE_SUB_KEYS | registry.WOW64_64KEY},
		{key: registry.LOCAL_MACHINE, access: registry.QUERY_VALUE | registry.ENUMERATE_SUB_KEYS | registry.WOW64_32KEY},
	}
	for _, root := range roots {
		parent, err := registry.OpenKey(root.key, uninstallRegistryPath, root.access)
		if err != nil {
			continue
		}
		names, err := parent.ReadSubKeyNames(-1)
		_ = parent.Close()
		if err != nil {
			continue
		}
		for _, name := range names {
			if registeredWinGetLocationOwns(root, name, executablePath) {
				return true
			}
		}
	}
	return false
}

func registeredWinGetLocationOwns(root uninstallRegistryRoot, name, executablePath string) bool {
	key, err := registry.OpenKey(root.key, uninstallRegistryPath+`\`+name, root.access)
	if err != nil {
		return false
	}
	defer key.Close()

	identifier, _, _ := key.GetStringValue("WinGetPackageIdentifier")
	if !strings.EqualFold(identifier, WinGetPackageIdentifier) &&
		!strings.EqualFold(name, WinGetPackageIdentifier) &&
		!strings.HasPrefix(strings.ToLower(name), strings.ToLower(WinGetPackageIdentifier)+"_") {
		return false
	}

	location, _, _ := key.GetStringValue("InstallLocation")
	if pathOwnsExecutable(location, executablePath) {
		return true
	}

	// Some portable registrations omit InstallLocation but provide DisplayIcon.
	// It can carry a trailing icon index (",0"), which is not part of the path.
	displayIcon, _, _ := key.GetStringValue("DisplayIcon")
	displayIcon = strings.TrimSpace(strings.TrimSuffix(displayIcon, ",0"))
	displayIcon = strings.Trim(displayIcon, `"`)
	return sameWindowsPath(displayIcon, executablePath)
}

func pathOwnsExecutable(base, executablePath string) bool {
	base = strings.TrimSpace(strings.Trim(base, `"`))
	if base == "" || executablePath == "" {
		return false
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	executableAbs, err := filepath.Abs(executablePath)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(baseAbs, executableAbs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, `..\`) && !filepath.IsAbs(rel)
}

func sameWindowsPath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(filepath.Clean(leftAbs), filepath.Clean(rightAbs))
}
