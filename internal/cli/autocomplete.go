// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// completionLine returns the rc line that enables tab-completion for a shell by
// sourcing fleet's generator live. This is the OLD, slow path — it forks `fleet`
// on every shell startup — kept only so `fleet autocomplete` can show what the
// manual one-liner would be. `fleet autocomplete install` no longer uses it; it
// writes a cached completion file instead (see installCompletion).
func completionLine(sh string) (string, bool) {
	switch sh {
	case "bash":
		return "source <(fleet completion bash)", true
	case "zsh":
		return "source <(fleet completion zsh)", true
	case "fish":
		return "fleet completion fish | source", true
	default:
		return "", false
	}
}

func newAutocompleteCommand() *cobra.Command {
	var shellOverride string
	cmd := &cobra.Command{
		Use:   "autocomplete",
		Short: "Enable shell tab-completion for fleet (cached, no per-shell fleet fork)",
		Long: "Install fleet tab-completion as a CACHED file your shell loads once, rather\n" +
			"than re-running 'fleet completion' on every new shell. Completion includes\n" +
			"live server names (e.g. 'fleet exec <tab>').\n\n" +
			"  fleet autocomplete install   # install the cached completion for your shell\n" +
			"  fleet autocomplete           # print the cached-install instructions",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sh := shellOverride
			if sh == "" {
				sh = currentShellName()
			}
			if _, ok := completionLine(sh); !ok {
				return fmt.Errorf("unsupported shell %q (try --shell bash|zsh|fish)", sh)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Install cached fleet tab-completion for %s with:\n\n  fleet autocomplete install\n\n"+
					"This writes a completion file your shell loads once (no 'fleet' fork on every\n"+
					"new shell), and includes live server-name completion.\n", sh)
			return nil
		},
	}
	cmd.Flags().StringVar(&shellOverride, "shell", "", "shell to target (bash|zsh|fish); default: $SHELL")
	cmd.AddCommand(newAutocompleteInstallCommand(&shellOverride))
	return cmd
}

func newAutocompleteInstallCommand(shellOverride *string) *cobra.Command {
	var installShell string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install tab-completion as a cached file (no per-shell fleet fork)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sh := installShell
			if sh == "" {
				sh = *shellOverride
			}
			if sh == "" {
				sh = currentShellName()
			}
			script, err := generateCompletion(cmd.Root(), sh)
			if err != nil {
				return err
			}
			switch sh {
			case "zsh":
				return installZshCompletion(cmd, script)
			case "fish":
				return installFishCompletion(cmd, script)
			case "bash":
				return installBashCompletion(cmd, script)
			default:
				return fmt.Errorf("unsupported shell %q (try --shell bash|zsh|fish)", sh)
			}
		},
	}
	cmd.Flags().StringVar(&installShell, "shell", "", "shell to target (bash|zsh|fish); default: $SHELL")
	return cmd
}

// generateCompletion renders the completion script for a shell into memory using
// Cobra's generators — the same output as `fleet completion <sh>`, just captured
// so we can write it to a cached file once instead of re-running it every shell.
func generateCompletion(root *cobra.Command, sh string) ([]byte, error) {
	var buf bytes.Buffer
	var err error
	switch sh {
	case "bash":
		err = root.GenBashCompletionV2(&buf, true)
	case "zsh":
		err = root.GenZshCompletion(&buf)
	case "fish":
		err = root.GenFishCompletion(&buf, true)
	default:
		return nil, fmt.Errorf("unsupported shell %q (try --shell bash|zsh|fish)", sh)
	}
	return buf.Bytes(), err
}

// installZshCompletion writes the completion as `_fleet` into a directory on
// zsh's $fpath, so compinit loads it ONCE (cached in ~/.zcompdump) with no
// per-shell `fleet` fork. It also removes the legacy `source <(fleet completion
// zsh)` line if a previous install added it.
func installZshCompletion(cmd *cobra.Command, script []byte) error {
	out := cmd.OutOrStdout()
	dir, needFpathLine := zshCompletionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- a shared zsh completions dir is world-readable by design
		return fmt.Errorf("create completion dir %s: %w", dir, err)
	}
	target := filepath.Join(dir, "_fleet")
	if err := os.WriteFile(target, script, 0o644); err != nil { // #nosec G306 -- completion scripts are non-secret and must be world-readable
		return fmt.Errorf("write %s: %w", target, err)
	}

	rc, rcErr := shellRCPathFor("zsh")
	removedLegacy := false
	if rcErr == nil {
		removedLegacy, _ = removeMarkedBlock(rc, "cenvero-fleet:completion")
		if needFpathLine {
			// The fallback dir isn't on the default $fpath; add it (before a compinit)
			// so the new _fleet is found. Brew/site-functions dirs skip this.
			_, _ = appendOnce(rc, "cenvero-fleet:fpath",
				"# cenvero-fleet:fpath\nfpath=(\""+dir+"\" $fpath)\nautoload -Uz compinit && compinit\n")
		}
	}
	// Drop stale completion dumps so the next shell rebuilds and picks up _fleet.
	if home, err := os.UserHomeDir(); err == nil {
		if matches, _ := filepath.Glob(filepath.Join(home, ".zcompdump*")); matches != nil {
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}
	}

	fmt.Fprintf(out, "installed zsh completion → %s\n", target)
	if removedLegacy {
		fmt.Fprintf(out, "removed the old live-loading line from %s (it forked fleet on every new shell)\n", rc)
	}
	fmt.Fprintln(out, "Open a new shell or run: exec zsh")
	return nil
}

// installFishCompletion writes fleet.fish into fish's auto-loaded completions
// directory; fish loads it on demand, cached, with no fork.
func installFishCompletion(cmd *cobra.Command, script []byte) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "fish", "completions")
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- fish completions dir is non-secret
		return fmt.Errorf("create completion dir %s: %w", dir, err)
	}
	target := filepath.Join(dir, "fleet.fish")
	if err := os.WriteFile(target, script, 0o644); err != nil { // #nosec G306 -- completion scripts are non-secret
		return fmt.Errorf("write %s: %w", target, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "installed fish completion → %s\nOpen a new shell to use it.\n", target)
	return nil
}

// installBashCompletion writes the completion to a cached file and sources THAT
// from ~/.bashrc (a static read, no `source <(fleet ...)` fork), migrating the
// old live-fork line if present.
func installBashCompletion(cmd *cobra.Command, script []byte) error {
	out := cmd.OutOrStdout()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	target := filepath.Join(home, ".cenvero-fleet-completion.bash")
	if err := os.WriteFile(target, script, 0o644); err != nil { // #nosec G306 -- completion scripts are non-secret
		return fmt.Errorf("write %s: %w", target, err)
	}
	rc, err := shellRCPathFor("bash")
	if err != nil {
		return err
	}
	removedLegacy, _ := removeMarkedBlock(rc, "cenvero-fleet:completion")
	added, err := appendOnce(rc, "cenvero-fleet:completion-file",
		"# cenvero-fleet:completion-file\n[ -f \""+target+"\" ] && source \""+target+"\"\n")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "installed bash completion → %s\n", target)
	if removedLegacy {
		fmt.Fprintf(out, "removed the old live-loading line from %s\n", rc)
	}
	if added {
		fmt.Fprintf(out, "added a cached source line to %s\n", rc)
	}
	fmt.Fprintf(out, "Open a new shell or run: source %s\n", rc)
	return nil
}

// zshCompletionDir returns the best directory to drop `_fleet` into and whether
// an explicit fpath line is needed. It prefers a Homebrew site-functions dir
// (already on $fpath and compinit-cached → no rc edit). Only the user-local
// fallback needs an fpath line.
func zshCompletionDir() (dir string, needFpathLine bool) {
	candidates := make([]string, 0, 3)
	if p := strings.TrimSpace(os.Getenv("HOMEBREW_PREFIX")); p != "" {
		candidates = append(candidates, filepath.Join(p, "share", "zsh", "site-functions"))
	}
	candidates = append(candidates,
		"/opt/homebrew/share/zsh/site-functions",
		"/usr/local/share/zsh/site-functions",
	)
	for _, d := range candidates {
		if dirIsWritable(d) {
			return d, false
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".zsh", "completions"), true
}

// dirIsWritable reports whether dir exists and the current user can create files
// in it (probed with a temp file, since stat-mode checks lie under ACLs).
func dirIsWritable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	f, err := os.CreateTemp(dir, ".fleet-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// removeMarkedBlock strips a `# <marker>` comment line plus the lines that follow
// it up to the next blank line (the shape appendOnce writes), collapsing the
// surrounding blank lines. It preserves the file's existing mode. Returns whether
// anything was removed.
func removeMarkedBlock(path, marker string) (bool, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator's own shell rc file
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	removed := false
	for i := 0; i < len(lines); i++ {
		if strings.Contains(lines[i], marker) {
			removed = true
			// Drop any blank line we just appended before the block.
			for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			// Skip the marker line and following non-blank lines.
			i++
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
				i++
			}
			// i now points at a blank line (or EOF); the loop's i++ skips it too.
			continue
		}
		out = append(out, lines[i])
	}
	if !removed {
		return false, nil
	}
	// WriteFile keeps an existing file's mode (perm applies only on create).
	return true, os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o600) // #nosec G306 -- existing rc keeps its mode; 0600 only applies if newly created
}
