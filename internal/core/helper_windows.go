// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package core

// processLooksLikeHelper relies on the helper lock on Windows.
func processLooksLikeHelper(int) (bool, string) {
	return true, ""
}
