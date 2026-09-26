// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/safetext"
	"github.com/cenvero/fleet/pkg/proto"
)

// parseTargetArgs accepts a remote file as <server> <path> or <server:path>.
func parseTargetArgs(args []string) (server, remotePath string, err error) {
	switch len(args) {
	case 1:
		return parseServerPath(args[0])
	case 2:
		if args[0] == "" || args[1] == "" {
			return "", "", fmt.Errorf("expected <server> <path>")
		}
		return args[0], args[1], nil
	default:
		return "", "", fmt.Errorf("expected <server> <path> or <server:path>")
	}
}

func newFileViewCommand(configDir *string) *cobra.Command {
	var lineRange string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:               "view <server> <path> | <server:path>",
		ValidArgsFunction: serverNameComp(configDir),
		Short:             "Show a text file with line numbers and the sha256 an edit can expect",
		Long: "Read a text file from a server (every chunk checksum-verified) and print it with\n" +
			"line numbers, preceded by a header with its sha256, size, mode and line count.\n" +
			"Pass that sha256 to `fleet file edit --expect-sha256` so the edit only applies\n" +
			"to the exact version you looked at. Line numbers are for reading and for\n" +
			"--insert-after; never copy them into --old text.\n\n" +
			"Binary files and files over the edit size limit (8 MiB by default, see\n" +
			"`fleet config set edit-max-size`) are refused; use `file tail` or `file download`.\n\n" +
			"  fleet file view web-01 /etc/nginx/nginx.conf\n" +
			"  fleet file view web-01:/etc/nginx/nginx.conf --lines 20:60\n" +
			"  fleet file view web-01 /etc/hosts --json      # content + sha256 as JSON",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, remotePath, err := parseTargetArgs(args)
			if err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			view, err := app.ViewRemoteFile(server, remotePath)
			if err != nil {
				return err
			}
			lines := splitViewLines(view.Content)
			start, end, err := parseLineRange(lineRange, len(lines))
			if err != nil {
				return err
			}
			if jsonOut {
				var content strings.Builder
				for _, l := range lines[max(start-1, 0):end] {
					content.WriteString(l)
				}
				return writeJSON(cmd, struct {
					core.FileView
					Start   int    `json:"start"`
					End     int    `json:"end"`
					Content string `json:"content"`
				}{view, start, end, content.String()})
			}
			out := cmd.OutOrStdout()
			endings := ""
			if view.CRLF {
				endings = ", CRLF line endings"
			}
			fmt.Fprintf(out, "# %s:%s  sha256=%s  %s  mode %04o  %d lines%s\n", server, remotePath, view.SHA256, humanizeBytes(view.Size), view.Mode&0o7777, view.Lines, endings)
			tty := writerIsTerminal(out)
			for i := max(start, 1); i <= end; i++ {
				line := strings.TrimRight(lines[i-1], "\r\n")
				if tty {
					line = safetext.Terminal(line, false)
				}
				fmt.Fprintf(out, "%6d\t%s\n", i, line)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&lineRange, "lines", "", "only show lines START:END (1-based, inclusive; START: or :END for open ranges)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the file (or the selected lines) and its metadata as JSON")
	return cmd
}

// terminalSafeLines makes a diff that carries file content from a server safe
// to show on a terminal: control characters (ESC included), bidi overrides and
// invalid UTF-8 in each line become visible escapes (safetext.Terminal), and
// the "\r" of CRLF line endings is dropped rather than shown. It is applied
// only when writing to a terminal; piped output (scripts, AI agents) stays
// byte-exact so text can be copied into --old.
func terminalSafeLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = safetext.Terminal(strings.TrimSuffix(l, "\r"), false)
	}
	return strings.Join(lines, "\n")
}

// splitViewLines splits content into lines that keep their line endings.
func splitViewLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	parts := bytes.SplitAfter(content, []byte("\n"))
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	lines := make([]string, len(parts))
	for i, p := range parts {
		lines[i] = string(p)
	}
	return lines
}

// parseLineRange parses START:END (1-based, inclusive) against a file of n
// lines. An empty spec selects every line.
func parseLineRange(spec string, n int) (int, int, error) {
	if strings.TrimSpace(spec) == "" {
		return min(1, n), n, nil
	}
	a, b, ok := strings.Cut(spec, ":")
	if !ok {
		b = a
	}
	start, end := 1, n
	var err error
	if strings.TrimSpace(a) != "" {
		if start, err = strconv.Atoi(strings.TrimSpace(a)); err != nil || start < 1 {
			return 0, 0, fmt.Errorf("invalid --lines %q: START must be a line number from 1", spec)
		}
	}
	if strings.TrimSpace(b) != "" {
		if end, err = strconv.Atoi(strings.TrimSpace(b)); err != nil || end < 1 {
			return 0, 0, fmt.Errorf("invalid --lines %q: END must be a line number from 1", spec)
		}
	}
	end = min(end, n)
	if start > end {
		if n == 0 {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("--lines %q selects nothing: the file has %d lines", spec, n)
	}
	return start, end, nil
}

type fileEditFlags struct {
	old, repl   string
	all         bool
	editsPath   string
	insertAfter int
	text        string
	contentPath string
	create      bool
	mode        string
	expect      string
	dryRun      bool
	force       bool
	jsonOut     bool
	undo        bool
	history     bool
	parallel    int
	chunkSize   string
}

func newFileEditCommand(configDir *string) *cobra.Command {
	var f fileEditFlags
	cmd := &cobra.Command{
		Use:               "edit <server> <path> | <server:path>",
		ValidArgsFunction: serverNameComp(configDir),
		Short:             "Edit a remote file in place: exact-text replace, insert, or whole content (safe for AI agents)",
		Long: "Change a file on a server without downloading and re-uploading it. The agent\n" +
			"applies the edit and saves it the way a careful editor does: the new content goes\n" +
			"to a temp file beside the original, is fsynced and read back to check its\n" +
			"sha256, gets the original's owner, group, mode (incl. setuid/setgid), ACLs,\n" +
			"SELinux label and other extended attributes, and then replaces the original in\n" +
			"one atomic rename. A connection that drops mid-edit changes nothing, a file is\n" +
			"never left half-written, and a lost reply is retried without applying the edit\n" +
			"twice. Editing through a symlink changes its target and keeps the link.\n" +
			"Hard-linked files, binary files (for text edits) and files the agent could not\n" +
			"write with its own permissions are refused and left unchanged.\n\n" +
			"Say what to change with exactly one of:\n" +
			"  --old TEXT --new TEXT [--all]   replace TEXT; it must match the file exactly\n" +
			"                                  (whitespace, indentation, line breaks) and\n" +
			"                                  exactly once unless --all is given\n" +
			"  --insert-after N --text TEXT    insert lines after line N (0 = top of file)\n" +
			"  --edits FILE|-                  several edits from a JSON list, applied in order,\n" +
			"                                  all or nothing:\n" +
			"                                  [{\"old\":\"a\",\"new\":\"b\"},\n" +
			"                                   {\"old\":\"x\",\"new\":\"y\",\"all\":true},\n" +
			"                                   {\"kind\":\"insert\",\"line\":12,\"text\":\"z\"}]\n" +
			"  --content FILE|-                replace the whole file; needs --expect-sha256\n" +
			"                                  (or --force), or --create for a new file\n" +
			"  --undo                          restore the version before the last Fleet edit,\n" +
			"                                  only if the file is unchanged since that edit\n" +
			"  --history                       list the versions kept for --undo\n" +
			"With none of these, the file opens in $EDITOR and is saved back the same safe\n" +
			"way when you quit (only if you changed it).\n\n" +
			"--expect-sha256 HASH (from `fleet file view`) makes the edit apply only if the\n" +
			"file still has that hash, so it cannot overwrite a change made in between.\n" +
			"--dry-run shows the diff without writing. Line endings are kept: text written\n" +
			"with \\n is converted for a CRLF file. Every edit prints a unified diff and is\n" +
			"audited; the previous version is kept on the controller for --undo (see\n" +
			"`fleet config set edit-backups`). Needs an agent with file.edit support\n" +
			"(update older agents with `fleet agent update <server>`).\n\n" +
			"  fleet file view web-01 /etc/nginx/nginx.conf\n" +
			"  fleet file edit web-01 /etc/nginx/nginx.conf --old 'worker_connections 768;' \\\n" +
			"      --new 'worker_connections 2048;' --expect-sha256 <sha256>\n" +
			"  fleet file edit web-01 /etc/hosts --insert-after 2 --text '10.0.0.5 db-01'\n" +
			"  fleet file edit web-01 /srv/app/.env --edits edits.json --dry-run\n" +
			"  fleet file edit web-01 /srv/app/config.yml --content ./config.yml --expect-sha256 <sha256>\n" +
			"  fleet file edit web-01 /etc/motd --content - --create --mode 0644 < motd.txt\n" +
			"  fleet file edit web-01 /etc/nginx/nginx.conf --undo\n" +
			"  fleet file edit web-01:/etc/nginx/nginx.conf           # interactive, $EDITOR",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, remotePath, err := parseTargetArgs(args)
			if err != nil {
				return err
			}
			return runFileEdit(cmd, *configDir, server, remotePath, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.old, "old", "", "exact text to replace (must occur once unless --all)")
	fl.StringVar(&f.repl, "new", "", "replacement text for --old (may be empty to delete)")
	fl.BoolVar(&f.all, "all", false, "with --old: replace every occurrence")
	fl.StringVar(&f.editsPath, "edits", "", "JSON list of edits to apply in order (file path, or - for stdin)")
	fl.IntVar(&f.insertAfter, "insert-after", 0, "insert --text after this line number (0 = at the top)")
	fl.StringVar(&f.text, "text", "", "with --insert-after: the lines to insert")
	fl.StringVar(&f.contentPath, "content", "", "replace the whole file with this local file's content (- for stdin)")
	fl.BoolVar(&f.create, "create", false, "with --content: create a new file (refuses to overwrite)")
	fl.StringVar(&f.mode, "mode", "", "with --create: permissions of the new file, e.g. 0644 (default 0644)")
	fl.StringVar(&f.expect, "expect-sha256", "", "only edit if the file's current sha256 is this (from `fleet file view`)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "show the diff and result without changing the file")
	fl.BoolVar(&f.force, "force", false, "with --content: replace the file without --expect-sha256")
	fl.BoolVar(&f.jsonOut, "json", false, "print the result (hashes, sizes, mode, owner, diff) as JSON")
	fl.BoolVar(&f.undo, "undo", false, "restore the version from before the last edit made through Fleet")
	fl.BoolVar(&f.history, "history", false, "list the previous versions kept for --undo")
	fl.IntVar(&f.parallel, "parallel", 0, "interactive mode fallback for old agents: parallel streams")
	fl.StringVar(&f.chunkSize, "chunk-size", "", "interactive mode fallback for old agents: chunk size, e.g. 4M")
	_ = fl.MarkHidden("parallel")
	_ = fl.MarkHidden("chunk-size")
	return cmd
}

func runFileEdit(cmd *cobra.Command, configDir, server, remotePath string, f fileEditFlags) error {
	fl := cmd.Flags()
	modes := 0
	for _, set := range []bool{fl.Changed("old"), fl.Changed("edits"), fl.Changed("insert-after"), fl.Changed("content"), f.undo, f.history} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return fmt.Errorf("give only one of --old/--new, --insert-after, --edits, --content, --undo or --history")
	}
	switch {
	case fl.Changed("old") != fl.Changed("new"):
		return fmt.Errorf("--old and --new go together (use --new '' to delete the text)")
	case f.all && !fl.Changed("old"):
		return fmt.Errorf("--all only applies to --old/--new")
	case fl.Changed("text") != fl.Changed("insert-after"):
		return fmt.Errorf("--insert-after and --text go together")
	case (f.create || f.force) && !fl.Changed("content"):
		return fmt.Errorf("--create and --force only apply to --content")
	case f.mode != "" && !f.create:
		return fmt.Errorf("--mode only applies with --create")
	case f.create && f.expect != "":
		return fmt.Errorf("--create makes a new file; --expect-sha256 does not apply")
	case (f.undo || f.history) && (f.expect != "" || f.dryRun):
		return fmt.Errorf("--undo and --history take no --expect-sha256 or --dry-run")
	}

	app, err := openApp(configDir)
	if err != nil {
		return err
	}
	defer app.Close()

	switch {
	case f.history:
		items, err := app.EditHistory(server, remotePath)
		if err != nil {
			return err
		}
		if f.jsonOut {
			return writeJSON(cmd, items)
		}
		if len(items) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "no edits of %s:%s are recorded on this controller\n", server, remotePath)
			return nil
		}
		for _, it := range items {
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s → %s  %s  by %s\n", it.Time.Local().Format("2006-01-02 15:04:05"), safetext.Terminal(it.Path, false), safetext.Terminal(shortHash(it.OldSHA256), false), safetext.Terminal(shortHash(it.NewSHA256), false), humanizeBytes(it.OldSize), safetext.Terminal(firstNonEmpty(it.Operator, "?"), false))
		}
		return nil
	case f.undo:
		res, err := app.UndoRemoteEdit(server, remotePath)
		if err != nil {
			return err
		}
		return printEditResult(cmd, server, remotePath, res, f.jsonOut, "restored")
	case modes == 0:
		if f.jsonOut || f.dryRun || f.expect != "" {
			return fmt.Errorf("say what to change (--old/--new, --insert-after, --edits or --content); --json, --dry-run and --expect-sha256 do not apply to interactive editing")
		}
		return runInteractiveEdit(cmd, app, configDir, server, remotePath, f)
	}

	req := core.EditRequest{Path: remotePath, BaseSHA256: strings.TrimSpace(f.expect), DryRun: f.dryRun}
	switch {
	case fl.Changed("old"):
		req.Ops = []proto.FileEditOp{{Kind: proto.FileEditOpReplace, Old: f.old, New: f.repl, All: f.all}}
	case fl.Changed("insert-after"):
		req.Ops = []proto.FileEditOp{{Kind: proto.FileEditOpInsert, Line: f.insertAfter, Text: f.text}}
	case fl.Changed("edits"):
		data, err := readEditSource(cmd, configDir, app, f.editsPath)
		if err != nil {
			return err
		}
		if req.Ops, err = parseEditList(data); err != nil {
			return err
		}
	case fl.Changed("content"):
		if !f.create && req.BaseSHA256 == "" && !f.force {
			return fmt.Errorf("replacing a whole file needs --expect-sha256 <hash from `fleet file view`> so it cannot overwrite a newer version (or --force to replace whatever is there, or --create for a new file)")
		}
		data, err := readEditSource(cmd, configDir, app, f.contentPath)
		if err != nil {
			return err
		}
		req.Replace, req.Content, req.Create = true, data, f.create
		if f.mode != "" {
			m, err := strconv.ParseUint(f.mode, 8, 32)
			if err != nil || m > 0o777 {
				return fmt.Errorf("invalid --mode %q: use octal permissions such as 0644", f.mode)
			}
			req.Mode = uint32(m)
		}
	}
	if req.BaseSHA256 != "" && !isHexSHA256(req.BaseSHA256) {
		return fmt.Errorf("--expect-sha256 must be a 64-character hex sha256 (from `fleet file view`)")
	}
	res, err := app.EditRemoteFile(server, req)
	if err != nil {
		return err
	}
	verb := "edited"
	if res.Created {
		verb = "created"
	}
	return printEditResult(cmd, server, remotePath, res, f.jsonOut, verb)
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range strings.ToLower(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readEditSource reads --content / --edits input from a file or stdin ("-").
// A scoped token may not read the controller's own protected files this way:
// sending the controller key as a file's content would hand out access to
// every server, exactly as `file upload` would.
func readEditSource(cmd *cobra.Command, configDir string, app *core.App, source string) ([]byte, error) {
	if source == "-" {
		return app.ReadEditInput(cmd.InOrStdin())
	}
	if err := refuseScopedProtectedPath(cmd, configDir, app, source, false); err != nil {
		return nil, err
	}
	fh, err := os.Open(source) // #nosec G304 -- operator-chosen local input file
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return app.ReadEditInput(fh)
}

// parseEditList decodes a JSON list of edit operations, rejecting unknown
// fields so a misspelt key cannot silently turn into a different edit.
func parseEditList(data []byte) ([]proto.FileEditOp, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var ops []proto.FileEditOp
	if err := dec.Decode(&ops); err != nil {
		return nil, fmt.Errorf("--edits: expected a JSON list like [{\"old\":\"a\",\"new\":\"b\"}, {\"kind\":\"insert\",\"line\":3,\"text\":\"c\"}]: %w", err)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("--edits: the list is empty")
	}
	return ops, nil
}

func printEditResult(cmd *cobra.Command, server, remotePath string, res core.EditResult, jsonOut bool, verb string) error {
	if res.BackupWarning != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+res.BackupWarning)
	}
	if jsonOut {
		return writeJSON(cmd, res)
	}
	out := cmd.OutOrStdout()
	// Everything below except the diff is metadata the agent reported (the
	// resolved path, hashes, owner names); show any control characters in it
	// as escapes, terminal or not.
	clean := func(s string) string { return safetext.Terminal(s, false) }
	if !res.Changed {
		fmt.Fprintf(out, "no change: %s:%s already has that content (sha256 %s)\n", server, remotePath, clean(res.NewSHA256))
		return nil
	}
	if res.DryRun {
		verb = "dry run, nothing written —"
	}
	changes := "1 change"
	if res.Edits != 1 {
		changes = fmt.Sprintf("%d changes", res.Edits)
	}
	target := server + ":" + remotePath
	if res.Path != "" && res.Path != remotePath {
		target += " → " + clean(res.Path)
	}
	check := ""
	if res.Verified {
		check = ", verified on disk"
	}
	fmt.Fprintf(out, "%s %s (%s%s)\n", verb, target, changes, check)
	fmt.Fprintf(out, "  sha256 %s → %s\n", clean(firstNonEmpty(res.OldSHA256, "(new file)")), clean(res.NewSHA256))
	fmt.Fprintf(out, "  size   %s → %s\n", humanizeBytes(res.OldSize), humanizeBytes(res.NewSize))
	owner := ""
	if res.Owner != "" {
		owner = "  owner " + clean(res.Owner) + ":" + clean(res.Group)
	}
	kept := ""
	if len(res.Preserved) > 0 {
		kept = "  kept " + clean(strings.Join(res.Preserved, ", "))
	}
	fmt.Fprintf(out, "  mode   %04o%s%s\n", res.Mode&0o7777, owner, kept)
	if res.Diff != "" {
		diff := res.Diff
		if writerIsTerminal(out) {
			diff = terminalSafeLines(diff)
		}
		fmt.Fprint(out, diff)
		if res.DiffTruncated {
			fmt.Fprintln(out, "(diff truncated)")
		}
	}
	if res.BackupID != "" {
		fmt.Fprintf(out, "undo with: fleet file edit %s %s --undo\n", server, remotePath)
	}
	return nil
}

// runInteractiveEdit opens the file in $EDITOR and saves it back with the same
// safe in-place edit, conditional on the version that was opened. If the save
// fails, the edited copy is kept and its path printed, so no work is lost.
func runInteractiveEdit(cmd *cobra.Command, app *core.App, configDir, server, remotePath string, f fileEditFlags) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec G115 -- a file descriptor fits in int
		return fmt.Errorf("interactive editing needs a terminal; say what to change with --old/--new, --insert-after, --edits or --content")
	}
	view, err := app.ViewRemoteFile(server, remotePath)
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "fleet-edit-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	record, err := app.GetServer(server)
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(tmpDir, core.TargetPathStyleForServer(record).Base(remotePath))
	if err := os.WriteFile(tmpPath, view.Content, 0o600); err != nil {
		return err
	}
	if err := openInEditor(cmd, configDir, tmpPath); err != nil {
		return err
	}
	edited, err := os.ReadFile(tmpPath) // #nosec G304 -- the controller's own temp file
	if err != nil {
		return err
	}
	if bytes.Equal(edited, view.Content) {
		fmt.Fprintln(cmd.OutOrStdout(), "no changes — nothing saved")
		return nil
	}
	res, err := app.EditRemoteFile(server, core.EditRequest{Path: remotePath, Replace: true, Content: edited, BaseSHA256: view.SHA256})
	if errors.Is(err, core.ErrEditUnsupported) {
		// An older agent: fall back to the checksummed atomic upload, which
		// cannot carry the file's owner over. Say so.
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v; saving with a plain upload instead, which does not keep the file's owner or extended attributes\n", err)
		opts, oerr := transferOptsFromFlags(f.parallel, f.chunkSize)
		if oerr != nil {
			keep = true
			return fmt.Errorf("%w (your edited copy is kept at %s)", oerr, tmpPath)
		}
		if err := os.Chmod(tmpPath, os.FileMode(view.Mode&0o777)); err != nil { // the upload sends the local mode
			keep = true
			return fmt.Errorf("%w (your edited copy is kept at %s)", err, tmpPath)
		}
		result, uerr := app.UploadFile(server, tmpPath, remotePath, opts, nil)
		if uerr != nil {
			keep = true
			return fmt.Errorf("upload after edit: %w (your edited copy is kept at %s)", uerr, tmpPath)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "saved %s:%s (%s, sha256=%s)\n", server, result.Path, humanizeBytes(result.Size), shortHash(result.SHA256))
		return nil
	}
	if err != nil {
		keep = true
		if core.EditErrorCode(err) == "edit_conflict" {
			return fmt.Errorf("%s:%s changed on the server while you were editing, so your version was not saved over it; your edited copy is kept at %s", server, remotePath, tmpPath)
		}
		return fmt.Errorf("%w (your edited copy is kept at %s)", err, tmpPath)
	}
	return printEditResult(cmd, server, remotePath, res, false, "saved")
}
