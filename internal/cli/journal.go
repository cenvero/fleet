// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/safetext"
	"github.com/cenvero/fleet/pkg/proto"
	"github.com/spf13/cobra"
)

// FL-022 — journal/service log tailing.
//
// The existing `fleet logs` command (in root.go) reads tracked-service logs and
// the controller audit log. This command pages or streams the systemd journal
// for an arbitrary unit over the live agent transport:
//
//	fleet journal <server> --unit <name> [--since 1h] [--follow] [--grep PATTERN] [-n N]
//
// It runs `journalctl -u <unit>` via App.ExecCommand. There is no streaming exec
// primitive on App, so --follow polls the journal on a short interval. Each poll
// asks journalctl only for the entries after the cursor it printed last time
// (--show-cursor / --after-cursor), so nothing is lost or repeated however fast
// the unit logs. When the remote journalctl supports it (systemd >= 237), the
// --grep filter runs server-side (journalctl --grep, matched against the
// message); the substring filter is still applied to every printed line.
//
// newJournalCommand is exported so root.go can register it with
// root.AddCommand(newJournalCommand(&configDir)).

func newJournalCommand(configDir *string) *cobra.Command {
	var unit, since, grep string
	var lines int
	var follow bool

	cmd := &cobra.Command{
		Use:               "journal <server> --unit <name>",
		ValidArgsFunction: serverNameComp(configDir),
		Short:             "Page or follow the systemd journal for a unit",
		Long: `Read journalctl -u <unit> from a server over the live agent transport.

  fleet journal web-01 --unit nginx
  fleet journal web-01 --unit nginx --since 1h -n 200
  fleet journal web-01 --unit nginx --follow --grep error`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			if strings.TrimSpace(unit) == "" {
				return fmt.Errorf("--unit is required")
			}
			if err := validUnitName(unit); err != nil {
				return err
			}
			if err := validJournalSince(since); err != nil {
				return err
			}
			if lines <= 0 {
				lines = 200
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			if follow {
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
				defer stop()
				return followJournal(ctx, cmd, app, server, unit, since, grep, lines)
			}
			return pageJournal(cmd, app, server, unit, since, grep, lines)
		},
	}
	cmd.Flags().StringVar(&unit, "unit", "", "systemd unit name (required)")
	cmd.Flags().StringVar(&since, "since", "", "journalctl --since window, e.g. '1h' or '2024-01-01'")
	cmd.Flags().StringVar(&grep, "grep", "", "only show lines matching this substring (case-insensitive)")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "maximum number of lines to return")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream new journal lines until Ctrl-C")
	return cmd
}

// journalCommand builds the plain remote journalctl invocation. tail caps the
// number of returned lines.
func journalCommand(unit, since string, tail int) string {
	return journalQuery{unit: unit, since: since, tail: tail}.command()
}

// journalQuery is one journalctl invocation. Every value that can come from a
// user, an AI agent or the remote host is shell-quoted as a single word, and
// the options carrying such values use the --option=value form so the value
// can never be parsed as an option of its own.
type journalQuery struct {
	unit  string
	since string // ignored when cursor is set (journalctl rejects both)
	// cursor resumes after a cursor printed by --show-cursor.
	cursor string
	// pattern is a journalctl --grep regular expression (see journalPattern).
	pattern    string
	tail       int // -n; 0 = no limit
	quiet      bool
	showCursor bool
}

func (q journalQuery) command() string {
	parts := []string{"journalctl", "-u", shellQuote(q.unit), "--no-pager"}
	if q.quiet {
		parts = append(parts, "-q")
	}
	if q.cursor != "" {
		parts = append(parts, shellQuote("--after-cursor="+q.cursor))
	} else if q.since != "" {
		parts = append(parts, "--since", shellQuote(q.since))
	}
	if q.tail > 0 {
		parts = append(parts, "-n", strconv.Itoa(q.tail))
	}
	if q.pattern != "" {
		parts = append(parts, shellQuote("--grep="+q.pattern), "--case-sensitive=false")
	}
	if q.showCursor {
		parts = append(parts, "--show-cursor")
	}
	return strings.Join(parts, " ")
}

// journalExec runs one command on the server (App.ExecCommand in production).
type journalExec func(command string) (proto.ExecResult, error)

// journalExitError is a journalctl run that exited non-zero, as opposed to a
// transport failure; only these are worth retrying with fewer features.
type journalExitError struct{ err error }

func (e journalExitError) Error() string { return e.err.Error() }
func (e journalExitError) Unwrap() error { return e.err }

const journalCursorPrefix = "-- cursor: "

// runJournal executes the remote journalctl and returns its non-empty output
// lines. journalctl exits 0 with a "-- No entries --" banner when empty; the
// caller treats that as no lines via grep/printing logic. With --show-cursor
// the trailing "-- cursor: ..." line is removed from the lines and returned
// separately ("" when journalctl printed none, e.g. for an empty journal).
func runJournal(exec journalExec, server string, q journalQuery) ([]string, string, error) {
	res, err := exec(q.command())
	if err != nil {
		return nil, "", err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("journalctl exited %d", res.ExitCode)
		}
		return nil, "", journalExitError{fmt.Errorf("journal read on %s failed: %s", server, msg)}
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	cursor := ""
	if q.showCursor && len(out) > 0 {
		if last := out[len(out)-1]; strings.HasPrefix(last, journalCursorPrefix) {
			out = out[:len(out)-1]
			if c := strings.TrimSpace(strings.TrimPrefix(last, journalCursorPrefix)); validJournalCursor(c) {
				cursor = c
			}
		}
	}
	return out, cursor, nil
}

// validJournalCursor accepts the sd-journal cursor format
// ("s=<hex>;i=<hex>;b=<hex>;m=<hex>;t=<hex>;x=<hex>"). It is shell-quoted
// anyway; this just refuses anything that is not a cursor.
func validJournalCursor(c string) bool {
	if c == "" || len(c) > 512 {
		return false
	}
	for _, r := range c {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '=', r == ';', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func matchGrep(line, grep string) bool {
	if grep == "" {
		return true
	}
	return strings.Contains(strings.ToLower(line), strings.ToLower(grep))
}

// journalPattern turns the --grep substring into a journalctl --grep regular
// expression matching it literally: lowercased (journalctl is case-insensitive
// for an all-lowercase pattern; --case-sensitive=false is passed as well) with
// every ASCII non-alphanumeric escaped, which PCRE2 always reads as a literal.
// It returns "" (filter client-side only) for patterns this cannot express
// faithfully: non-ASCII (PCRE2 case folding differs from strings.ToLower),
// control characters, or very long ones.
func journalPattern(grep string) string {
	if grep == "" || len(grep) > 256 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(grep); i++ {
		c := grep[i]
		switch {
		case c < 0x20 || c > 0x7e:
			return ""
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		default:
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	return b.String()
}

// journalSupportsGrep reports whether the server's journalctl has --grep:
// systemd 237 or newer, built with PCRE2.
func journalSupportsGrep(exec journalExec) bool {
	res, err := exec("journalctl --version")
	if err != nil || res.ExitCode != 0 {
		return false
	}
	if strings.Contains(res.Stdout, "-PCRE2") {
		return false
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) < 2 || fields[0] != "systemd" {
		return false
	}
	version, err := strconv.Atoi(fields[1])
	return err == nil && version >= 237
}

// serverJournalPattern is the pattern to push to journalctl for grep, or "".
func serverJournalPattern(exec journalExec, grep string) string {
	pattern := journalPattern(grep)
	if pattern == "" || !journalSupportsGrep(exec) {
		return ""
	}
	return pattern
}

func printJournalLines(out io.Writer, lines []string, grep string) {
	for _, line := range lines {
		if matchGrep(line, grep) {
			fmt.Fprintln(out, safetext.Terminal(line, false))
		}
	}
}

func pageJournal(cmd *cobra.Command, app *core.App, server, unit, since, grep string, lines int) error {
	exec := func(command string) (proto.ExecResult, error) { return app.ExecCommand(server, command) }
	return pageJournalTo(cmd.OutOrStdout(), exec, server, unit, since, grep, lines)
}

func pageJournalTo(w io.Writer, exec journalExec, server, unit, since, grep string, lines int) error {
	q := journalQuery{unit: unit, since: since, tail: lines, pattern: serverJournalPattern(exec, grep)}
	out, _, err := runJournal(exec, server, q)
	if err != nil && q.pattern != "" && errors.As(err, new(journalExitError)) {
		// --grep advertised but unusable (e.g. PCRE2 missing at runtime).
		q.pattern = ""
		out, _, err = runJournal(exec, server, q)
	}
	if err != nil {
		return err
	}
	printJournalLines(w, out, grep)
	return nil
}

// followJournal prints the recent journal window and then every new entry
// until ctx is cancelled.
func followJournal(ctx context.Context, cmd *cobra.Command, app *core.App, server, unit, since, grep string, lines int) error {
	exec := func(command string) (proto.ExecResult, error) { return app.ExecCommand(server, command) }
	f := newJournalFollower(exec, cmd.OutOrStdout(), server, unit, since, grep, lines)
	if err := f.start(); err != nil {
		return err
	}
	interval := core.DefaultLogFollowInterval
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
		for {
			more, err := f.poll()
			if err != nil {
				return err
			}
			if !more || ctx.Err() != nil {
				break
			}
		}
	}
}

// journalFollowPage is how many entries one cursor poll asks for; a full page
// is followed immediately by the next.
var journalFollowPage = 1000

// journalFollower tracks what has been printed while following the journal.
//
// With a cursor, each poll runs
//
//	journalctl -u UNIT --no-pager -q '--after-cursor=C' -n PAGE [--grep] --show-cursor
//
// and prints exactly the entries after the last one printed. Without one (the
// journal was empty, the cursor was rejected, or journalctl lacks
// --show-cursor), it falls back to the original approach: re-read the last
// `lines` entries and print what follows the last line printed.
type journalFollower struct {
	exec   journalExec
	out    io.Writer
	server string
	unit   string
	since  string
	grep   string
	lines  int

	pattern  string // server-side --grep pattern, "" for none
	cursors  bool   // journalctl accepts --show-cursor
	cursor   string // resume point, "" for none
	lastSeen string // last output line, for the fallback de-duplication
}

func newJournalFollower(exec journalExec, out io.Writer, server, unit, since, grep string, lines int) *journalFollower {
	return &journalFollower{exec: exec, out: out, server: server, unit: unit, since: since, grep: grep, lines: lines, cursors: true}
}

// start prints the initial window: the page-mode command plus --show-cursor
// (and --grep when available). If journalctl rejects those options it retries
// without them, ending with exactly the original command.
func (f *journalFollower) start() error {
	f.pattern = serverJournalPattern(f.exec, f.grep)
	for {
		lines, cursor, err := runJournal(f.exec, f.server, f.tailQuery())
		if err == nil {
			f.print(lines)
			f.cursor = cursor
			return nil
		}
		if !errors.As(err, new(journalExitError)) {
			return err
		}
		switch {
		case f.pattern != "":
			f.pattern = ""
		case f.cursors:
			f.cursors = false
		default:
			return err
		}
	}
}

// poll prints what is new. more reports a full page: poll again right away.
func (f *journalFollower) poll() (bool, error) {
	if f.cursor != "" {
		q := journalQuery{unit: f.unit, cursor: f.cursor, tail: journalFollowPage, pattern: f.pattern, quiet: true, showCursor: true}
		lines, cursor, err := runJournal(f.exec, f.server, q)
		if err != nil && !errors.As(err, new(journalExitError)) {
			return false, err
		}
		if err == nil && cursor != "" {
			f.print(lines)
			f.cursor = cursor
			return len(lines) >= journalFollowPage, nil
		}
		// The cursor was rejected (its entry was vacuumed) or the cursor line
		// is missing: fall back to re-reading the tail, de-duplicated.
		f.cursor = ""
	}
	batch, cursor, err := runJournal(f.exec, f.server, f.tailQuery())
	if err != nil {
		return false, err
	}
	fresh := newJournalLines(batch, f.lastSeen)
	f.printed(fresh)
	if len(batch) > 0 {
		f.lastSeen = batch[len(batch)-1]
	}
	f.cursor = cursor
	return false, nil
}

func (f *journalFollower) tailQuery() journalQuery {
	return journalQuery{unit: f.unit, since: f.since, tail: f.lines, pattern: f.pattern, showCursor: f.cursors}
}

func (f *journalFollower) print(lines []string) {
	f.printed(lines)
	if len(lines) > 0 {
		f.lastSeen = lines[len(lines)-1]
	}
}

func (f *journalFollower) printed(lines []string) {
	printJournalLines(f.out, lines, f.grep)
}

// newJournalLines returns the lines in batch that appear after the last
// occurrence of lastSeen. When lastSeen is absent from the batch (log rotated or
// the tail window moved past it) the whole batch is treated as new.
func newJournalLines(batch []string, lastSeen string) []string {
	if lastSeen == "" {
		return batch
	}
	for i := len(batch) - 1; i >= 0; i-- {
		if batch[i] == lastSeen {
			return batch[i+1:]
		}
	}
	return batch
}

// validJournalSince guards the --since value so it cannot smuggle shell words
// into the remote command. journalctl accepts free-form times, but we restrict
// to a safe character set (digits, letters, space, dash, colon, dot).
func validJournalSince(since string) error {
	since = strings.TrimSpace(since)
	if since == "" {
		return nil
	}
	if len(since) > 64 {
		return fmt.Errorf("--since value too long")
	}
	for _, r := range since {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ', r == '-', r == ':', r == '.', r == '+':
		default:
			return fmt.Errorf("invalid --since value %q: contains %q", since, string(r))
		}
	}
	return nil
}
