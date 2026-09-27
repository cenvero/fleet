// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package textdiff

import (
	"fmt"
	"strings"
	"testing"
)

func TestUnifiedSmallEditInLargeFile(t *testing.T) {
	t.Parallel()
	var a strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&a, "line %d\n", i)
	}
	b := strings.Replace(a.String(), "line 12345\n", "changed\n", 1)
	out := Unified("a", "b", a.String(), b)
	want := "--- a\n+++ b\n@@ -12343,7 +12343,7 @@\n line 12342\n line 12343\n line 12344\n-line 12345\n+changed\n line 12346\n line 12347\n line 12348\n"
	if out != want {
		t.Fatalf("diff =\n%s\nwant\n%s", out, want)
	}
}

func TestUnifiedFinalNewlineOnly(t *testing.T) {
	t.Parallel()
	out := Unified("a", "b", "x\ny", "x\ny\n")
	if !strings.Contains(out, "newline at the end") {
		t.Fatalf("final-newline change not reported:\n%s", out)
	}
}

func TestUnifiedLimitTruncates(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("old\n", 500)
	b := strings.Repeat("new\n", 500)
	out, truncated := UnifiedLimit("a", "b", a, b, 200)
	if !truncated || len(out) > 260 || !strings.Contains(out, "diff truncated") {
		t.Fatalf("truncated=%v len=%d:\n%s", truncated, len(out), out)
	}
}

func TestUnifiedHugeChangeFallsBackToBlocks(t *testing.T) {
	t.Parallel()
	var a, b strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&a, "a%d\n", i)
		fmt.Fprintf(&b, "b%d\n", i)
	}
	out := Unified("a", "b", a.String(), b.String())
	if !strings.HasPrefix(out, "--- a\n+++ b\n@@ -1,3000 +1,3000 @@\n-a0\n") {
		t.Fatalf("unexpected header:\n%.200s", out)
	}
}
