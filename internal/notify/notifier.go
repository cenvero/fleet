// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

// Package notify provides desktop notification support for the fleet controller.
//
// # Current status
//
// Desktop notifications are temporarily disabled. The previous implementation
// used github.com/gen2brain/beeep which pulled in several abandoned or
// unmaintained transitive dependencies:
//
//   - github.com/nfnt/resize      — archived, no security fixes possible
//   - github.com/tadvi/systray    — last commit 2019, effectively dead
//   - github.com/sergeymakinen/go-bmp — 17 stars, obscure single maintainer
//   - github.com/sergeymakinen/go-ico — 3 stars, essentially unknown
//   - github.com/esiqveland/notify — low activity, Linux D-Bus
//   - git.sr.ht/~jackmordaunt/go-toast — Windows-only, invisible on Sourcehut
//   - github.com/jackmordaunt/icns/v3  — niche macOS icon lib
//
// These packages process binary image data and several are abandoned, making
// them a supply-chain risk with no upstream fix path.
//
// # Future implementation
//
// Desktop notifications will be re-implemented in a future version using
// OS-native APIs directly, with no third-party dependencies:
//
//   - macOS  — osascript / UserNotifications framework via CGo or exec
//   - Linux  — D-Bus org.freedesktop.Notifications via golang.org/x/sys
//   - Windows — Windows Toast API via golang.org/x/sys/windows
//
// The Notifier interface below is intentionally preserved so the rest of
// the codebase requires zero changes when the native implementation lands.
package notify

// Notifier sends a desktop notification to the operator running the controller.
type Notifier interface {
	Notify(title, message string) error
}

type silentNotifier struct{}

func (silentNotifier) Notify(string, string) error { return nil }

// TODO(future): implement nativeNotifier using OS APIs directly
// (no third-party deps) and wire it in NewDesktopNotifier below.
// macOS:   exec osascript -e 'display notification "msg" with title "title"'
// Linux:   D-Bus call to org.freedesktop.Notifications via golang.org/x/sys
// Windows: Windows Toast API via golang.org/x/sys/windows

// NewDesktopNotifier returns a Notifier. Currently always returns a silent
// no-op notifier — desktop notifications will be restored in a future version
// with a native OS implementation that carries no third-party dependencies.
func NewDesktopNotifier(_ bool) Notifier {
	return silentNotifier{}
}
