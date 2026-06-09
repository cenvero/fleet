// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cenvero/fleet/internal/logs"
)

// ChmodPath sets octal permissions (e.g. "755") on a path. server=="" → local.
func (a *App) ChmodPath(server, p, octalMode string) error {
	m, err := strconv.ParseUint(strings.TrimSpace(octalMode), 8, 32)
	if err != nil {
		return fmt.Errorf("invalid mode %q", octalMode)
	}
	if server == "" {
		return os.Chmod(p, os.FileMode(m)) // #nosec G302 -- operator-chosen mode
	}
	return a.runRemoteShell(server, fmt.Sprintf("chmod %s %s", shellQuote(octalMode), shellQuote(p)))
}

// ChecksumPath returns the SHA-256 of a file. server=="" → local.
func (a *App) ChecksumPath(server, p string) (string, error) {
	if server == "" {
		f, err := os.Open(p) // #nosec G304 -- operator-chosen path
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
	res, err := a.ExecCommand(server, "sha256sum "+shellQuote(p))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if fields := strings.Fields(res.Stdout); len(fields) > 0 {
		return fields[0], nil
	}
	return "", fmt.Errorf("empty checksum output")
}

// ArchiveFormats are the compression formats offered by the file managers.
func ArchiveFormats() []string { return []string{"zip", "tar.gz", "tar.bz2", "tar.xz", "tar"} }

// FormatFromName infers a compression format from an archive file name.
func FormatFromName(name string) string {
	low := strings.ToLower(name)
	switch {
	case strings.HasSuffix(low, ".zip"):
		return "zip"
	case strings.HasSuffix(low, ".tar.gz"), strings.HasSuffix(low, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(low, ".tar.bz2"):
		return "tar.bz2"
	case strings.HasSuffix(low, ".tar.xz"):
		return "tar.xz"
	case strings.HasSuffix(low, ".tar"):
		return "tar"
	default:
		return "tar.gz"
	}
}

// compressCmd builds the shell command that creates `archive` (a bare name) under
// `dir` from the given base `names`, all shell-quoted so file names can't inject.
func compressCmd(dir string, names []string, archive, format string) (string, error) {
	if len(names) == 0 {
		return "", fmt.Errorf("nothing selected to compress")
	}
	// Prefix every operand with "./" so a file literally named like a flag
	// (e.g. "--checkpoint-action=exec=...") is treated by tar/zip as a path, not
	// an option. Shell-quoting alone blocks shell injection but not this
	// argument-level option injection.
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = shellQuote("./" + path.Base(n))
	}
	items := strings.Join(quoted, " ")
	d := shellQuote(dir)
	out := shellQuote("./" + path.Base(archive))
	switch format {
	case "zip":
		return fmt.Sprintf("cd %s && rm -f %s && zip -r -q %s %s", d, out, out, items), nil
	case "tar.gz", "tgz":
		return fmt.Sprintf("cd %s && tar -czf %s %s", d, out, items), nil
	case "tar.bz2":
		return fmt.Sprintf("cd %s && tar -cjf %s %s", d, out, items), nil
	case "tar.xz":
		return fmt.Sprintf("cd %s && tar -cJf %s %s", d, out, items), nil
	case "tar":
		return fmt.Sprintf("cd %s && tar -cf %s %s", d, out, items), nil
	default:
		return "", fmt.Errorf("unsupported archive format %q", format)
	}
}

// extractCmd builds the shell command that extracts `archive` (full path) into its
// own directory.
func extractCmd(archivePath string) string {
	dir := shellQuote(path.Dir(archivePath))
	a := shellQuote(archivePath)
	low := strings.ToLower(archivePath)
	if strings.HasSuffix(low, ".zip") {
		return fmt.Sprintf("unzip -o -q %s -d %s", a, dir)
	}
	// GNU/BSD tar auto-detect gzip/bzip2/xz from the stream with -xf.
	return fmt.Sprintf("tar -xf %s -C %s", a, dir)
}

// compressArgv builds the argv (no shell) that creates `archive` from base
// `names` in the working directory. Operands are "./"-prefixed and tar gets a
// "--" terminator so a flag-shaped file name is always treated as a path.
func compressArgv(names []string, archive, format string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("nothing selected to compress")
	}
	ops := make([]string, 0, len(names))
	for _, n := range names {
		ops = append(ops, "./"+path.Base(n))
	}
	out := "./" + path.Base(archive)
	switch format {
	case "zip":
		return append([]string{"zip", "-r", "-q", out}, ops...), nil
	case "tar.gz", "tgz":
		return append([]string{"tar", "-czf", out, "--"}, ops...), nil
	case "tar.bz2":
		return append([]string{"tar", "-cjf", out, "--"}, ops...), nil
	case "tar.xz":
		return append([]string{"tar", "-cJf", out, "--"}, ops...), nil
	case "tar":
		return append([]string{"tar", "-cf", out, "--"}, ops...), nil
	default:
		return nil, fmt.Errorf("unsupported archive format %q", format)
	}
}

// extractArgv builds the argv (no shell) that extracts archivePath into dir.
func extractArgv(archivePath, dir string) []string {
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return []string{"unzip", "-o", "-q", archivePath, "-d", dir}
	}
	return []string{"tar", "-xf", archivePath, "-C", dir}
}

// CompressPaths creates an archive of `names` inside `dir`. server=="" runs on the
// controller locally via a direct tool exec (no shell); otherwise it runs on that
// server's agent. Operands are "./"-prefixed so a flag-shaped file name can't be
// read as an option.
func (a *App) CompressPaths(server, dir string, names []string, archiveName, format string) error {
	if server == "" {
		if format == "zip" {
			_ = os.Remove(filepath.Join(dir, path.Base(archiveName))) // zip appends; start fresh
		}
		argv, err := compressArgv(names, archiveName, format)
		if err != nil {
			return err
		}
		if err := runLocalTool(dir, argv); err != nil {
			return err
		}
	} else {
		cmd, err := compressCmd(dir, names, archiveName, format)
		if err != nil {
			return err
		}
		if err := a.runRemoteShell(server, cmd); err != nil {
			return err
		}
	}
	a.auditArchive("file.compress", server, path.Join(dir, archiveName))
	return nil
}

// ExtractArchive extracts an archive (full path) into its containing directory.
func (a *App) ExtractArchive(server, archivePath string) error {
	if server == "" {
		if err := runLocalTool("", extractArgv(archivePath, path.Dir(archivePath))); err != nil {
			return err
		}
	} else if err := a.runRemoteShell(server, extractCmd(archivePath)); err != nil {
		return err
	}
	a.auditArchive("file.extract", server, archivePath)
	return nil
}

// runLocalTool runs a FIXED tool (argv[0] is a constant tar/zip/unzip) with its
// arguments passed directly to exec — there is no shell, so neither command nor
// option injection is possible. Runs in dir when non-empty.
func runLocalTool(dir string, argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv[0] is a constant tool; args are not shell-interpreted
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runRemoteShell runs a /bin/sh command on a server's agent (paths shell-quoted).
func (a *App) runRemoteShell(server, command string) error {
	res, err := a.ExecCommand(server, command)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (a *App) auditArchive(action, server, target string) {
	where := server
	if where == "" {
		where = "local"
	}
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   action,
		Target:   where,
		Operator: a.operator(),
		Details:  target,
	})
}

// SuggestArchiveName proposes a default archive base name for a selection.
func SuggestArchiveName(names []string, format string) string {
	base := "archive"
	if len(names) == 1 {
		base = strings.TrimSuffix(path.Base(names[0]), filepath.Ext(names[0]))
		if base == "" {
			base = "archive"
		}
	}
	return base + "." + format
}
