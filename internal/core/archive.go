// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
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
	return extractCmdToDir(archivePath, path.Dir(archivePath))
}

// extractCmdToDir builds the shell command that extracts archiveFile into destDir.
// The archive format is detected from archiveFile's extension (so a staged copy
// must preserve the original base name). Both operands are shell-quoted.
func extractCmdToDir(archiveFile, destDir string) string {
	a := shellQuote(archiveFile)
	d := shellQuote(destDir)
	if strings.HasSuffix(strings.ToLower(archiveFile), ".zip") {
		return fmt.Sprintf("unzip -o -q %s -d %s", a, d)
	}
	// GNU/BSD tar auto-detect gzip/bzip2/xz from the stream with -xf.
	return fmt.Sprintf("tar -xf %s -C %s", a, d)
}

// compressArgv builds the (tool, args) for creating `archive` from base `names`
// in the working directory. tool is always a constant ("tar"/"zip"); operands are
// "./"-prefixed and tar gets a "--" terminator so a flag-shaped file name is
// always treated as a path, never an option.
func compressArgv(names []string, archive, format string) (tool string, args []string, err error) {
	if len(names) == 0 {
		return "", nil, fmt.Errorf("nothing selected to compress")
	}
	ops := make([]string, 0, len(names))
	for _, n := range names {
		ops = append(ops, "./"+path.Base(n))
	}
	out := "./" + path.Base(archive)
	switch format {
	case "zip":
		return "zip", append([]string{"-r", "-q", out}, ops...), nil
	case "tar.gz", "tgz":
		return "tar", append([]string{"-czf", out, "--"}, ops...), nil
	case "tar.bz2":
		return "tar", append([]string{"-cjf", out, "--"}, ops...), nil
	case "tar.xz":
		return "tar", append([]string{"-cJf", out, "--"}, ops...), nil
	case "tar":
		return "tar", append([]string{"-cf", out, "--"}, ops...), nil
	default:
		return "", nil, fmt.Errorf("unsupported archive format %q", format)
	}
}

// extractArgv builds the (tool, args) for extracting archivePath into dir. tool
// is always a constant ("tar"/"unzip").
func extractArgv(archivePath, dir string) (tool string, args []string) {
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return "unzip", []string{"-o", "-q", archivePath, "-d", dir}
	}
	return "tar", []string{"-xf", archivePath, "-C", dir}
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
		tool, args, err := compressArgv(names, archiveName, format)
		if err != nil {
			return err
		}
		if err := runLocalTool(dir, tool, args); err != nil {
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
//
// SECURITY: archive members are attacker-controlled. A member named "../evil"
// or "/etc/cron.d/x" (zip-slip / absolute-path traversal) would otherwise write
// outside the destination directory — and on a remote node the extract runs via
// shell.exec, escaping the file.* sandbox and its --file-root entirely. To
// prevent this:
//   - Local extraction is performed Go-natively (archive/zip + archive/tar),
//     routing every member through SafeLocalJoin(destDir, member) so an escaping
//     member is impossible to write by construction.
//   - For formats with no stdlib reader (tar.xz) and for remote extraction, the
//     archive's member list is enumerated first and the extraction is REFUSED if
//     any member is absolute or contains a ".." component; only an all-safe
//     archive is handed to the (shell-quoted) tar/unzip tool.
func (a *App) ExtractArchive(server, archivePath string) error {
	destDir := path.Dir(archivePath)
	if server == "" {
		if err := a.extractLocal(archivePath, destDir); err != nil {
			return err
		}
	} else {
		// Remote: copy the archive to a private, unpredictably-named temp, then
		// validate + extract THAT copy. Operating on the copy (not the original)
		// closes the validate/extract TOCTOU — a local attacker can no longer swap
		// the archive between the member-check and the extraction.
		staged, err := a.stageRemoteArchiveCopy(server, archivePath)
		if err != nil {
			return err
		}
		defer func() { _ = a.runRemoteShell(server, "rm -rf "+shellQuote(path.Dir(staged))) }()
		if err := a.validateRemoteArchiveMembers(server, staged); err != nil {
			return err
		}
		if err := a.runRemoteShell(server, extractCmdToDir(staged, destDir)); err != nil {
			return err
		}
	}
	a.auditArchive("file.extract", server, archivePath)
	return nil
}

// extractLocal extracts archivePath into destDir on the controller. zip and the
// tar family (tar/tar.gz/tar.bz2) are extracted Go-natively through
// SafeLocalJoin so zip-slip is impossible by construction. tar.xz has no stdlib
// decompressor, so it falls back to listing-and-validating members before
// invoking the constant-tool tar with a path-checked, shell-free exec.
func (a *App) extractLocal(archivePath, destDir string) error {
	low := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(low, ".zip"):
		return extractZipNative(archivePath, destDir)
	case strings.HasSuffix(low, ".tar.xz"):
		// No stdlib xz reader: list members via tar, refuse any escape, then
		// extract with the constant-tool path (no shell).
		if err := validateTarXZMembers(archivePath); err != nil {
			return err
		}
		tool, args := extractArgv(archivePath, destDir)
		return runLocalTool("", tool, args)
	default:
		// tar, tar.gz/.tgz, tar.bz2 — all handled by streaming readers.
		return extractTarNative(archivePath, destDir)
	}
}

// archiveMemberSafe reports whether an archive member name is safe to extract:
// it must not be absolute and must not escape the destination via a ".."
// component. Mirrors the rejection rules SafeLocalJoin enforces, but operates on
// the raw member name so it can vet a listing before any bytes are written.
func archiveMemberSafe(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	// Normalise separators: tar uses "/" but a crafted name may embed "\".
	slashed := strings.ReplaceAll(name, "\\", "/")
	if path.IsAbs(slashed) || strings.HasPrefix(slashed, "/") {
		return false
	}
	// Reject a drive-letter / UNC style absolute path too (defensive).
	if filepath.IsAbs(name) {
		return false
	}
	clean := path.Clean(slashed)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// extractZipNative extracts a zip archive, writing each member through
// SafeLocalJoin so a "../" or absolute member cannot escape destDir.
func extractZipNative(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		target, err := SafeLocalJoin(destDir, f.Name)
		if err != nil {
			return fmt.Errorf("refusing zip member %q: %w", f.Name, err)
		}
		info := f.FileInfo()
		if info.IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		// Skip symlinks and other non-regular members: a symlink could point
		// outside destDir and a later member could then be written through it.
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		if err := writeArchiveFile(target, rc, info.Mode().Perm()); err != nil {
			_ = rc.Close()
			return err
		}
		_ = rc.Close()
	}
	return nil
}

// extractTarNative extracts a tar / tar.gz / tar.bz2 archive, writing each
// member through SafeLocalJoin so an escaping member cannot leave destDir.
func extractTarNative(archivePath, destDir string) error {
	f, err := os.Open(archivePath) // #nosec G304 -- operator-chosen archive path
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	var src io.Reader = f
	low := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(low, ".tar.gz"), strings.HasSuffix(low, ".tgz"):
		gz, gerr := gzip.NewReader(f)
		if gerr != nil {
			return fmt.Errorf("open gzip: %w", gerr)
		}
		defer gz.Close()
		src = gz
	case strings.HasSuffix(low, ".tar.bz2"):
		src = bzip2.NewReader(f)
	}

	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		target, err := SafeLocalJoin(destDir, hdr.Name)
		if err != nil {
			return fmt.Errorf("refusing tar member %q: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			if err := writeArchiveFile(target, tr, os.FileMode(hdr.Mode&0o7777).Perm()); err != nil { // #nosec G115 -- masked to 0o7777
				return err
			}
		default:
			// Skip symlinks/hardlinks/devices: a symlink could later be used to
			// redirect a write outside destDir.
			continue
		}
	}
	return nil
}

// writeArchiveFile copies one archive member's bytes to target with the given
// perm. The copy is bounded by maxExtractedFileBytes so a decompression bomb (a
// member that inflates enormously) cannot exhaust the controller's disk.
func writeArchiveFile(target string, r io.Reader, perm os.FileMode) error {
	if perm == 0 {
		perm = 0o600
	}
	// Never follow an existing symlink at target (defense in depth alongside the
	// symlink-skip above); O_EXCL is too strict because directories may pre-exist
	// but the file itself should be created fresh.
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) // #nosec G304 -- target validated via SafeLocalJoin
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(r, maxExtractedFileBytes)); err != nil {
		return err
	}
	return nil
}

// maxExtractedFileBytes caps a single extracted member's size to bound a
// decompression bomb. 16 GiB is generous for real archives while still finite.
const maxExtractedFileBytes = int64(16) * 1024 * 1024 * 1024

// stageRemoteArchiveCopy copies archivePath into a fresh 0700 temp dir on the
// server (preserving the base name so format detection by extension still works)
// and returns the copy's path. Validating + extracting the COPY closes the
// validate/extract TOCTOU: both operations read this agent-private,
// unpredictably-named file, so a local attacker cannot swap the archive between
// the member-check and the extraction. The caller removes the temp dir.
func (a *App) stageRemoteArchiveCopy(server, archivePath string) (string, error) {
	qbase := shellQuote(path.Base(archivePath))
	cmd := `d=$(mktemp -d) && cp -- ` + shellQuote(archivePath) + ` "$d"/` + qbase +
		` && chmod 600 "$d"/` + qbase + ` && printf '%s' "$d"/` + qbase
	res, err := a.ExecCommand(server, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("stage archive copy: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	tmp := strings.TrimSpace(res.Stdout)
	if tmp == "" || !strings.Contains(tmp, "/") {
		return "", fmt.Errorf("stage archive copy: unexpected temp path %q", tmp)
	}
	return tmp, nil
}

// validateRemoteArchiveMembers lists the members of a remote archive via the
// agent's shell.exec (the same path the extract uses) and returns an error if
// ANY member is absolute or contains a "..". This blocks zip-slip on a node
// where extraction would otherwise run unsandboxed (outside --file-root).
func (a *App) validateRemoteArchiveMembers(server, archivePath string) error {
	cmd, parse := listMembersCmd(archivePath)
	res, err := a.ExecCommand(server, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("list archive members: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err := rejectUnsafeMembers(parse(res.Stdout)); err != nil {
		return err
	}
	// A name-only check cannot see member TYPE: a symlink/hardlink member (e.g.
	// "x" -> "/etc/cron.d") followed by a regular member written THROUGH it
	// ("x/evil") escapes the destination even though both names are
	// traversal-free, because tar/unzip follow the planted link on extract. List
	// members verbosely and refuse any non-regular/dir member.
	vres, err := a.ExecCommand(server, verboseListMembersCmd(archivePath))
	if err != nil {
		return err
	}
	if vres.ExitCode != 0 {
		return fmt.Errorf("list archive members (verbose): exit %d: %s", vres.ExitCode, strings.TrimSpace(vres.Stderr))
	}
	if unsafe, line := archiveListingHasUnsafeType(vres.Stdout); unsafe {
		return fmt.Errorf("refusing to extract archive: it contains a non-regular member (symlink/hardlink/special) that can write outside the destination: %q", strings.TrimSpace(line))
	}
	return nil
}

// verboseListMembersCmd builds the shell command (path shell-quoted) that lists
// an archive's members WITH ls-style type+permission info so a symlink/hardlink
// member can be detected. zip uses `unzip -Z`, the tar family `tar -tvf`.
func verboseListMembersCmd(archivePath string) string {
	a := shellQuote(archivePath)
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return fmt.Sprintf("unzip -Z %s", a)
	}
	return fmt.Sprintf("tar -tvf %s", a)
}

// archiveListingHasUnsafeType reports whether a verbose `tar -tvf` / `unzip -Z`
// listing contains a member that is not a regular file or directory — i.e. a
// symlink, hardlink, or device/fifo/socket — any of which lets an extractor
// follow a planted link and write OUTSIDE the destination. It keys off the
// leading ls-style type character of each member line; header/summary lines
// (which never begin with one of these type chars) are ignored.
func archiveListingHasUnsafeType(verbose string) (bool, string) {
	for _, raw := range strings.Split(verbose, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		switch line[0] {
		case 'l', 'h', 'c', 'b', 'p', 's':
			return true, line
		}
	}
	return false, ""
}

// validateTarXZMembers lists a local tar.xz's members via the constant-tool tar
// (no shell) and refuses extraction if any member escapes by name OR is a
// non-regular (symlink/hardlink/special) member.
func validateTarXZMembers(archivePath string) error {
	namesOut, err := exec.Command("tar", "-tf", archivePath).Output() // #nosec G204 -- constant tool, archive path is an argument, no shell
	if err != nil {
		return fmt.Errorf("list archive members: %w", err)
	}
	if err := rejectUnsafeMembers(splitMemberLines(string(namesOut))); err != nil {
		return err
	}
	typesOut, err := exec.Command("tar", "-tvf", archivePath).Output() // #nosec G204 -- constant tool, archive path is an argument, no shell
	if err != nil {
		return fmt.Errorf("list archive members (verbose): %w", err)
	}
	if unsafe, line := archiveListingHasUnsafeType(string(typesOut)); unsafe {
		return fmt.Errorf("refusing to extract archive: it contains a non-regular member (symlink/hardlink/special): %q", strings.TrimSpace(line))
	}
	return nil
}

// rejectUnsafeMembers returns an error naming the first member that would escape
// the destination directory, or nil if every member is safe.
func rejectUnsafeMembers(members []string) error {
	for _, m := range members {
		if m == "" {
			continue
		}
		if !archiveMemberSafe(m) {
			return fmt.Errorf("refusing to extract archive: member %q escapes the destination directory", m)
		}
	}
	return nil
}

// listMembersCmd builds the shell command (path shell-quoted) that lists an
// archive's members and a parser for its stdout. zip uses `unzip -Z1`, the tar
// family uses `tar -tf` (auto-detecting gz/bz2/xz from the stream).
func listMembersCmd(archivePath string) (cmd string, parse func(string) []string) {
	a := shellQuote(archivePath)
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return fmt.Sprintf("unzip -Z1 %s", a), splitMemberLines
	}
	return fmt.Sprintf("tar -tf %s", a), splitMemberLines
}

// splitMemberLines splits a tool's newline-separated member listing into trimmed
// member names.
func splitMemberLines(out string) []string {
	lines := strings.Split(out, "\n")
	members := make([]string, 0, len(lines))
	for _, l := range lines {
		if t := strings.TrimRight(l, "\r"); strings.TrimSpace(t) != "" {
			members = append(members, t)
		}
	}
	return members
}

// runLocalTool runs a FIXED tool (a constant tar/zip/unzip) with its arguments
// passed directly to exec — there is no shell, so neither command nor option
// injection is possible. Runs in dir when non-empty.
func runLocalTool(dir, tool string, args []string) error {
	cmd := exec.Command(tool, args...) // #nosec G204 -- tool is a constant selected by format; no shell, args not interpreted
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
