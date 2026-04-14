// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package notify

import "github.com/gen2brain/beeep"

type Notifier interface {
	Notify(title, message string) error
}

type silentNotifier struct{}

func (silentNotifier) Notify(string, string) error { return nil }

type desktopNotifier struct{}

func (desktopNotifier) Notify(title, message string) error {
	return beeep.Notify(title, message, "")
}

func NewDesktopNotifier(enabled bool) Notifier {
	if !enabled {
		return silentNotifier{}
	}
	return desktopNotifier{}
}
