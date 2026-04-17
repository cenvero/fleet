// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import "math"

func clampSSHDimension(size int) uint32 {
	if size <= 0 {
		return 0
	}
	if uint64(size) > math.MaxUint32 {
		return uint32(math.MaxUint32)
	}
	return uint32(size)
}
