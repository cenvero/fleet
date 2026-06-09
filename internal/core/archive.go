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
	return a.runShell(server, fmt.Sprintf("chmod %s %s", shellQuote(octalMode), shellQuote(p)))
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

// CompressPaths creates an archive of `names` inside `dir`. server=="" runs on the
// controller's local filesystem; otherwise it runs on that server's agent. Uses
// the host's tar/zip; names are shell-quoted so they cannot inject.
func (a *App) CompressPaths(server, dir string, names []string, archiveName, format string) error {
	cmd, err := compressCmd(dir, names, archiveName, format)
	if err != nil {
		return err
	}
	if err := a.runShell(server, cmd); err != nil {
		return err
	}
	a.auditArchive("file.compress", server, path.Join(dir, archiveName))
	return nil
}

// ExtractArchive extracts an archive (full path) into its containing directory.
func (a *App) ExtractArchive(server, archivePath string) error {
	if err := a.runShell(server, extractCmd(archivePath)); err != nil {
		return err
	}
	a.auditArchive("file.extract", server, archivePath)
	return nil
}

// runShell runs a /bin/sh command locally (server=="") or on a server's agent.
func (a *App) runShell(server, command string) error {
	if server == "" {
		out, err := exec.Command("/bin/sh", "-c", command).CombinedOutput() // #nosec G204 -- operator-driven, paths shell-quoted
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
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
