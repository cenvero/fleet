// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

func newSyncCommand(configDir *string) *cobra.Command {
	var interval time.Duration
	var noDelete bool
	var parallel int
	var from string
	cmd := &cobra.Command{
		Use:               "sync <server> <local-dir> <remote-dir>",
		ValidArgsFunction: serverNameComp(configDir),
		Short:             "Live mirror a directory between local and a server (writer → replica)",
		Long: "Keep a local directory and a server directory mirrored, live, until you stop\n" +
			"it with Ctrl-C.\n\n" +
			"One side is the writer (the source of truth) and the other is a read-only\n" +
			"replica. Choose the writer with --from:\n" +
			"  --from local   (default) the local directory is the writer; it is pushed to\n" +
			"                 the server, which becomes the replica.\n" +
			"  --from remote  the server directory is the writer; it is pulled down and the\n" +
			"                 local directory becomes the replica.\n\n" +
			"The writer is copied to the replica once, then re-scanned on an interval:\n" +
			"files that are new or differ overwrite the replica, and — by default — replica\n" +
			"files that do not exist on the writer are deleted, so the replica becomes an\n" +
			"exact mirror. Pass --no-delete to keep the replica's extra files (still\n" +
			"overwriting the ones that differ).\n\n" +
			"Examples:\n" +
			"  fleet sync web-01 ./site /var/www/site\n" +
			"  fleet sync web-01 ./site /var/www/site --no-delete\n" +
			"  fleet sync web-01 ./backup /srv/data --from remote",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			direction, err := parseSyncDirection(from)
			if err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			server, localDir, remoteDir := args[0], args[1], args[2]
			out := cmd.OutOrStdout()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			fmt.Fprintf(out, "Live sync  %s\n", core.SyncSummary(server, localDir, remoteDir, direction))
			mode := "mirror (replica extras are deleted)"
			if noDelete {
				mode = "no-delete (replica extras are kept)"
			}
			fmt.Fprintf(out, "%s · scan every %s · press Ctrl-C to stop\n\n", mode, interval)

			arrow := "↑"
			if direction == core.SyncFromRemote {
				arrow = "↓"
			}
			var copied, deleted int
			err = app.SyncDir(ctx, server, localDir, remoteDir, core.SyncOptions{
				Interval: interval,
				NoDelete: noDelete,
				Parallel: parallel,
				From:     direction,
			}, func(e core.SyncEvent) {
				switch e.Kind {
				case core.SyncReady:
					fmt.Fprintln(out, "✓ initial mirror complete — watching for changes…")
				case core.SyncCopy:
					copied++
					fmt.Fprintf(out, "%s %s\n", arrow, e.Path)
				case core.SyncDelete:
					deleted++
					fmt.Fprintf(out, "✗ %s\n", e.Path)
				case core.SyncError:
					if e.Path != "" {
						fmt.Fprintf(out, "! %s: %v\n", e.Path, e.Err)
					} else {
						fmt.Fprintf(out, "! %v\n", e.Err)
					}
				}
			})
			if errors.Is(err, context.Canceled) {
				fmt.Fprintf(out, "\nsync stopped — %d copied, %d deleted\n", copied, deleted)
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&from, "from", "local", "which side is the writer: local (push) or remote (pull)")
	cmd.Flags().DurationVar(&interval, "interval", core.DefaultSyncInterval, "how often to re-scan the writer for changes")
	cmd.Flags().BoolVar(&noDelete, "no-delete", false, "keep replica files that don't exist on the writer (default mirrors exactly)")
	cmd.Flags().IntVar(&parallel, "parallel", 0, "parallel streams per file (0 = server/global default)")
	return cmd
}

func parseSyncDirection(v string) (core.SyncDirection, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "local", "push", "up":
		return core.SyncFromLocal, nil
	case "remote", "pull", "down", "server":
		return core.SyncFromRemote, nil
	default:
		return "", fmt.Errorf("invalid --from %q: use 'local' or 'remote'", v)
	}
}
