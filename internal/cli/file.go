// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newFileCommand(configDir *string) *cobra.Command {
	fileCmd := &cobra.Command{
		Use:   "file",
		Short: "Browse and transfer files to and from managed servers",
		Long: "Browse and transfer files over the same authenticated, host-key-pinned SSH\n" +
			"channel the controller already uses — no extra port or daemon. Transfers are\n" +
			"split into chunks sent over several concurrent channels, every chunk and the\n" +
			"whole file are SHA-256 verified, and an interrupted upload or download resumes\n" +
			"from where it stopped when you re-run it. Parallelism and chunk size come from\n" +
			"per-server then global defaults (see 'file defaults'). Related: 'fleet files'\n" +
			"(terminal UI), 'fleet file ui' (web UI), and 'fleet sync' (live directory sync).",
	}

	fileCmd.AddCommand(newFileListCommand(configDir))
	fileCmd.AddCommand(newFileStatCommand(configDir))
	fileCmd.AddCommand(newFileCatCommand(configDir))
	fileCmd.AddCommand(newFileTailCommand(configDir))
	fileCmd.AddCommand(newFileEditCommand(configDir))
	fileCmd.AddCommand(newFileDiffCommand(configDir))
	fileCmd.AddCommand(newFileUploadCommand(configDir))
	fileCmd.AddCommand(newFileDownloadCommand(configDir))
	fileCmd.AddCommand(newFileCopyCommand(configDir))
	fileCmd.AddCommand(newFileServerMoveCommand(configDir))
	fileCmd.AddCommand(newFileCompressCommand(configDir))
	fileCmd.AddCommand(newFileExtractCommand(configDir))
	fileCmd.AddCommand(newFileMkdirCommand(configDir))
	fileCmd.AddCommand(newFileRemoveCommand(configDir))
	fileCmd.AddCommand(newFileMoveCommand(configDir))
	fileCmd.AddCommand(newUICommand(configDir))
	fileCmd.AddCommand(newFileDefaultsCommand(configDir))

	return fileCmd
}

func newFileListCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list <server> [path]",
		Short: "List a directory on a managed server",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			path := ""
			if len(args) == 2 {
				path = args[1]
			}
			result, err := app.ListRemoteDir(args[0], path)
			if err != nil {
				return err
			}
			return writeJSON(cmd, result)
		},
	}
}

func newFileUploadCommand(configDir *string) *cobra.Command {
	var parallel int
	var chunkSize string
	var recursive bool
	cmd := &cobra.Command{
		Use:   "upload <server> <local> [remote]",
		Short: "Upload a local file (or directory with -r) to a server (chunked, parallel, resumable)",
		Long: "Upload <local> to <remote> on <server>. If <remote> is omitted (or ends in '/')\n" +
			"the file lands in the server's default remote directory under its base name.\n" +
			"The transfer is chunked, run over --parallel concurrent channels, SHA-256\n" +
			"verified, and resumable: re-running the same command after an interruption\n" +
			"skips the chunks already on the server. On a terminal it shows a live progress\n" +
			"bar; otherwise it prints periodic JSON.\n\n" +
			"With -r/--recursive, <local> is a directory and <remote> (required) is the\n" +
			"destination directory; the whole tree is uploaded, preserving structure.",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			opts, err := transferOptsFromFlags(parallel, chunkSize)
			if err != nil {
				return err
			}
			remote := ""
			if len(args) == 3 {
				remote = args[2]
			}
			if recursive {
				if remote == "" {
					return fmt.Errorf("recursive upload requires a <remote> directory")
				}
				n, err := app.UploadDir(args[0], args[1], remote, opts, nil)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "uploaded %d files to %s\n", n, remote)
				return nil
			}
			progress, finish := newProgressReporter(cmd, "upload")
			result, err := app.UploadFile(args[0], args[1], remote, opts, progress)
			finish()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "uploaded %s (%s, sha256=%s)\n", result.Path, humanizeBytes(result.Size), shortHash(result.SHA256))
			return nil
		},
	}
	cmd.Flags().IntVar(&parallel, "parallel", 0, "number of parallel streams (0 = use server/global default)")
	cmd.Flags().StringVar(&chunkSize, "chunk-size", "", "chunk size, e.g. 4M, 8M (0 = use default)")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "upload a directory tree")
	return cmd
}

func newFileDownloadCommand(configDir *string) *cobra.Command {
	var parallel int
	var chunkSize string
	var recursive bool
	cmd := &cobra.Command{
		Use:   "download <server> <remote> [local] | <server:remote> [local]",
		Short: "Download a file (or directory with -r) from a server (chunked, parallel, resumable)",
		Long: "Download <remote> from <server> into <local> (defaults to the remote base name\n" +
			"in the current directory; a local directory is allowed and the base name is\n" +
			"appended). Same engine as upload: chunked, parallel, SHA-256 verified, and\n" +
			"resumable from a partial local file.\n\n" +
			"The source may be given as two arguments (<server> <remote>) or combined as\n" +
			"<server:remote>, so both of these are equivalent:\n" +
			"  fleet file download web-01 /root/x.log ./\n" +
			"  fleet file download web-01:/root/x.log ./\n\n" +
			"With -r/--recursive, <remote> is a directory and the whole tree is downloaded\n" +
			"into <local> (default: current directory). Remote names are vetted so a\n" +
			"compromised server cannot write outside <local>.",
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			opts, err := transferOptsFromFlags(parallel, chunkSize)
			if err != nil {
				return err
			}
			server, remote, local, err := parseDownloadArgs(args)
			if err != nil {
				return err
			}
			if recursive {
				dest := local
				if dest == "" {
					dest = "."
				}
				n, err := app.DownloadDir(server, remote, dest, opts, nil)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "downloaded %d files into %s\n", n, dest)
				return nil
			}
			progress, finish := newProgressReporter(cmd, "download")
			result, err := app.DownloadFile(server, remote, local, opts, progress)
			finish()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "downloaded %s (%s)\n", remote, humanizeBytes(result.Entry.Size))
			return nil
		},
	}
	cmd.Flags().IntVar(&parallel, "parallel", 0, "number of parallel streams (0 = use server/global default)")
	cmd.Flags().StringVar(&chunkSize, "chunk-size", "", "chunk size, e.g. 4M, 8M (0 = use default)")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "download a directory tree")
	return cmd
}

func newFileStatCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "stat <server> <path>",
		Short: "Show metadata (size, mode, mtime, type) for a remote path",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			res, err := app.StatRemoteFile(args[0], args[1])
			if err != nil {
				return err
			}
			return writeJSON(cmd, res.Entry)
		},
	}
}

func newFileCatCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "cat <server> <path>",
		Short: "Stream a remote file to stdout (each chunk checksum-verified)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			_, err = app.CatRemoteFile(args[0], args[1], cmd.OutOrStdout())
			return err
		},
	}
}

func newFileTailCommand(configDir *string) *cobra.Command {
	var lines int
	var search string
	cmd := &cobra.Command{
		Use:   "tail <server> <path>",
		Short: "Show the last lines of a remote text file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			res, err := app.TailRemoteFile(args[0], args[1], lines, search)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, line := range res.Lines {
				fmt.Fprintln(out, line.Text)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "number of trailing lines to show")
	cmd.Flags().StringVar(&search, "search", "", "only show lines containing this substring")
	return cmd
}

func newFileEditCommand(configDir *string) *cobra.Command {
	var parallel int
	var chunkSize string
	cmd := &cobra.Command{
		Use:   "edit <server:path>",
		Short: "Edit a remote file in $EDITOR, then upload it back atomically",
		Long: "Download <path> from <server> into a local temp file, open it in your editor\n" +
			"($EDITOR, falling back to vi then nano), and on save upload it back over the\n" +
			"same chunked, checksummed, resumable engine — the remote file is replaced\n" +
			"atomically (temp file -> fsync -> rename). If you quit the editor without\n" +
			"changing anything, the upload is skipped.\n\n" +
			"  fleet file edit web-01:/etc/nginx/nginx.conf",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, remotePath, err := parseServerPath(args[0])
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

			tmpDir, err := os.MkdirTemp("", "fleet-edit-")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(tmpDir) }()
			tmpPath := filepath.Join(tmpDir, path.Base(remotePath))

			if _, err := app.DownloadFile(server, remotePath, tmpPath, opts, nil); err != nil {
				return fmt.Errorf("download for edit: %w", err)
			}
			before, err := fileSHA256(tmpPath)
			if err != nil {
				return err
			}

			if err := openInEditor(cmd, tmpPath); err != nil {
				return err
			}

			after, err := fileSHA256(tmpPath)
			if err != nil {
				return err
			}
			if after == before {
				fmt.Fprintln(cmd.OutOrStdout(), "no changes — upload skipped")
				return nil
			}

			progress, finish := newProgressReporter(cmd, "upload")
			result, err := app.UploadFile(server, tmpPath, remotePath, opts, progress)
			finish()
			if err != nil {
				return fmt.Errorf("upload after edit: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved %s:%s (%s, sha256=%s)\n", server, result.Path, humanizeBytes(result.Size), shortHash(result.SHA256))
			return nil
		},
	}
	cmd.Flags().IntVar(&parallel, "parallel", 0, "number of parallel streams (0 = use server/global default)")
	cmd.Flags().StringVar(&chunkSize, "chunk-size", "", "chunk size, e.g. 4M, 8M (0 = use default)")
	return cmd
}

// openInEditor opens path in the operator's editor: $EDITOR (then $VISUAL),
// falling back to vi then nano. stdin/stdout/stderr are wired to the terminal so
// full-screen editors work.
func openInEditor(cmd *cobra.Command, path string) error {
	editor := firstNonEmpty(os.Getenv("EDITOR"), os.Getenv("VISUAL"))
	if editor == "" {
		for _, candidate := range []string{"vi", "nano"} {
			if _, err := exec.LookPath(candidate); err == nil {
				editor = candidate
				break
			}
		}
	}
	if editor == "" {
		return fmt.Errorf("no editor found: set $EDITOR (or install vi/nano)")
	}
	// The editor string may carry arguments (e.g. "code --wait"); split on spaces.
	parts := strings.Fields(editor)
	c := exec.Command(parts[0], append(parts[1:], path)...) // #nosec G204 -- operator-controlled $EDITOR
	c.Stdin = os.Stdin
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	if err := c.Run(); err != nil {
		return fmt.Errorf("editor exited with error: %w", err)
	}
	return nil
}

// fileSHA256 returns the hex SHA-256 of a local file, used to detect whether an
// edit actually changed the file before re-uploading.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- controller-created temp file
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func newFileDiffCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <serverA:path> <serverB:path>",
		Short: "Show a unified line diff of the same-or-different file on two servers",
		Long: "Fetch a file from each side (each chunk checksum-verified) and print a unified\n" +
			"line diff (the format `patch` understands). Useful for spotting config drift\n" +
			"across the fleet.\n\n" +
			"  fleet file diff web-01:/etc/nginx/nginx.conf web-02:/etc/nginx/nginx.conf\n\n" +
			"Exit status is 0 when the files are identical and 1 when they differ.",
		Args: cobra.ExactArgs(2),
		// A non-zero "files differ" exit must not dump usage text or an
		// "Error: files differ" line — the diff itself is the output. Genuine
		// errors are printed explicitly below before being returned.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runFileDiff(cmd, *configDir, args[0], args[1])
			if err != nil && !errors.Is(err, errFilesDiffer) {
				fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
			}
			return err
		},
	}
	return cmd
}

func runFileDiff(cmd *cobra.Command, configDir, srcA, srcB string) error {
	serverA, pathA, err := parseServerPath(srcA)
	if err != nil {
		return err
	}
	serverB, pathB, err := parseServerPath(srcB)
	if err != nil {
		return err
	}
	app, err := openApp(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	var bufA, bufB bytes.Buffer
	if _, err := app.CatRemoteFile(serverA, pathA, &bufA); err != nil {
		return fmt.Errorf("read %s: %w", srcA, err)
	}
	if _, err := app.CatRemoteFile(serverB, pathB, &bufB); err != nil {
		return fmt.Errorf("read %s: %w", srcB, err)
	}

	out := unifiedDiff(srcA, srcB, bufA.String(), bufB.String())
	if out == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "files are identical\n")
		return nil
	}
	fmt.Fprint(cmd.OutOrStdout(), out)
	// Mirror diff(1): exit non-zero when the files differ.
	return errFilesDiffer
}

// errFilesDiffer signals that `fleet file diff` found differences. main.go
// translates any returned error into a non-zero process exit.
var errFilesDiffer = errors.New("files differ")

// parseServerPath splits a "<server>:<path>" argument.
func parseServerPath(arg string) (server, remotePath string, err error) {
	i := strings.IndexByte(arg, ':')
	if i <= 0 || i == len(arg)-1 {
		return "", "", fmt.Errorf("expected <server>:<path>, got %q", arg)
	}
	return arg[:i], arg[i+1:], nil
}

// parseDownloadArgs accepts the download source either as two positional
// arguments (<server> <remote> [local]) or a combined <server:remote> [local],
// returning the server, remote path, and optional local destination ("" if
// none). It lets the same command serve both ergonomic forms.
func parseDownloadArgs(args []string) (server, remote, local string, err error) {
	if len(args) >= 1 && strings.ContainsRune(args[0], ':') {
		// Combined <server:remote> form; at most one extra arg (the local dest).
		if len(args) > 2 {
			return "", "", "", fmt.Errorf("with <server:remote>, pass at most one [local] argument")
		}
		server, remote, err = parseServerPath(args[0])
		if err != nil {
			return "", "", "", err
		}
		if len(args) == 2 {
			local = args[1]
		}
		return server, remote, local, nil
	}
	// Split <server> <remote> [local] form.
	if len(args) < 2 {
		return "", "", "", fmt.Errorf("expected <server> <remote> [local] or <server:remote> [local]")
	}
	server, remote = args[0], args[1]
	if len(args) == 3 {
		local = args[2]
	}
	return server, remote, local, nil
}

func newFileCopyCommand(configDir *string) *cobra.Command {
	var recursive bool
	var parallel int
	var chunkSize string
	cmd := &cobra.Command{
		Use:   "copy <srcServer:path> <dstServer:path>",
		Short: "Copy a file (or directory with -r) directly between two servers",
		Long: "Copy a file or, with -r, a whole directory tree from one managed server to\n" +
			"another. Bytes are relayed through the controller (download then upload), so it\n" +
			"works for every server mode and reuses the resumable, checksummed engine.\n\n" +
			"Examples:\n" +
			"  fleet file copy web-01:/etc/hosts db-01:/tmp/hosts\n" +
			"  fleet file copy web-01:/srv/app db-01:/srv/app -r",
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

func newFileServerMoveCommand(configDir *string) *cobra.Command {
	var recursive bool
	var parallel int
	var chunkSize string
	cmd := &cobra.Command{
		Use:   "move <srcServer:path> <dstServer:path>",
		Short: "Move a file (or directory with -r) between two servers",
		Long: "Move a file or, with -r, a whole directory tree between managed servers.\n" +
			"Within one server it's an efficient rename; across servers it copies (relayed\n" +
			"through the controller) then deletes the source. ('fleet file mv' renames within\n" +
			"a single server.)\n\n" +
			"Examples:\n" +
			"  fleet file move web-01:/tmp/a db-01:/tmp/a\n" +
			"  fleet file move web-01:/srv/app db-01:/srv/app -r",
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
				n, err := app.MoveDir(srcServer, srcPath, dstServer, dstPath, opts, nil)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "moved %d files %s -> %s\n", n, args[0], args[1])
				return nil
			}
			progress, finish := newProgressReporter(cmd, "move")
			err = app.MoveFile(srcServer, srcPath, dstServer, dstPath, opts, progress)
			finish()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "moved %s -> %s\n", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "move a directory tree")
	cmd.Flags().IntVar(&parallel, "parallel", 0, "parallel streams per file (0 = server/global default)")
	cmd.Flags().StringVar(&chunkSize, "chunk-size", "", "chunk size, e.g. 4M, 8M (0 = use default)")
	return cmd
}

func newFileCompressCommand(configDir *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "compress <server> <archive> <item>...",
		Short: "Compress files/folders into an archive on a server (zip, tar.gz, ...)",
		Long: "Create <archive> on <server> containing the given items (which live in the same\n" +
			"directory as <archive>). Format is taken from the archive extension, or --format.\n\n" +
			"  fleet file compress web-01 /srv/site.tar.gz /srv/public /srv/index.html\n" +
			"  fleet file compress web-01 /tmp/logs.zip /var/log/app.log --format zip",
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			server, archive, items := args[0], args[1], args[2:]
			f := format
			if f == "" {
				f = core.FormatFromName(archive)
			}
			names := make([]string, len(items))
			for i, it := range items {
				names[i] = path.Base(it)
			}
			if err := app.CompressPaths(server, path.Dir(archive), names, path.Base(archive), f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", archive)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "zip | tar.gz | tar.bz2 | tar.xz | tar (default: from extension)")
	return cmd
}

func newFileExtractCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "extract <server> <archivePath>",
		Short: "Extract an archive into its directory on a server",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.ExtractArchive(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "extracted %s\n", args[1])
			return nil
		},
	}
}

func newFileMkdirCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "mkdir <server> <path>",
		Short: "Create a directory on a managed server",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.RemoteMkdir(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", args[1])
			return nil
		},
	}
}

func newFileRemoveCommand(configDir *string) *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:   "rm <server> <path>",
		Short: "Remove a file or directory on a managed server",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.RemoteDelete(args[0], args[1], recursive); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[1])
			return nil
		},
	}
	cmd.Flags().BoolVar(&recursive, "recursive", false, "remove directories and their contents recursively")
	return cmd
}

func newFileMoveCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "mv <server> <from> <to>",
		Short: "Rename or move a path on a managed server",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.RemoteRename(args[0], args[1], args[2]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "moved %s -> %s\n", args[1], args[2])
			return nil
		},
	}
}

func newFileDefaultsCommand(configDir *string) *cobra.Command {
	defaultsCmd := &cobra.Command{
		Use:   "defaults",
		Short: "Show or set file-transfer defaults (global or per-server)",
	}

	defaultsCmd.AddCommand(&cobra.Command{
		Use:   "show [server]",
		Short: "Show global defaults, or the effective defaults for a server",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if len(args) == 1 {
				effective, err := app.FileTransferDefaultsFor(args[0])
				if err != nil {
					return err
				}
				return writeJSON(cmd, effective)
			}
			return writeJSON(cmd, app.Config.Runtime.FileTransfer)
		},
	})

	var parallel int
	var chunkSize string
	var remoteDir string
	setCmd := &cobra.Command{
		Use:   "set [server]",
		Short: "Set global defaults, or per-server overrides when a server is given",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			var chunkBytes int64
			if chunkSize != "" {
				chunkBytes, err = parseSize(chunkSize)
				if err != nil {
					return err
				}
			}

			if len(args) == 1 {
				server, err := app.GetServer(args[0])
				if err != nil {
					return err
				}
				applyDefaultFlags(&server.FileTransfer, cmd, parallel, chunkBytes, remoteDir)
				if err := app.SaveServer(server); err != nil {
					return err
				}
				return writeJSON(cmd, server.FileTransfer)
			}

			applyDefaultFlags(&app.Config.Runtime.FileTransfer, cmd, parallel, chunkBytes, remoteDir)
			if err := core.SaveConfig(core.ConfigPath(*configDir), app.Config); err != nil {
				return err
			}
			return writeJSON(cmd, app.Config.Runtime.FileTransfer)
		},
	}
	setCmd.Flags().IntVar(&parallel, "parallel", 0, "default number of parallel streams")
	setCmd.Flags().StringVar(&chunkSize, "chunk-size", "", "default chunk size, e.g. 4M, 8M")
	setCmd.Flags().StringVar(&remoteDir, "remote-dir", "", "default remote directory for uploads")
	defaultsCmd.AddCommand(setCmd)

	return defaultsCmd
}

func applyDefaultFlags(d *core.FileTransferDefaults, cmd *cobra.Command, parallel int, chunkBytes int64, remoteDir string) {
	if cmd.Flags().Changed("parallel") {
		d.ParallelStreams = parallel
	}
	if cmd.Flags().Changed("chunk-size") {
		d.ChunkSizeBytes = chunkBytes
	}
	if cmd.Flags().Changed("remote-dir") {
		d.RemoteDir = remoteDir
	}
}

func transferOptsFromFlags(parallel int, chunkSize string) (core.FileTransferOptions, error) {
	opts := core.FileTransferOptions{Parallel: parallel}
	if chunkSize != "" {
		bytes, err := parseSize(chunkSize)
		if err != nil {
			return core.FileTransferOptions{}, err
		}
		opts.ChunkSize = bytes
	}
	return opts, nil
}

// parseSize parses a byte size with an optional K/M/G suffix (powers of 1024).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	upper := strings.ToUpper(strings.TrimSuffix(strings.ToUpper(s), "B"))
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(upper, "K"):
		multiplier, upper = 1024, strings.TrimSuffix(upper, "K")
	case strings.HasSuffix(upper, "M"):
		multiplier, upper = 1024*1024, strings.TrimSuffix(upper, "M")
	case strings.HasSuffix(upper, "G"):
		multiplier, upper = 1024*1024*1024, strings.TrimSuffix(upper, "G")
	}
	value, err := strconv.ParseInt(strings.TrimSpace(upper), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("invalid size %q: must not be negative", s)
	}
	if value > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	return value * multiplier, nil
}

// newProgressReporter returns a core.ProgressFunc that renders a live one-line
// bar on a TTY (throttled), or periodic percentage lines otherwise, plus a
// finish func that closes the line.
func newProgressReporter(cmd *cobra.Command, verb string) (core.ProgressFunc, func()) {
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	var (
		mu       sync.Mutex
		last     time.Time
		anything bool
	)
	out := cmd.OutOrStdout()
	report := func(u core.ProgressUpdate) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if !u.Done && now.Sub(last) < 100*time.Millisecond {
			return
		}
		last = now
		anything = true
		pct := 0
		if u.TotalBytes > 0 {
			pct = int(u.BytesDone * 100 / u.TotalBytes)
		}
		if isTTY {
			fmt.Fprintf(out, "\r%s %s  %s  %d streams  %s/%s   ",
				verb, renderBar(pct, 24), humanizeRate(u.RatePerSec), u.ActiveStreams,
				humanizeBytes(u.BytesDone), humanizeBytes(u.TotalBytes))
		} else {
			fmt.Fprintf(out, "%s %d%% (%s/%s) %s\n", verb, pct,
				humanizeBytes(u.BytesDone), humanizeBytes(u.TotalBytes), humanizeRate(u.RatePerSec))
		}
	}
	finish := func() {
		mu.Lock()
		defer mu.Unlock()
		if anything && isTTY {
			fmt.Fprintln(out)
		}
	}
	return report, finish
}

func renderBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * width / 100
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "] " + fmt.Sprintf("%3d%%", pct)
}

func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanizeRate(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "0 B/s"
	}
	return humanizeBytes(int64(bytesPerSec)) + "/s"
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
