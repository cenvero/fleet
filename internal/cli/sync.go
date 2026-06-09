// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

func newSyncCommand(configDir *string) *cobra.Command {
	var interval time.Duration
	var del bool
	var parallel int
	cmd := &cobra.Command{
		Use:   "sync <server> <local-dir> <remote-dir>",
		Short: "Live one-way sync of a local directory to a server (until stopped)",
		Long: "Continuously mirror a local directory to a directory on a server. The full\n" +
			"directory is pushed once, then changes are uploaded as they happen. The sync\n" +
			"runs until you stop it with Ctrl-C.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			server, localDir, remoteDir := args[0], args[1], args[2]
			out := cmd.OutOrStdout()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			fmt.Fprintf(out, "Live sync  %s\n", core.SyncTargetSummary(server, localDir, remoteDir))
			mode := "scan every " + interval.String()
			if del {
				mode += " · delete enabled"
			}
			fmt.Fprintf(out, "%s · press Ctrl-C to stop\n\n", mode)

			var uploaded, deleted int
			err = app.SyncDir(ctx, server, localDir, remoteDir, core.SyncOptions{
				Interval: interval,
				Delete:   del,
				Parallel: parallel,
			}, func(e core.SyncEvent) {
				switch e.Kind {
				case core.SyncReady:
					fmt.Fprintln(out, "✓ initial sync complete — watching for changes…")
				case core.SyncUpload:
					uploaded++
					fmt.Fprintf(out, "↑ %s\n", e.Path)
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
				fmt.Fprintf(out, "\nsync stopped — %d uploaded, %d deleted\n", uploaded, deleted)
				return nil
			}
			return err
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", core.DefaultSyncInterval, "how often to re-scan the local directory for changes")
	cmd.Flags().BoolVar(&del, "delete", false, "delete remote files that were removed locally")
	cmd.Flags().IntVar(&parallel, "parallel", 0, "parallel streams per file (0 = server/global default)")
	return cmd
}
