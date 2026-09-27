// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cenvero/fleet/internal/textdiff"
)

// maxDiffFileBytes caps how much of a remote file `file diff` reads into memory
// (it reads BOTH files whole before diffing). A larger file yields a "too large
// to diff" result instead of being slurped — bounding controller memory.
const maxDiffFileBytes = 8 << 20 // 8 MiB

// errDiffFileTooLarge signals a diff operand exceeded maxDiffFileBytes.
var errDiffFileTooLarge = errors.New("file too large to diff")

// cappedWriter buffers writes up to limit bytes, then fails with
// errDiffFileTooLarge so a huge remote file can't be read fully into memory.
type cappedWriter struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		return 0, errDiffFileTooLarge
	}
	return c.buf.Write(p)
}

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
			"from one managed server to another. Within one server the agent copies the file\n" +
			"itself; across servers the bytes stream through the controller chunk by chunk\n" +
			"(no temp copy), so it works for every server mode and reuses the resumable,\n" +
			"checksummed transfer engine. Agents older than this release are copied the\n" +
			"previous way (download, then upload). Progress for a single file goes to stderr\n" +
			"(a live bar on a terminal, JSON lines otherwise).\n\n" +
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
func unifiedDiff(labelA, labelB, a, b string) string {
	return textdiff.Unified(labelA, labelB, a, b)
}

// splitLines splits s into lines, dropping a single trailing newline so a file
// ending in "\n" does not yield a spurious trailing empty line.
func splitLines(s string) []string {
	return textdiff.SplitLines(s)
}
