// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"regexp"
	"strings"
)

// Managed cron jobs are stored on the server inside the user's crontab, wrapped
// in per-job marker comments so fleet can add, list, and remove a single job
// without disturbing hand-written crontab entries:
//
//	# >>> fleet:<name> >>>
//	<5-field schedule> <command>
//	# <<< fleet:<name> <<<
//
// All editing happens controller-side over `crontab -l` / `crontab -` so the
// schedule and command never need to be re-quoted into a remote shell beyond a
// single safe heredoc/pipe.

// CronJob is one managed scheduled job parsed out of a crontab.
type CronJob struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
}

// cronNamePattern restricts a job name to a strict charset so it is safe to
// embed in a crontab marker comment and to use as a map key.
var cronNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidateCronName checks a job name against the allowed charset.
func ValidateCronName(name string) error {
	if !cronNamePattern.MatchString(name) {
		return fmt.Errorf("invalid cron name %q (use letters, digits, '.', '_', '-'; max 64 chars)", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("invalid cron name %q (must not contain '..')", name)
	}
	return nil
}

// ValidateCronSchedule checks that schedule is a plausible 5-field cron spec.
// It accepts the common @-shortcuts (@daily, @hourly, ...) and otherwise
// requires exactly five whitespace-separated fields drawn from a conservative
// charset (digits, * , - / and comma). It is deliberately strict so the value
// cannot smuggle in a newline or shell metacharacters when written back.
func ValidateCronSchedule(schedule string) error {
	s := strings.TrimSpace(schedule)
	if s == "" {
		return fmt.Errorf("cron schedule is required")
	}
	if strings.ContainsAny(s, "\n\r") {
		return fmt.Errorf("cron schedule must be a single line")
	}
	if strings.HasPrefix(s, "@") {
		switch s {
		case "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly":
			return nil
		default:
			return fmt.Errorf("unsupported cron shortcut %q", s)
		}
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return fmt.Errorf("cron schedule must have 5 fields (got %d): %q", len(fields), schedule)
	}
	fieldPattern := regexp.MustCompile(`^[0-9*/,\-]+$`)
	for i, f := range fields {
		if !fieldPattern.MatchString(f) {
			return fmt.Errorf("invalid character in cron field %d (%q)", i+1, f)
		}
	}
	return nil
}

// ValidateCronCommand rejects only what would break the single-line crontab
// entry format (embedded newlines); cron itself runs the command via /bin/sh,
// so arbitrary shell is intentionally allowed.
func ValidateCronCommand(command string) error {
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("cron command is required")
	}
	if strings.ContainsAny(command, "\n\r") {
		return fmt.Errorf("cron command must be a single line")
	}
	return nil
}

func cronStartMarker(name string) string { return "# >>> fleet:" + name + " >>>" }
func cronEndMarker(name string) string   { return "# <<< fleet:" + name + " <<<" }

// renderCronBlock renders the marker-wrapped block for a single managed job.
func renderCronBlock(job CronJob) string {
	return strings.Join([]string{
		cronStartMarker(job.Name),
		strings.TrimSpace(job.Schedule) + " " + strings.TrimSpace(job.Command),
		cronEndMarker(job.Name),
	}, "\n")
}

// ParseManagedCron extracts fleet-managed jobs from raw crontab content.
func ParseManagedCron(crontab string) []CronJob {
	lines := strings.Split(crontab, "\n")
	var jobs []CronJob
	for i := 0; i < len(lines); i++ {
		name, ok := matchCronStart(lines[i])
		if !ok {
			continue
		}
		// The entry line is the next non-marker line; the end marker follows.
		job := CronJob{Name: name}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == cronEndMarker(name) {
				i = j
				break
			}
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			schedule, command := splitCronEntry(lines[j])
			job.Schedule = schedule
			job.Command = command
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// matchCronStart reports whether line is a fleet start marker and returns the name.
func matchCronStart(line string) (string, bool) {
	const prefix = "# >>> fleet:"
	const suffix = " >>>"
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix)
	if name == "" {
		return "", false
	}
	return name, true
}

// splitCronEntry splits a crontab entry into its 5-field schedule and command.
// @-shortcut entries (single first field) are handled too.
func splitCronEntry(line string) (schedule, command string) {
	line = strings.TrimSpace(line)
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", ""
	}
	if strings.HasPrefix(fields[0], "@") {
		if len(fields) < 2 {
			return fields[0], ""
		}
		return fields[0], strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	}
	if len(fields) < 6 {
		return line, ""
	}
	schedule = strings.Join(fields[:5], " ")
	// Recover the command as the remainder after the 5th field to preserve
	// internal spacing, rather than re-joining split fields.
	rest := line
	for i := 0; i < 5; i++ {
		rest = strings.TrimSpace(rest)
		idx := strings.IndexAny(rest, " \t")
		if idx < 0 {
			rest = ""
			break
		}
		rest = rest[idx:]
	}
	return schedule, strings.TrimSpace(rest)
}

// UpsertManagedCron returns new crontab content with the named job's block
// removed (if present) and the new block appended. Passing a zero-value command
// is the caller's responsibility to avoid; use RemoveManagedCron to delete.
func UpsertManagedCron(crontab string, job CronJob) string {
	stripped := RemoveManagedCron(crontab, job.Name)
	block := renderCronBlock(job)
	stripped = strings.TrimRight(stripped, "\n")
	if stripped == "" {
		return block + "\n"
	}
	return stripped + "\n" + block + "\n"
}

// RemoveManagedCron returns new crontab content with the named job's block (and
// its markers) removed. Content outside the block is preserved verbatim.
func RemoveManagedCron(crontab, name string) string {
	lines := strings.Split(crontab, "\n")
	var out []string
	skipping := false
	for _, line := range lines {
		if !skipping {
			if n, ok := matchCronStart(line); ok && n == name {
				skipping = true
				continue
			}
			out = append(out, line)
			continue
		}
		// skipping: drop lines until (and including) the matching end marker.
		if line == cronEndMarker(name) {
			skipping = false
		}
	}
	result := strings.Join(out, "\n")
	// Collapse the run of blank lines a removal may leave behind, but keep a
	// single trailing newline when there is content.
	result = strings.TrimRight(result, "\n")
	if result == "" {
		return ""
	}
	return result + "\n"
}

// cronHeredocDelim is the quoted heredoc terminator used to feed new crontab
// content to `crontab -`. It is a sufficiently unique token (not a plausible
// crontab line) AND ContentBreaksCronHeredoc rejects any content that contains a
// line equal to it, so the heredoc can never be broken out of — mirroring the
// dead-man's-switch heredoc hardening in deadman.go.
const cronHeredocDelim = "FLEET_CRONTAB_EOF_b3f1c2a4"

// ContentBreaksCronHeredoc reports whether content contains a line that exactly
// equals the heredoc delimiter (ignoring a trailing CR). Such a line would
// terminate the heredoc early and let the remainder run as shell, so it must be
// rejected before the content is written back. Exported so callers can validate
// up front; CronWriteCommand also enforces it.
func ContentBreaksCronHeredoc(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimRight(line, "\r") == cronHeredocDelim {
			return true
		}
	}
	return false
}

// CronWriteCommand builds the remote shell command that writes newContent back
// to the user crontab via `crontab -`, using a quoted heredoc so the content is
// never interpreted by the shell.
//
// Heredoc safety: the body is fed through a quoted-delimiter heredoc (<<'EOF'),
// so $, `, \ inside commands are passed through literally. The ONLY way crafted
// content could escape such a heredoc is a line exactly equal to the delimiter;
// if newContent contains one, this returns a command that fails loudly on the
// remote (non-zero exit, which writeCrontab surfaces) and writes NOTHING, instead
// of emitting a breakable heredoc. The delimiter itself is a unique token that a
// real crontab line would never equal. This mirrors the deadman heredoc guard.
func CronWriteCommand(newContent string) string {
	if ContentBreaksCronHeredoc(newContent) {
		// Refuse on the remote without ever opening the heredoc, so the injected
		// delimiter line cannot start a shell. writeCrontab treats the non-zero exit
		// as a failure and reports it to the operator.
		return "echo 'fleet: refusing to write crontab: content contains the reserved heredoc delimiter' >&2; exit 1"
	}
	body := newContent
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	// Quoting the delimiter ('EOF') disables all expansion inside the heredoc,
	// so $, `, \ in commands are passed through literally.
	return "crontab - <<'" + cronHeredocDelim + "'\n" + body + cronHeredocDelim + "\n"
}
