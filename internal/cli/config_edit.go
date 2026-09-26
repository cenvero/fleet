// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// runConfigEdit is `fleet config edit`. The editor works on a private draft
// next to config.toml; the draft replaces config.toml (atomically, by rename)
// only once it parses and validates. A config that no longer loads would make
// every later fleet command fail, so a broken edit is never saved: on a
// terminal the editor is offered again, otherwise the draft is kept for the
// operator and the command fails.
func runConfigEdit(cmd *cobra.Command, configDir string) error {
	path := core.ConfigPath(configDir)
	original, err := os.ReadFile(path) // #nosec G304 -- the controller's own config file
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	draft, err := os.CreateTemp(filepath.Dir(path), ".config.edit-*.toml")
	if err != nil {
		return fmt.Errorf("create config draft: %w", err)
	}
	draftPath := draft.Name()
	keepDraft := false
	defer func() {
		if !keepDraft {
			_ = os.Remove(draftPath)
		}
	}()
	if err := draft.Chmod(0o600); err != nil {
		_ = draft.Close()
		return fmt.Errorf("set draft permissions: %w", err)
	}
	if _, err := draft.Write(original); err != nil {
		_ = draft.Close()
		return fmt.Errorf("write config draft: %w", err)
	}
	if err := draft.Close(); err != nil {
		return fmt.Errorf("write config draft: %w", err)
	}

	interactive := term.IsTerminal(int(os.Stdin.Fd())) // #nosec G115 -- a file descriptor fits in int
	in := bufio.NewReader(cmd.InOrStdin())
	for {
		if err := openInEditor(cmd, configDir, draftPath); err != nil {
			return err
		}
		edited, err := os.ReadFile(draftPath) // #nosec G304 -- draft created above
		if err != nil {
			return fmt.Errorf("read config draft: %w", err)
		}
		if bytes.Equal(edited, original) {
			fmt.Fprintln(cmd.OutOrStdout(), "config unchanged")
			return nil
		}
		verr := validateConfigDraft(draftPath)
		if verr == nil {
			if err := os.Rename(draftPath, path); err != nil {
				return fmt.Errorf("replace config file: %w", err)
			}
			keepDraft = true // renamed into place; nothing left to remove
			fmt.Fprintf(cmd.OutOrStdout(), "saved %s\n", path)
			return nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "The edited config is not valid: %v\n", verr)
		if !interactive {
			keepDraft = true
			return fmt.Errorf("config.toml was not changed; your edit is in %s", draftPath)
		}
		if !askReopenEditor(cmd.ErrOrStderr(), in) {
			return fmt.Errorf("config.toml was not changed; edit discarded")
		}
	}
}

// validateConfigDraft loads the draft exactly as fleet would load config.toml.
func validateConfigDraft(path string) error {
	_, err := core.LoadConfig(path)
	return err
}

func askReopenEditor(out io.Writer, in *bufio.Reader) bool {
	fmt.Fprint(out, "Re-open the editor to fix it? [Y/n] ")
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}
