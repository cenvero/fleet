// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"os"
	"path/filepath"
	"strings"
)

// WinGetPackageIdentifier is the stable Windows Package Manager identifier for
// the Cenvero Fleet controller.
const WinGetPackageIdentifier = "Cenvero.Fleet"

// InstallManager identifies who owns the controller executable. Package-manager
// installs must never be modified or removed by Fleet's built-in self-updater.
type InstallManager string

const (
	InstallManagerSelfManaged InstallManager = "self-managed"
	InstallManagerHomebrew    InstallManager = "homebrew"
	InstallManagerWinGet      InstallManager = "winget"
)

// DisplayName returns the name used in operator-facing guidance.
func (m InstallManager) DisplayName() string {
	switch m {
	case InstallManagerHomebrew:
		return "Homebrew"
	case InstallManagerWinGet:
		return "WinGet"
	default:
		return "Fleet"
	}
}

// ManagesController reports whether an external package manager owns the
// controller executable.
func (m InstallManager) ManagesController() bool {
	return m == InstallManagerHomebrew || m == InstallManagerWinGet
}

// UpgradeCommand returns the command that owns controller upgrades for this
// installation method.
func (m InstallManager) UpgradeCommand() string {
	switch m {
	case InstallManagerHomebrew:
		return "brew update && brew upgrade cenvero-fleet"
	case InstallManagerWinGet:
		return "winget upgrade --id " + WinGetPackageIdentifier + " --exact --source winget"
	default:
		return "fleet update apply"
	}
}

// UninstallCommand returns the package-manager command that removes the
// controller executable. Self-managed installs have no external command.
func (m InstallManager) UninstallCommand() string {
	switch m {
	case InstallManagerHomebrew:
		return "brew uninstall cenvero-fleet"
	case InstallManagerWinGet:
		return "winget uninstall --id " + WinGetPackageIdentifier + " --exact --source winget"
	default:
		return ""
	}
}

// DetectInstallManager identifies ownership from the executable location and,
// on Windows, WinGet's package registration. The registration check matters for
// portable packages installed with a custom --location outside WinGet's default
// package roots.
func DetectInstallManager(executablePath string) InstallManager {
	if manager := detectInstallManagerPath(executablePath); manager.ManagesController() {
		return manager
	}

	// Homebrew normally exposes Cellar binaries through /usr/local/bin or
	// /opt/homebrew/bin symlinks. os.Executable reports the invoked link on some
	// platforms, so classify the resolved target before deciding it is self-managed.
	resolvedPath, err := filepath.EvalSymlinks(executablePath)
	if err == nil && resolvedPath != executablePath {
		if manager := detectInstallManagerPath(resolvedPath); manager.ManagesController() {
			return manager
		}
	}

	// WinGet's Apps & Features registration covers portable packages installed
	// at a custom --location outside the standard package roots.
	if isRegisteredWinGetExecutable(executablePath) ||
		(err == nil && resolvedPath != executablePath && isRegisteredWinGetExecutable(resolvedPath)) {
		return InstallManagerWinGet
	}
	return InstallManagerSelfManaged
}

func detectInstallManagerPath(executablePath string) InstallManager {
	p := strings.ToLower(filepath.ToSlash(filepath.Clean(strings.TrimSpace(executablePath))))
	p = strings.ReplaceAll(p, `\`, "/")
	if strings.Contains(p, "/homebrew/") ||
		strings.Contains(p, "/cellar/") ||
		strings.Contains(p, "/linuxbrew/") {
		return InstallManagerHomebrew
	}
	if strings.Contains(p, "/microsoft/winget/packages/") ||
		strings.Contains(p, "/microsoft/winget/links/") ||
		strings.Contains(p, "/winget/packages/") ||
		strings.Contains(p, "/winget/links/") {
		return InstallManagerWinGet
	}
	return InstallManagerSelfManaged
}

// RuntimeInstallManager checks the currently running controller executable.
func RuntimeInstallManager() InstallManager {
	executablePath, err := os.Executable()
	if err != nil {
		return InstallManagerSelfManaged
	}
	return DetectInstallManager(executablePath)
}

// Compatibility helpers retain the existing API while callers migrate to the
// generalized install-manager abstraction.
func IsHomebrewInstall(executablePath string) bool {
	return DetectInstallManager(executablePath) == InstallManagerHomebrew
}

func RuntimeIsHomebrewInstall() bool {
	return RuntimeInstallManager() == InstallManagerHomebrew
}

func IsWinGetInstall(executablePath string) bool {
	return DetectInstallManager(executablePath) == InstallManagerWinGet
}

func RuntimeIsWinGetInstall() bool {
	return RuntimeInstallManager() == InstallManagerWinGet
}

// UpgradeCommand returns the controller upgrade command for the runtime's
// installation method.
func UpgradeCommand() string {
	return RuntimeInstallManager().UpgradeCommand()
}
