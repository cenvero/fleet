// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

func editReplace(path, old, repl string) proto.FileEditPayload {
	return proto.FileEditPayload{Path: path, Ops: []proto.FileEditOp{{Kind: proto.FileEditOpReplace, Old: old, New: repl}}}
}

func editErrCode(t *testing.T, err error) string {
	t.Helper()
	var re *RPCError
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not an *RPCError", err)
	}
	return re.Code
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // not subject to the umask
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertNoTempFiles fails if an edit left a temp file behind in dir.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".fleet-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestEditReplacePreservesModeAndReportsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	writeTestFile(t, path, "port=80\nname=web\n", 0o640)
	before, _ := os.Stat(path)

	res, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(path, "port=80", "port=8080"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, path); got != "port=8080\nname=web\n" {
		t.Fatalf("content = %q", got)
	}
	after, _ := os.Stat(path)
	if after.Mode().Perm() != 0o640 || res.Mode != 0o640 {
		t.Fatalf("mode = %v (reported %o), want 0640", after.Mode().Perm(), res.Mode)
	}
	if os.SameFile(before, after) {
		t.Fatal("expected the file to be replaced by a new inode (atomic rename)")
	}
	if !res.Changed || !res.Verified || res.Edits != 1 || res.OldSHA256 == res.NewSHA256 {
		t.Fatalf("result = %+v", res)
	}
	if res.NewSHA256 != sha256Hex([]byte("port=8080\nname=web\n")) {
		t.Fatalf("new sha256 %s does not match the content", res.NewSHA256)
	}
	for _, want := range []string{"-port=80", "+port=8080", " name=web"} {
		if !strings.Contains(res.Diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, res.Diff)
		}
	}
	assertNoTempFiles(t, dir)
}

func TestEditThroughSymlinkKeepsLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sites-available.conf")
	link := filepath.Join(dir, "sites-enabled.conf")
	writeTestFile(t, target, "root /srv/a;\n", 0o644)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	res, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(link, "/srv/a", "/srv/b"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced: %v %v", fi.Mode(), err)
	}
	if got := readTestFile(t, target); got != "root /srv/b;\n" {
		t.Fatalf("target content = %q", got)
	}
	if want, _ := filepath.EvalSymlinks(target); res.Path != want {
		t.Fatalf("result path %q, want the link target %q", res.Path, want)
	}
	if !strings.Contains(strings.Join(res.Preserved, ","), "symlink") {
		t.Fatalf("preserved = %v, want symlink listed", res.Preserved)
	}
}

func TestEditRefusesHardLinkedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	writeTestFile(t, path, "x\n", 0o644)
	if err := os.Link(path, filepath.Join(dir, "b")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(path, "x", "y"))
	if editErrCode(t, err) != "hard_linked" {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, path); got != "x\n" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestEditBaseHashConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "v1\n", 0o644)
	p := editReplace(path, "v1", "v2")
	p.BaseSHA256 = sha256Hex([]byte("something else\n"))
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), p)
	if editErrCode(t, err) != "edit_conflict" || !strings.Contains(err.Error(), sha256Hex([]byte("v1\n"))) {
		t.Fatalf("err = %v", err)
	}
	p.BaseSHA256 = strings.ToUpper(sha256Hex([]byte("v1\n")))
	if _, err := NewFileManager().(fileEditor).Edit(context.Background(), p); err != nil {
		t.Fatalf("matching base hash (any case) should apply: %v", err)
	}
	if got := readTestFile(t, path); got != "v2\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestEditRefusesWhenFileChangesMidEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "a\n", 0o644)
	testHookBeforeEditInstall = func() {
		// Another program rewrites the file in place while the edit runs.
		if err := os.WriteFile(path, []byte("written elsewhere\n"), 0o644); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookBeforeEditInstall = nil }()
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(path, "a", "b"))
	if editErrCode(t, err) != "edit_conflict" {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, path); got != "written elsewhere\n" {
		t.Fatalf("the concurrent write was clobbered: %q", got)
	}
	assertNoTempFiles(t, dir)
}

func TestEditDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "a\n", 0o600)
	before, _ := os.Stat(path)
	p := editReplace(path, "a", "b")
	p.DryRun = true
	res, err := NewFileManager().(fileEditor).Edit(context.Background(), p)
	if err != nil || !res.DryRun || !res.Changed || res.Verified || !strings.Contains(res.Diff, "+b") {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || readTestFile(t, path) != "a\n" {
		t.Fatal("a dry run changed the file")
	}
	assertNoTempFiles(t, dir)
}

func TestEditIsIdempotentByEditID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeTestFile(t, path, "count=1\n", 0o644)
	p := editReplace(path, "count=1", "count=1\ncount=1")
	p.EditID = "retry-test-edit-id"
	first, err := NewFileManager().(fileEditor).Edit(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	// A retry after a lost reply must not apply the edit a second time (which
	// here would fail as ambiguous, or double the lines).
	second, err := NewFileManager().(fileEditor).Edit(context.Background(), p)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.NewSHA256 != first.NewSHA256 || readTestFile(t, path) != "count=1\ncount=1\n" {
		t.Fatalf("retry applied the edit again: %q", readTestFile(t, path))
	}
}

func TestEditReplaceContentChecksAndCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.conf")
	fm := NewFileManager().(fileEditor)
	body := []byte("hello\n")

	bad := proto.FileEditPayload{Path: path, Replace: true, Create: true, Content: body, ContentSHA256: sha256Hex([]byte("other"))}
	if _, err := fm.Edit(context.Background(), bad); editErrCode(t, err) != "checksum_mismatch" {
		t.Fatalf("bad checksum: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a rejected create left a file behind")
	}

	create := proto.FileEditPayload{Path: path, Replace: true, Create: true, Content: body, ContentSHA256: sha256Hex(body), Mode: 0o600}
	res, err := fm.Edit(context.Background(), create)
	if err != nil || !res.Created || !res.Verified {
		t.Fatalf("create: %+v %v", res, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 || readTestFile(t, path) != "hello\n" {
		t.Fatalf("created file mode %v content %q", fi.Mode().Perm(), readTestFile(t, path))
	}
	if _, err := fm.Edit(context.Background(), create); editErrCode(t, err) != "exists" {
		t.Fatalf("second create should refuse to overwrite: %v", err)
	}

	next := []byte("bye\n")
	replace := proto.FileEditPayload{Path: path, Replace: true, Content: next, ContentSHA256: sha256Hex(next), BaseSHA256: sha256Hex(body), ReturnOriginal: true}
	res, err = fm.Edit(context.Background(), replace)
	if err != nil || string(res.Original) != "hello\n" || readTestFile(t, path) != "bye\n" {
		t.Fatalf("replace: %+v %v", res, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("replace changed the mode to %v", fi.Mode().Perm())
	}
	assertNoTempFiles(t, dir)
}

func TestEditRefusesUnsafeTargets(t *testing.T) {
	dir := t.TempDir()
	fm := NewFileManager().(fileEditor)
	if _, err := fm.Edit(context.Background(), editReplace(dir, "a", "b")); editErrCode(t, err) != "not_regular" {
		t.Fatalf("directory: %v", err)
	}
	if _, err := fm.Edit(context.Background(), editReplace(filepath.Join(dir, "missing"), "a", "b")); editErrCode(t, err) != "not_found" {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := fm.Edit(context.Background(), editReplace(dir+"/x/../f", "a", "b")); editErrCode(t, err) != "invalid_path" {
		t.Fatalf("dot components: %v", err)
	}
	if _, err := fm.Edit(context.Background(), editReplace("/proc/self/environ", "a", "b")); editErrCode(t, err) != "invalid_path" {
		t.Fatalf("blocked prefix: %v", err)
	}
	big := filepath.Join(dir, "big")
	writeTestFile(t, big, strings.Repeat("x", 100), 0o644)
	p := editReplace(big, "x", "y")
	p.MaxBytes = 10
	if _, err := fm.Edit(context.Background(), p); editErrCode(t, err) != "file_too_large" {
		t.Fatalf("size limit: %v", err)
	}
}

func TestEditPreservesSetIDBitsAndOwnerAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tool")
	writeTestFile(t, path, "#!/bin/sh\necho a\n", 0o755)
	if err := os.Chown(path, 65534, 65534); err != nil {
		t.Skipf("chown: %v", err)
	}
	if err := os.Chmod(path, 0o755|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode()&os.ModeSetgid == 0 {
		t.Skip("this filesystem does not keep the set-gid bit")
	}
	res, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(path, "echo a", "echo b"))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	st := fi.Sys().(*syscall.Stat_t)
	if st.Uid != 65534 || st.Gid != 65534 {
		t.Fatalf("owner = %d:%d, want 65534:65534", st.Uid, st.Gid)
	}
	if fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm() != 0o755 || res.Mode != 0o2755 {
		t.Fatalf("mode = %v (reported %o), want setgid 0755", fi.Mode(), res.Mode)
	}
}

func TestEditRefusesFileTheAgentCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write any file")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ro")
	writeTestFile(t, path, "a\n", 0o444)
	_, err := NewFileManager().(fileEditor).Edit(context.Background(), editReplace(path, "a", "b"))
	if editErrCode(t, err) != "permission_denied" {
		t.Fatalf("err = %v", err)
	}
	if got := readTestFile(t, path); got != "a\n" {
		t.Fatalf("read-only file was replaced: %q", got)
	}
}
