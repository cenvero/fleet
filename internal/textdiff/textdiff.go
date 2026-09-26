// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

// Package textdiff renders unified line diffs (the format `patch` and
// `git apply` consume). It is small and dependency-free, and it is shared by
// the controller (`fleet file diff`, `fleet cp --diff`) and the agent (the
// diff `file.edit` reports for a change).
package textdiff

import (
	"fmt"
	"strings"
)

// maxLCSCells bounds the longest-common-subsequence table built for the part
// of two texts that differs (after their common first and last lines are set
// aside): 2500×2500 int32 cells, about 25 MB. Anything larger is shown as one
// block of removed lines followed by one block of added lines — still a valid
// diff, just not a minimal one.
const maxLCSCells = 2500 * 2500

// contextLines is how many unchanged lines surround each hunk.
const contextLines = 3

// Unified returns a unified diff of a and b labelled labelA/labelB, or "" when
// the texts are equal.
func Unified(labelA, labelB, a, b string) string {
	out, _ := UnifiedLimit(labelA, labelB, a, b, 0)
	return out
}

// UnifiedLimit is Unified with the output capped near maxBytes (0 = no cap).
// A capped diff ends at a line boundary with a note saying how many lines were
// left out, and truncated reports that it was cut.
func UnifiedLimit(labelA, labelB, a, b string, maxBytes int) (diff string, truncated bool) {
	if a == b {
		return "", false
	}
	linesA, linesB := SplitLines(a), SplitLines(b)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", labelA, labelB)
	hunks := groupHunks(diffLines(linesA, linesB), contextLines)
	if len(hunks) == 0 {
		// Same lines, different bytes: only the final newline differs.
		sb.WriteString("@@ only the newline at the end of the file changed @@\n")
		return sb.String(), false
	}
	var body []string
	for _, h := range hunks {
		body = append(body, fmt.Sprintf("@@ -%s +%s @@", hunkRange(h.startA, h.countA), hunkRange(h.startB, h.countB)))
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				body = append(body, " "+op.text)
			case opDelete:
				body = append(body, "-"+op.text)
			case opInsert:
				body = append(body, "+"+op.text)
			}
		}
	}
	for i, line := range body {
		if maxBytes > 0 && sb.Len()+len(line)+1 > maxBytes {
			fmt.Fprintf(&sb, "... diff truncated: %d more lines\n", len(body)-i)
			return sb.String(), true
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String(), false
}

// SplitLines splits s into lines, dropping a single trailing newline so a file
// ending in "\n" does not yield a spurious trailing empty line.
func SplitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type op struct {
	kind opKind
	text string
}

// diffLines computes a line-level diff of a and b. The lines both texts start
// and end with are matched directly, so an edit in a large file only runs the
// quadratic LCS over the region that actually changed.
func diffLines(a, b []string) []op {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	ops := make([]op, 0, len(a)+len(b)-prefix-suffix)
	for _, line := range a[:prefix] {
		ops = append(ops, op{opEqual, line})
	}
	ops = append(ops, lcsDiff(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])...)
	for _, line := range a[len(a)-suffix:] {
		ops = append(ops, op{opEqual, line})
	}
	return ops
}

// lcsDiff diffs two line slices with a classic LCS dynamic program, or — past
// maxLCSCells — as a whole-block replacement.
func lcsDiff(a, b []string) []op {
	n, m := len(a), len(b)
	ops := make([]op, 0, n+m)
	if n == 0 || m == 0 || n > maxLCSCells/max(m, 1) {
		for _, line := range a {
			ops = append(ops, op{opDelete, line})
		}
		for _, line := range b {
			ops = append(ops, op{opInsert, line})
		}
		return ops
	}
	// lcs[i*(m+1)+j] = length of the LCS of a[i:] and b[j:]. n*m is bounded by
	// maxLCSCells above, so the table stays small.
	width := m + 1
	lcs := make([]int32, (n+1)*width)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i*width+j] = lcs[(i+1)*width+j+1] + 1
			case lcs[(i+1)*width+j] >= lcs[i*width+j+1]:
				lcs[i*width+j] = lcs[(i+1)*width+j]
			default:
				lcs[i*width+j] = lcs[i*width+j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{opEqual, a[i]})
			i++
			j++
		case lcs[(i+1)*width+j] >= lcs[i*width+j+1]:
			ops = append(ops, op{opDelete, a[i]})
			i++
		default:
			ops = append(ops, op{opInsert, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{opDelete, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{opInsert, b[j]})
	}
	return ops
}

type hunk struct {
	startA, countA int
	startB, countB int
	ops            []op
}

// groupHunks slices the op stream into hunks, keeping up to `context` equal
// lines around each run of changes and merging changes that are within
// 2*context of each other (so adjacent edits share one hunk).
func groupHunks(ops []op, context int) []hunk {
	type pos struct{ a, b int }
	positions := make([]pos, len(ops))
	changed := make([]bool, len(ops))
	a, b := 0, 0
	for i, o := range ops {
		positions[i] = pos{a + 1, b + 1}
		switch o.kind {
		case opEqual:
			a++
			b++
		case opDelete:
			a++
			changed[i] = true
		case opInsert:
			b++
			changed[i] = true
		}
	}

	var hunks []hunk
	i := 0
	for i < len(ops) {
		if !changed[i] {
			i++
			continue
		}
		start := i
		for start > 0 && !changed[start-1] && i-start < context {
			start--
		}
		end := i
		for end < len(ops) {
			if changed[end] {
				end++
				continue
			}
			run := end
			for run < len(ops) && !changed[run] {
				run++
			}
			if run < len(ops) && run-end <= 2*context {
				end = run
				continue
			}
			end = min(end+context, run)
			break
		}
		end = min(end, len(ops))

		h := hunk{ops: ops[start:end], startA: positions[start].a, startB: positions[start].b}
		for _, o := range h.ops {
			switch o.kind {
			case opEqual:
				h.countA++
				h.countB++
			case opDelete:
				h.countA++
			case opInsert:
				h.countB++
			}
		}
		// An empty side starts at the line before, per unified-diff convention.
		if h.countA == 0 {
			h.startA--
		}
		if h.countB == 0 {
			h.startB--
		}
		hunks = append(hunks, h)
		i = end
	}
	return hunks
}

func hunkRange(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
