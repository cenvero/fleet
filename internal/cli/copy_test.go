// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import "testing"

func TestParseServerPath(t *testing.T) {
	t.Parallel()
	ok := map[string][2]string{
		"web-01:/etc/hosts": {"web-01", "/etc/hosts"},
		"db:/srv/app/data":  {"db", "/srv/app/data"},
		"a:/b:c":            {"a", "/b:c"}, // only the first colon splits
	}
	for in, want := range ok {
		s, p, err := parseServerPath(in)
		if err != nil {
			t.Fatalf("parseServerPath(%q): %v", in, err)
		}
		if s != want[0] || p != want[1] {
			t.Fatalf("parseServerPath(%q) = %q,%q want %q,%q", in, s, p, want[0], want[1])
		}
	}
	for _, bad := range []string{"", "noColon", ":/leading", "server:", "server"} {
		if _, _, err := parseServerPath(bad); err == nil {
			t.Fatalf("parseServerPath(%q) should have errored", bad)
		}
	}
}
