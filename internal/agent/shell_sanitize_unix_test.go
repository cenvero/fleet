// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"math"
	"strconv"
	"testing"
)

func TestClampPTYDimension(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   uint32
		want uint16
	}{
		{name: "zero", in: 0, want: 0},
		{name: "small", in: 80, want: 80},
		{name: "max", in: math.MaxUint16, want: math.MaxUint16},
		{name: "clamped", in: math.MaxUint16 + 1, want: math.MaxUint16},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clampPTYDimension(tt.in); got != tt.want {
				t.Fatalf("clampPTYDimension(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestSSHExitStatusValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int
		want uint32
	}{
		{name: "negative", in: -1, want: 255},
		{name: "zero", in: 0, want: 0},
		{name: "positive", in: 42, want: 42},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sshExitStatusValue(tt.in); got != tt.want {
				t.Fatalf("sshExitStatusValue(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}

	if strconv.IntSize == 64 {
		tooLarge := uint64(math.MaxUint32) + 1
		if got := sshExitStatusValue(int(tooLarge)); got != math.MaxUint32 {
			t.Fatalf("sshExitStatusValue(%d) = %d, want %d", tooLarge, got, uint32(math.MaxUint32))
		}
	}
}
