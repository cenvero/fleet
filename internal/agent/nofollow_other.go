// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !unix

package agent

// oNoFollow is a no-op on platforms without O_NOFOLLOW (e.g. windows, plan9).
const oNoFollow = 0
