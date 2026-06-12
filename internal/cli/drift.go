// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// newDriftCommand builds `fleet drift` plus its `capture` subcommand.
//
//	fleet drift capture <server> --paths /etc/ssh/sshd_config,/etc/fstab
//	fleet drift <server>
//
// `capture` records the current content (and sha256) of each path as a baseline
// stored under <configDir>/baselines/<server>.json. `fleet drift <server>`
// re-reads those paths and reports unchanged / CHANGED (with a unified line
// diff) / missing, exiting non-zero if any drift is detected.
func newDriftCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drift <server>",
		Short: "Detect config drift of captured paths against a stored baseline",
		Long: "Compare a server's tracked config files against a previously captured baseline.\n\n" +
			"First capture a baseline of the files you care about:\n\n" +
			"  fleet drift capture web-01 --paths /etc/ssh/sshd_config,/etc/fstab\n\n" +
			"Later, check for drift (exits non-zero if anything changed or went missing):\n\n" +
			"  fleet drift web-01\n\n" +
			"Each path is reported as unchanged, CHANGED (with a unified line diff), or missing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDrift(cmd, *configDir, args[0])
		},
	}
	cmd.AddCommand(newBaselineCaptureCommand(configDir))
	return cmd
}

// newBaselineCaptureCommand builds `fleet drift capture <server> --paths ...`.
func newBaselineCaptureCommand(configDir *string) *cobra.Command {
	var paths []string
	cmd := &cobra.Command{
		Use:   "capture <server> --paths /etc/ssh/sshd_config,/etc/fstab",
		Short: "Capture the current content of paths as a drift baseline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cleaned := cleanPaths(paths)
			if len(cleaned) == 0 {
				return fmt.Errorf("--paths is required (comma-separated absolute paths)")
			}
			// Each path is read with `cat -- <path>` on the server; require an
			// ABSOLUTE path so a flag-shaped entry ("-n", "--help") or a relative
			// path can never be captured into a baseline (defense in depth alongside
			// the `--` end-of-options guard in readRemoteFile).
			for _, p := range cleaned {
				if !strings.HasPrefix(p, "/") {
					return fmt.Errorf("invalid --paths entry %q: paths must be absolute (start with '/')", p)
				}
			}
			return runBaselineCapture(cmd, *configDir, args[0], cleaned)
		},
	}
	cmd.Flags().StringSliceVar(&paths, "paths", nil, "comma-separated paths to capture (e.g. /etc/ssh/sshd_config,/etc/fstab)")
	return cmd
}

// cleanPaths trims and drops empty entries from a --paths value.
func cleanPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runBaselineCapture fetches each path's content and stores a baseline.
func runBaselineCapture(cmd *cobra.Command, configDir, server string, paths []string) error {
	app, err := openApp(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	out := cmd.OutOrStdout()
	baseline := core.Baseline{
		Server:     server,
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Paths:      make(map[string]core.BaselineEntry, len(paths)),
	}
	for _, path := range paths {
		content, err := readRemoteFile(app, server, path)
		if err != nil {
			return fmt.Errorf("capture %s: %w", path, err)
		}
		baseline.Paths[path] = core.BaselineEntry{
			SHA256:  core.HashContent(content),
			Content: content,
		}
		fmt.Fprintf(out, "captured %s (%d bytes)\n", path, len(content))
	}

	store := core.NewBaselineStore(configDir)
	if err := store.Save(baseline); err != nil {
		return err
	}
	fmt.Fprintf(out, "baseline saved for %s (%d path(s))\n", server, len(paths))
	return nil
}

// runDrift re-reads the captured paths and reports per-path drift.
func runDrift(cmd *cobra.Command, configDir, server string) error {
	store := core.NewBaselineStore(configDir)
	baseline, ok, err := store.Get(server)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no baseline captured for %q — run 'fleet drift capture %s --paths ...' first", server, server)
	}
	if len(baseline.Paths) == 0 {
		return fmt.Errorf("baseline for %q has no captured paths", server)
	}

	app, err := openApp(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	out := cmd.OutOrStdout()
	drifted := false
	for _, path := range baseline.SortedPaths() {
		entry := baseline.Paths[path]
		current, readErr := readRemoteFile(app, server, path)
		if readErr != nil {
			// A read failure (e.g. file removed) is treated as drift: missing.
			drifted = true
			fmt.Fprintf(out, "missing  %s (%v)\n", path, readErr)
			continue
		}
		if core.HashContent(current) == entry.SHA256 {
			fmt.Fprintf(out, "unchanged  %s\n", path)
			continue
		}
		drifted = true
		fmt.Fprintf(out, "CHANGED  %s\n", path)
		diff := unifiedLineDiff(entry.Content, current)
		for _, line := range diff {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}

	if drifted {
		// Best-effort notification: a fire failure must not change the drift
		// result the operator sees, so its error is intentionally ignored.
		_ = core.NewNotifyStore(configDir).Fire(core.NotifyEventDrift, fmt.Sprintf("config drift detected on %q", server))
		return fmt.Errorf("drift detected on %q", server)
	}
	fmt.Fprintf(out, "\nno drift on %q\n", server)
	return nil
}

// readRemoteFile fetches the content of a path on a server via `cat`. The path
// is single-quote shell-escaped so spaces or metacharacters are safe, AND it is
// passed after a `--` end-of-options marker so a flag-shaped path (e.g. "--help",
// "-n") cannot be parsed by `cat` as an OPTION — shell-quoting prevents word
// splitting, not option parsing, so the `--` guard is what actually contains it.
func readRemoteFile(app *core.App, server, path string) (string, error) {
	res, err := app.ExecCommand(server, "cat -- "+shellQuote(path))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", res.ExitCode)
		}
		return "", fmt.Errorf("%s", msg)
	}
	return res.Stdout, nil
}

// unifiedLineDiff returns a simple unified-style line diff between old and new.
// Common leading/trailing lines are elided; the differing region is shown with
// '-' (baseline) and '+' (current) prefixes. It is intentionally small (an LCS
// is overkill for config-file drift); equal files produce no lines.
func unifiedLineDiff(oldText, newText string) []string {
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)

	// Trim the common prefix.
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}
	// Trim the common suffix (not past the prefix).
	endOld, endNew := len(oldLines), len(newLines)
	for endOld > start && endNew > start && oldLines[endOld-1] == newLines[endNew-1] {
		endOld--
		endNew--
	}

	var diff []string
	for i := start; i < endOld; i++ {
		diff = append(diff, "- "+oldLines[i])
	}
	for i := start; i < endNew; i++ {
		diff = append(diff, "+ "+newLines[i])
	}
	return diff
}
