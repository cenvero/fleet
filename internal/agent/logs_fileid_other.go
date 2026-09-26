// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !unix

package agent

import "os"

// logFileID has no portable file identity here; log cursors then rely on the
// size and the fingerprint of the bytes before the cursor to detect rotation.
func logFileID(os.FileInfo) string {
	return ""
}
