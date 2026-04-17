// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"math"
	"strconv"
	"testing"
)

func TestClampSSHDimension(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int
		want uint32
	}{
		{name: "negative", in: -1, want: 0},
		{name: "zero", in: 0, want: 0},
		{name: "positive", in: 120, want: 120},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clampSSHDimension(tt.in); got != tt.want {
				t.Fatalf("clampSSHDimension(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}

	if strconv.IntSize == 64 {
		tooLarge := uint64(math.MaxUint32) + 1
		if got := clampSSHDimension(int(tooLarge)); got != math.MaxUint32 {
			t.Fatalf("clampSSHDimension(%d) = %d, want %d", tooLarge, got, uint32(math.MaxUint32))
		}
	}
}
