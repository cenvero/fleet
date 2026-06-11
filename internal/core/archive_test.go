// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompressCmdNeutralizesOptionInjection(t *testing.T) {
	// A file literally named like a tar flag must reach tar as a path (./-prefixed),
	// never as a bare option.
	cmd, err := compressCmd("/srv/data", []string{"--checkpoint-action=exec=evil", "ok.txt"}, "out.tar.gz", "tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "'./--checkpoint-action=exec=evil'") {
		t.Fatalf("operand not ./-prefixed+quoted: %s", cmd)
	}
	if strings.Contains(cmd, " '--checkpoint") {
		t.Fatalf("flag-like operand leaked unprefixed: %s", cmd)
	}
}

func TestCompressCmdNeutralizesShellInjection(t *testing.T) {
	cmd, err := compressCmd("/d", []string{"a'; rm -rf ~ #"}, "o.zip", "zip")
	if err != nil {
		t.Fatal(err)
	}
	// The single quote in the name must be escaped via the '"'"' idiom, so it
	// cannot terminate the quoting and inject a command.
	if !strings.Contains(cmd, `'"'"'`) {
		t.Fatalf("single-quote in name not escaped: %s", cmd)
	}
}

func TestExtractCmdByExtension(t *testing.T) {
	if got := extractCmd("/srv/a.zip"); !strings.HasPrefix(got, "unzip ") {
		t.Fatalf("zip extract should use unzip: %s", got)
	}
	for _, p := range []string{"/srv/a.tar.gz", "/srv/a.tgz", "/srv/a.tar.xz", "/srv/a.tar"} {
		if got := extractCmd(p); !strings.HasPrefix(got, "tar -xf") {
			t.Fatalf("%s should use tar: %s", p, got)
		}
	}
}

func TestFormatFromName(t *testing.T) {
	cases := map[string]string{
		"x.zip": "zip", "x.tar.gz": "tar.gz", "x.tgz": "tar.gz",
		"x.tar.bz2": "tar.bz2", "x.tar.xz": "tar.xz", "x.tar": "tar", "x.bin": "tar.gz",
	}
	for name, want := range cases {
		if got := FormatFromName(name); got != want {
			t.Errorf("FormatFromName(%q)=%q want %q", name, got, want)
		}
	}
}

func TestArchiveMemberSafe(t *testing.T) {
	safe := []string{"a.txt", "dir/b.txt", "./c.txt", "a/b/c/d.txt", "weird..name.txt", "x/..y/z"}
	for _, m := range safe {
		if !archiveMemberSafe(m) {
			t.Errorf("expected %q to be safe", m)
		}
	}
	unsafe := []string{
		"../evil", "../../etc/passwd", "a/../../b", "/etc/passwd",
		"/abs/path", "dir/../../x", "..", "", "   ",
		"a/../..", `..\windows`, `dir\..\..\evil`,
	}
	for _, m := range unsafe {
		if archiveMemberSafe(m) {
			t.Errorf("expected %q to be rejected", m)
		}
	}
}

func TestRejectUnsafeMembers(t *testing.T) {
	if err := rejectUnsafeMembers([]string{"a.txt", "dir/b.txt"}); err != nil {
		t.Fatalf("all-safe listing should pass: %v", err)
	}
	err := rejectUnsafeMembers([]string{"ok.txt", "../evil", "more.txt"})
	if err == nil {
		t.Fatal("a listing containing ../evil must be rejected")
	}
	if !strings.Contains(err.Error(), "../evil") {
		t.Fatalf("error should name the offending member, got: %v", err)
	}
}

// TestExtractZipNativeRefusesZipSlip builds a real zip whose member name escapes
// the destination and confirms the Go-native extractor refuses it AND writes
// nothing outside the destination directory.
func TestExtractZipNativeRefusesZipSlip(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dest, "evil.zip")

	zf, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	// A traversal member that would land in `root` (one level above dest).
	w, err := zw.Create("../evil.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("pwned")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}

	err = extractZipNative(archivePath, dest)
	if err == nil {
		t.Fatal("zip-slip member must be refused")
	}
	if !strings.Contains(err.Error(), "escapes") && !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("unexpected error (want a refusal): %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "evil.txt")); statErr == nil {
		t.Fatal("zip-slip wrote a file outside the destination directory")
	}
}

// TestExtractTarNativeRefusesTraversal does the same for the tar extractor.
func TestExtractTarNativeRefusesTraversal(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dest, "evil.tar")

	tf, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(tf)
	body := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tf.Close(); err != nil {
		t.Fatal(err)
	}

	if err := extractTarNative(archivePath, dest); err == nil {
		t.Fatal("tar traversal member must be refused")
	}
	if _, statErr := os.Stat(filepath.Join(root, "evil.txt")); statErr == nil {
		t.Fatal("tar traversal wrote a file outside the destination directory")
	}
}

// TestExtractZipNativeExtractsSafeArchive confirms the hardened path still
// extracts a well-formed archive correctly (no regression).
func TestExtractZipNativeExtractsSafeArchive(t *testing.T) {
	dest := t.TempDir()
	archivePath := filepath.Join(dest, "ok.zip")

	zf, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for name, content := range map[string]string{"a.txt": "alpha", "sub/b.txt": "bravo"} {
		w, werr := zw.Create(name)
		if werr != nil {
			t.Fatal(werr)
		}
		if _, werr := w.Write([]byte(content)); werr != nil {
			t.Fatal(werr)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}

	if err := extractZipNative(archivePath, dest); err != nil {
		t.Fatalf("safe archive should extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	if err != nil {
		t.Fatalf("expected extracted file: %v", err)
	}
	if string(got) != "bravo" {
		t.Fatalf("extracted content mismatch: %q", got)
	}
}

func TestArchiveListingHasUnsafeType(t *testing.T) {
	safe := "-rw-r--r-- u/g 11 2026-01-01 file.txt\ndrwxr-xr-x u/g 0 2026-01-01 dir/\n"
	if unsafe, _ := archiveListingHasUnsafeType(safe); unsafe {
		t.Error("a regular-files-and-dirs listing must be safe")
	}
	symlink := "-rw-r--r-- u/g 11 2026-01-01 ok.txt\nlrwxrwxrwx u/g 0 2026-01-01 x -> /etc/cron.d\n"
	if unsafe, line := archiveListingHasUnsafeType(symlink); !unsafe {
		t.Error("a symlink member must be flagged unsafe")
	} else if !strings.Contains(line, "->") {
		t.Errorf("flagged the wrong line: %q", line)
	}
	if unsafe, _ := archiveListingHasUnsafeType("hrw-r--r-- u/g 0 2026-01-01 hl link to target\n"); !unsafe {
		t.Error("a hardlink member must be flagged unsafe")
	}
	withHeader := "Archive:  x.zip\n-rw-r--r--  3.0 unx 11 tx defN file.txt\n1 file, 11 bytes\n"
	if unsafe, _ := archiveListingHasUnsafeType(withHeader); unsafe {
		t.Error("zipinfo header/footer lines must not be flagged")
	}
}
