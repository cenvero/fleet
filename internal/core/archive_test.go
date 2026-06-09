// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"
)

func TestCompressCmdNeutralizesOptionInjection(t *testing.T) {
	// A file literally named like a tar flag must reach tar as a path (./-prefixed),
	// never as a bare option.
	cmd, err := compressCmd("/srv/data", []string{"--checkpoint-action=exec=evil", "ok.txt"}, "out.tar.gz", "tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "'./--checkpoint-action=exec=evil'") {
		t.Fatalf("operand not ./-prefixed+quoted: %s", cmd)
	}
	if strings.Contains(cmd, " '--checkpoint") {
		t.Fatalf("flag-like operand leaked unprefixed: %s", cmd)
	}
}

func TestCompressCmdNeutralizesShellInjection(t *testing.T) {
	cmd, err := compressCmd("/d", []string{"a'; rm -rf ~ #"}, "o.zip", "zip")
	if err != nil {
		t.Fatal(err)
	}
	// The single quote in the name must be escaped via the '"'"' idiom, so it
	// cannot terminate the quoting and inject a command.
	if !strings.Contains(cmd, `'"'"'`) {
		t.Fatalf("single-quote in name not escaped: %s", cmd)
	}
}

func TestExtractCmdByExtension(t *testing.T) {
	if got := extractCmd("/srv/a.zip"); !strings.HasPrefix(got, "unzip ") {
		t.Fatalf("zip extract should use unzip: %s", got)
	}
	for _, p := range []string{"/srv/a.tar.gz", "/srv/a.tgz", "/srv/a.tar.xz", "/srv/a.tar"} {
		if got := extractCmd(p); !strings.HasPrefix(got, "tar -xf") {
			t.Fatalf("%s should use tar: %s", p, got)
		}
	}
}

func TestFormatFromName(t *testing.T) {
	cases := map[string]string{
		"x.zip": "zip", "x.tar.gz": "tar.gz", "x.tgz": "tar.gz",
		"x.tar.bz2": "tar.bz2", "x.tar.xz": "tar.xz", "x.tar": "tar", "x.bin": "tar.gz",
	}
	for name, want := range cases {
		if got := FormatFromName(name); got != want {
			t.Errorf("FormatFromName(%q)=%q want %q", name, got, want)
		}
	}
}
