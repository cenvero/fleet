// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package version

import "testing"

func TestNormalizeSemVer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: "2.4.2", want: "v2.4.2", ok: true},
		{raw: " v2.4.2 ", want: "v2.4.2", ok: true},
		{raw: "2.4.2-beta.1+build.7", want: "v2.4.2-beta.1+build.7", ok: true},
		{raw: "", ok: false},
		{raw: "   ", ok: false},
		{raw: "unknown", ok: false},
		{raw: "unavailable", ok: false},
		{raw: "version unavailable", ok: false},
		{raw: "n/a", ok: false},
		{raw: "-", ok: false},
		{raw: "dev", ok: false},
		{raw: "V2.4.2", ok: false},
		{raw: "2.4", ok: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, ok := NormalizeSemVer(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("NormalizeSemVer(%q)=(%q,%t), want (%q,%t)", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDisplaySemVerUsesPlaceholderForUnavailable(t *testing.T) {
	t.Parallel()
	if got := DisplaySemVer("2.4.0"); got != "v2.4.0" {
		t.Fatalf("DisplaySemVer(valid)=%q", got)
	}
	if got := DisplaySemVer("version unavailable"); got != "-" {
		t.Fatalf("DisplaySemVer(unavailable)=%q", got)
	}
}
