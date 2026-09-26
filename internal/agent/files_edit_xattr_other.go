// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix && !linux

package agent

import "os"

// copyXattrs is a no-op outside Linux: extended attributes there do not carry
// permissions (macOS ACLs are not exposed as extended attributes).
func copyXattrs(_, _ *os.File) ([]string, *RPCError) { return nil, nil }
