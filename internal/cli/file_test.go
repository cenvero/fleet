// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"":     0,
		"1024": 1024,
		"4M":   4 * 1024 * 1024,
		"8m":   8 * 1024 * 1024,
		"2G":   2 * 1024 * 1024 * 1024,
		"512K": 512 * 1024,
		"10MB": 10 * 1024 * 1024,
	}
	for in, want := range ok {
		got, err := parseSize(in)
		if err != nil {
			t.Fatalf("parseSize(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}

	// Overflow and negative inputs must error, not wrap.
	for _, bad := range []string{"9223372036854775807M", "-5M", "99999999999999999999G", "abc"} {
		if _, err := parseSize(bad); err == nil {
			t.Fatalf("parseSize(%q) expected error", bad)
		}
	}
}

func TestSplitRemoteCompressPathsUsesServerStyle(t *testing.T) {
	tests := []struct {
		name        string
		style       core.TargetPathStyle
		archive     string
		items       []string
		wantDir     string
		wantArchive string
	}{
		{"posix", core.TargetPathPOSIX, "/srv/site.tar.gz", []string{"public", "index.html"}, "/srv", "site.tar.gz"},
		{"windows drive", core.TargetPathWindows, `D:\sites\site.zip`, []string{"public", "index.html"}, `D:\sites`, "site.zip"},
		{"windows UNC", core.TargetPathWindows, `\\host\share\logs.zip`, []string{"app.log"}, `\\host\share\`, "logs.zip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, archive, names, err := splitRemoteCompressPaths(tt.style, tt.archive, tt.items)
			if err != nil {
				t.Fatal(err)
			}
			if dir != tt.wantDir || archive != tt.wantArchive {
				t.Fatalf("split = dir %q archive %q, want %q %q", dir, archive, tt.wantDir, tt.wantArchive)
			}
			if len(names) != len(tt.items) {
				t.Fatalf("names = %#v", names)
			}
		})
	}
	// Full paths of entries in the archive's own directory are accepted.
	if _, _, names, err := splitRemoteCompressPaths(core.TargetPathPOSIX, "/srv/site.tar.gz", []string{"/srv/public", "index.html"}); err != nil || names[0] != "public" || names[1] != "index.html" {
		t.Fatalf("full path in the archive directory: names=%v err=%v", names, err)
	}
	if _, _, names, err := splitRemoteCompressPaths(core.TargetPathWindows, `D:\sites\site.zip`, []string{`D:\sites\public`}); err != nil || names[0] != "public" {
		t.Fatalf("windows full path in the archive directory: names=%v err=%v", names, err)
	}
	for _, item := range []string{"/etc/passwd", "/srv/x/../public", "/srv/./public", "/srv/sub/file", "/srv/.."} {
		if _, _, _, err := splitRemoteCompressPaths(core.TargetPathPOSIX, "/srv/site.tar.gz", []string{item}); err == nil {
			t.Errorf("item %q outside the archive directory accepted", item)
		}
	}
	for _, item := range []string{`sub/file`, `sub\file`, ".."} {
		if _, _, _, err := splitRemoteCompressPaths(core.TargetPathWindows, `C:\out.zip`, []string{item}); err == nil {
			t.Errorf("unsafe Windows item %q accepted", item)
		}
	}
}
