// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newCpCommand is the top-level `fleet cp` convenience wrapper around the
// server-to-server file copy already exposed as `fleet file copy`. It exists so
// the common case reads like the shell `cp` operators expect:
//
//	fleet cp web-01:/etc/hosts db-01:/tmp/hosts
//	fleet cp web-01:/srv/app   db-01:/srv/app -r
//
// root.go registers it via NewRootCommand (root.AddCommand(newCpCommand(...))).
func newCpCommand(configDir *string) *cobra.Command {
	var recursive bool
	var parallel int
	var chunkSize string
	cmd := &cobra.Command{
		Use:   "cp <srcServer:path> <dstServer:path>",
		Short: "Copy a file (or directory with -r) directly between two servers",
		Long: "Shortcut for `fleet file copy`: copy a file or, with -r, a whole directory tree\n" +
			"from one managed server to another. Bytes are relayed through the controller\n" +
			"(download then upload), so it works for every server mode and reuses the\n" +
			"resumable, checksummed transfer engine.\n\n" +
			"Examples:\n" +
			"  fleet cp web-01:/etc/hosts db-01:/tmp/hosts\n" +
			"  fleet cp web-01:/srv/app   db-01:/srv/app -r",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcServer, srcPath, err := parseServerPath(args[0])
			if err != nil {
				return err
			}
			dstServer, dstPath, err := parseServerPath(args[1])
			if err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			opts, err := transferOptsFromFlags(parallel, chunkSize)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if recursive {
				n, err := app.CopyDir(srcServer, srcPath, dstServer, dstPath, opts, nil)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "copied %d files %s -> %s\n", n, args[0], args[1])
				return nil
			}
			progress, finish := newProgressReporter(cmd, "copy")
			res, err := app.CopyFile(srcServer, srcPath, dstServer, dstPath, opts, progress)
			finish()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "copied %s -> %s:%s (%s)\n", args[0], dstServer, res.Path, humanizeBytes(res.Size))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy a directory tree")
	cmd.Flags().IntVar(&parallel, "parallel", 0, "parallel streams per file (0 = server/global default)")
	cmd.Flags().StringVar(&chunkSize, "chunk-size", "", "chunk size, e.g. 4M, 8M (0 = use default)")
	return cmd
}

// unifiedDiff returns a unified diff (the format `patch`/`git apply` consume) of
// two texts, labelled labelA / labelB. It returns "" when the texts are equal.
//
// It is a small, dependency-free implementation: a longest-common-subsequence
// over lines drives hunk grouping with 3 lines of context, which is plenty for
// comparing config files across the fleet.
func unifiedDiff(labelA, labelB, a, b string) string {
	if a == b {
		return ""
	}
	linesA := splitLines(a)
	linesB := splitLines(b)
	if len(linesA) > maxDiffLines || len(linesB) > maxDiffLines {
		return fmt.Sprintf("--- %s\n+++ %s\n@@ files differ; too large for a line-by-line diff (%d vs %d lines) @@\n",
			labelA, labelB, len(linesA), len(linesB))
	}
	ops := diffLines(linesA, linesB)

	const context = 3
	hunks := groupHunks(ops, context)
	if len(hunks) == 0 {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n", labelA)
	fmt.Fprintf(&sb, "+++ %s\n", labelB)
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", hunkRange(h.startA, h.countA), hunkRange(h.startB, h.countB))
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				sb.WriteString(" " + op.text + "\n")
			case opDelete:
				sb.WriteString("-" + op.text + "\n")
			case opInsert:
				sb.WriteString("+" + op.text + "\n")
			}
		}
	}
	return sb.String()
}

// splitLines splits s into lines, dropping a single trailing newline so a file
// ending in "\n" does not yield a spurious trailing empty line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

type diffOpKind int

const (
	opEqual diffOpKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind diffOpKind
	text string
}

// diffLines computes a line-level diff of a and b via a classic LCS dynamic
// program, emitting equal/delete/insert ops in order.
// maxDiffLines bounds the O(n*m) LCS table so two very large files cannot blow
// up controller memory (and satisfies the allocation-size-overflow check by
// bounding the allocation sizes before the make).
const maxDiffLines = 20000

func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n > maxDiffLines || m > maxDiffLines {
		// Too large for an in-memory LCS table — fall back to a linear
		// whole-file replace instead of an O(n*m) allocation.
		ops := make([]diffOp, 0, n+m)
		for _, line := range a {
			ops = append(ops, diffOp{opDelete, line})
		}
		for _, line := range b {
			ops = append(ops, diffOp{opInsert, line})
		}
		return ops
	}
	// lcs[i][j] = length of the LCS of a[i:] and b[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{opEqual, a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{opDelete, a[i]})
			i++
		default:
			ops = append(ops, diffOp{opInsert, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{opDelete, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{opInsert, b[j]})
	}
	return ops
}

type hunk struct {
	startA, countA int
	startB, countB int
	ops            []diffOp
}

// groupHunks slices the op stream into hunks, keeping up to `context` equal
// lines around each run of changes and merging changes that are within
// 2*context of each other (so adjacent edits share one hunk).
func groupHunks(ops []diffOp, context int) []hunk {
	// Index, for each op, the running line numbers in A and B (1-based start).
	type pos struct{ a, b int }
	positions := make([]pos, len(ops))
	a, b := 0, 0
	changed := make([]bool, len(ops))
	for i, op := range ops {
		positions[i] = pos{a + 1, b + 1}
		switch op.kind {
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
		// Walk back up to `context` equal lines for leading context.
		start := i
		for start > 0 && !changed[start-1] && i-start < context {
			start--
		}
		// Extend the hunk forward, absorbing changes separated by <= 2*context
		// equal lines.
		end := i
		for end < len(ops) {
			if changed[end] {
				end++
				continue
			}
			// Count the run of equal lines; if another change follows close by,
			// keep going, otherwise stop after `context` trailing lines.
			run := end
			for run < len(ops) && !changed[run] {
				run++
			}
			gap := run - end
			if run < len(ops) && gap <= 2*context {
				end = run
				continue
			}
			trailing := end + context
			if trailing > run {
				trailing = run
			}
			end = trailing
			break
		}
		if end > len(ops) {
			end = len(ops)
		}

		h := hunk{ops: ops[start:end]}
		h.startA, h.startB = positions[start].a, positions[start].b
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				h.countA++
				h.countB++
			case opDelete:
				h.countA++
			case opInsert:
				h.countB++
			}
		}
		// An empty side starts at line 0 in unified-diff convention.
		if h.countA == 0 {
			h.startA = positions[start].a - 1
		}
		if h.countB == 0 {
			h.startB = positions[start].b - 1
		}
		hunks = append(hunks, h)
		i = end
	}
	return hunks
}

// hunkRange formats the "start,count" field of a unified-diff hunk header,
// collapsing "start,1" to just "start" as diff(1) does.
func hunkRange(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
