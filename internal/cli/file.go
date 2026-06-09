// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"math"
	"os"
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
			"(terminal UI), 'fleet ui' (web UI), and 'fleet sync' (live directory sync).",
	}

	fileCmd.AddCommand(newFileListCommand(configDir))
	fileCmd.AddCommand(newFileUploadCommand(configDir))
	fileCmd.AddCommand(newFileDownloadCommand(configDir))
	fileCmd.AddCommand(newFileMkdirCommand(configDir))
	fileCmd.AddCommand(newFileRemoveCommand(configDir))
	fileCmd.AddCommand(newFileMoveCommand(configDir))
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
	cmd := &cobra.Command{
		Use:   "upload <server> <local> [remote]",
		Short: "Upload a local file to a server (chunked, parallel, resumable)",
		Long: "Upload <local> to <remote> on <server>. If <remote> is omitted (or ends in '/')\n" +
			"the file lands in the server's default remote directory under its base name.\n" +
			"The transfer is chunked, run over --parallel concurrent channels, SHA-256\n" +
			"verified, and resumable: re-running the same command after an interruption\n" +
			"skips the chunks already on the server. On a terminal it shows a live progress\n" +
			"bar; otherwise it prints periodic JSON.",
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
	return cmd
}

func newFileDownloadCommand(configDir *string) *cobra.Command {
	var parallel int
	var chunkSize string
	cmd := &cobra.Command{
		Use:   "download <server> <remote> [local]",
		Short: "Download a file from a server (chunked, parallel, resumable)",
		Long: "Download <remote> from <server> into <local> (defaults to the remote base name\n" +
			"in the current directory; a local directory is allowed and the base name is\n" +
			"appended). Same engine as upload: chunked, parallel, SHA-256 verified, and\n" +
			"resumable from a partial local file.",
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
			local := ""
			if len(args) == 3 {
				local = args[2]
			}
			progress, finish := newProgressReporter(cmd, "download")
			result, err := app.DownloadFile(args[0], args[1], local, opts, progress)
			finish()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "downloaded %s (%s)\n", args[1], humanizeBytes(result.Entry.Size))
			return nil
		},
	}
	cmd.Flags().IntVar(&parallel, "parallel", 0, "number of parallel streams (0 = use server/global default)")
	cmd.Flags().StringVar(&chunkSize, "chunk-size", "", "chunk size, e.g. 4M, 8M (0 = use default)")
	return cmd
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
