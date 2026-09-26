// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
)

// treeFixture is a test server whose config dir sits in a private temp parent
// next to an ordinary working folder, reproducing the reviewer's setup (the
// Local pane pointed at $HOME, the parent of ~/.cenvero-fleet). Every path it
// creates lives under t.TempDir().
type treeFixture struct {
	s        *Server
	base     string // httptest URL
	cfg      string // the controller config dir
	parent   string // its parent directory
	work     string // an ordinary folder beside the config dir
	sentinel string // a secret inside the config dir
}

const sentinelSecret = "PRIVATE-KEY-MATERIAL"

func newTreeFixture(t *testing.T) *treeFixture {
	t.Helper()
	s, ts := newTestServer(t)
	f := &treeFixture{s: s, base: ts.URL, cfg: s.app.ConfigDir, parent: filepath.Dir(s.app.ConfigDir)}
	f.sentinel = filepath.Join(f.cfg, "keys", "sentinel_ed25519")
	if err := os.MkdirAll(filepath.Dir(f.sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.sentinel, []byte(sentinelSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	f.work = filepath.Join(f.parent, "work")
	if err := os.MkdirAll(filepath.Join(f.work, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.work, "nested", "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.app.SaveServer(core.ServerRecord{Name: "web-01", Address: "127.0.0.1", Port: 1, Mode: transport.ModeDirect}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *treeFixture) post(t *testing.T, endpoint string, params url.Values) (int, string) {
	t.Helper()
	return postAPI(t, f.s, f.base, endpoint, params, f.base)
}

// expectRefused asserts a 403 carrying the guard's message.
func (f *treeFixture) expectRefused(t *testing.T, endpoint string, params url.Values) {
	t.Helper()
	code, body := f.post(t, endpoint, params)
	if code != http.StatusForbidden || !strings.Contains(body, "configuration directory") {
		t.Fatalf("POST %s %v: got %d %s, want 403 from the protected-path guard", endpoint, params, code, body)
	}
}

func (f *treeFixture) expectOK(t *testing.T, endpoint string, params url.Values) string {
	t.Helper()
	code, body := f.post(t, endpoint, params)
	if code != http.StatusOK {
		t.Fatalf("POST %s %v: got %d %s, want 200", endpoint, params, code, body)
	}
	return body
}

// expectIntact asserts the config dir and its secret were neither moved,
// deleted nor changed.
func (f *treeFixture) expectIntact(t *testing.T) {
	t.Helper()
	if b, err := os.ReadFile(f.sentinel); err != nil || string(b) != sentinelSecret {
		t.Fatalf("protected secret changed or gone: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(f.cfg, "servers", "web-01.toml")); err != nil {
		t.Fatalf("server record gone: %v", err)
	}
}

func expectAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s was created", p)
		}
	}
}

// waitTransfer waits for a hub transfer started by /api/copy or /api/move.
func waitTransfer(t *testing.T, s *Server, body string) progressSnapshot {
	t.Helper()
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &started); err != nil || started.ID == "" {
		t.Fatalf("transfer response %q: %v", body, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := s.hub.snapshot(started.ID); ok && snap.Done {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("transfer %s did not finish", started.ID)
	return progressSnapshot{}
}

// writeTarGz writes a tar.gz whose regular-file members are files.
func writeTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, gz, out} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// Reviewer variant 1: archiving the config dir from its parent, then
// downloading the archive.
func TestCompressRefusesProtectedMembers(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	cfgName := filepath.Base(f.cfg)
	for _, format := range []string{"tar.gz", "zip", "tar"} {
		archive := "steal." + format
		f.expectRefused(t, "/api/compress", url.Values{"dir": {f.parent}, "name": {cfgName}, "archive": {archive}, "format": {format}})
		// Every selected name is checked, not just the first.
		f.expectRefused(t, "/api/compress", url.Values{"dir": {f.parent}, "name": {"work", cfgName}, "archive": {archive}, "format": {format}})
		expectAbsent(t, filepath.Join(f.parent, archive))
	}
	// The archive output itself may not land on a protected path.
	f.expectRefused(t, "/api/compress", url.Values{"dir": {filepath.Join(f.cfg, "keys")}, "name": {"x"}, "archive": {"a.zip"}, "format": {"zip"}})
	f.expectIntact(t)
}

// zip follows symlinks while archiving, so a symlink to the config dir inside a
// selected folder must not smuggle it into the archive.
func TestCompressZipRefusesSymlinkIntoProtectedTree(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	link := filepath.Join(f.work, "nested", "cfg-link")
	if err := os.Symlink(f.cfg, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	f.expectRefused(t, "/api/compress", url.Values{"dir": {f.parent}, "name": {"work"}, "archive": {"w.zip"}, "format": {"zip"}})
	// A link to the parent (which contains the config dir) is refused too.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.parent, link); err != nil {
		t.Fatal(err)
	}
	f.expectRefused(t, "/api/compress", url.Values{"dir": {f.parent}, "name": {"work"}, "archive": {"w.zip"}, "format": {"zip"}})
	expectAbsent(t, filepath.Join(f.parent, "w.zip"))
	f.expectIntact(t)
}

// Reviewer variants 2 and 3 plus rm/duplicate: whole-tree copy, move, rename,
// delete and duplicate of an ancestor of the config dir.
func TestTreeOperationsRefuseAncestorsOfProtectedRoots(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	out := t.TempDir()
	// A folder shaped like the config dir, to be merged over the parent.
	evil := filepath.Join(out, "evil")
	plantedSrc := filepath.Join(evil, filepath.Base(f.cfg), "keys", "planted.txt")
	if err := os.MkdirAll(filepath.Dir(plantedSrc), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plantedSrc, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, recursive := range []string{"1", ""} {
		f.expectRefused(t, "/api/copy", url.Values{"srcPath": {f.parent}, "dstPath": {filepath.Join(out, "copy")}, "recursive": {recursive}})
		f.expectRefused(t, "/api/move", url.Values{"srcPath": {f.parent}, "dstPath": {filepath.Join(out, "moved")}, "recursive": {recursive}})
		// Merging a tree into the parent could plant files in the config dir.
		f.expectRefused(t, "/api/copy", url.Values{"srcPath": {evil}, "dstPath": {f.parent}, "recursive": {recursive}})
		// Local → server upload of the parent, and server → local into it.
		f.expectRefused(t, "/api/copy", url.Values{"srcPath": {f.parent}, "dstServer": {"web-01"}, "dstPath": {"/srv/stolen"}, "recursive": {recursive}})
		f.expectRefused(t, "/api/move", url.Values{"srcPath": {f.parent}, "dstServer": {"web-01"}, "dstPath": {"/srv/stolen"}, "recursive": {recursive}})
		f.expectRefused(t, "/api/copy", url.Values{"srcServer": {"web-01"}, "srcPath": {"/srv/evil"}, "dstPath": {f.parent}, "recursive": {recursive}})
	}
	f.expectRefused(t, "/api/mv", url.Values{"from": {f.parent}, "name": {"renamed"}})
	f.expectRefused(t, "/api/mv", url.Values{"from": {f.parent}, "to": {filepath.Join(out, "renamed")}})
	// Renaming something ONTO the config dir's name.
	f.expectRefused(t, "/api/mv", url.Values{"from": {f.work}, "to": {f.cfg}})
	f.expectRefused(t, "/api/rm", url.Values{"path": {f.parent}, "recursive": {"true"}})
	f.expectRefused(t, "/api/rm", url.Values{"path": {f.parent}})
	f.expectRefused(t, "/api/duplicate", url.Values{"path": {f.parent}})

	expectAbsent(t, filepath.Join(out, "copy"), filepath.Join(out, "moved"), filepath.Join(out, "renamed"),
		filepath.Join(f.cfg, "keys", "planted.txt"), filepath.Join(filepath.Dir(f.parent), filepath.Base(f.parent)+" copy"))
	f.expectIntact(t)
}

// Reviewer variant 4: an archive in the config dir's parent must not be able
// to plant files inside it, whatever its format.
func TestExtractCannotPlantIntoProtectedRoot(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	member := filepath.Base(f.cfg) + "/keys/planted.txt"
	server := filepath.Base(f.cfg) + "/servers/evil.toml"
	tgz := filepath.Join(f.parent, "evil.tar.gz")
	writeTarGz(t, tgz, map[string]string{"harmless.txt": "x", member: "planted", server: "name = 'evil'"})
	zp := filepath.Join(f.parent, "evil.zip")
	writeZip(t, zp, map[string]string{"harmless.txt": "x", member: "planted"})
	for _, archive := range []string{tgz, zp} {
		f.expectRefused(t, "/api/extract", url.Values{"path": {archive}})
	}
	// Nothing at all is merged when any member is refused.
	expectAbsent(t, filepath.Join(f.cfg, "keys", "planted.txt"), filepath.Join(f.cfg, "servers", "evil.toml"), filepath.Join(f.parent, "harmless.txt"))
	expectNoStaging(t, f.cfg)
	f.expectIntact(t)
}

func expectNoStaging(t *testing.T, cfg string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(cfg, "tmp", "webui-extract-*"))
	if len(left) != 0 {
		t.Fatalf("staging directories left behind: %v", left)
	}
}

// Reviewer variant 5: key and known-hosts files configured OUTSIDE the config
// dir are protected exactly like it, and single-entry creates check the joined
// dir/name path rather than only the directory.
func TestKeyAndKnownHostsOutsideConfigDirAreProtected(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	sshDir := filepath.Join(f.parent, "ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(sshDir, "id_fleet")
	if err := os.WriteFile(key, []byte(sentinelSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte("Host *"), 0o600); err != nil {
		t.Fatal(err)
	}
	khDir := filepath.Join(f.parent, "hosts")
	if err := os.MkdirAll(khDir, 0o700); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(khDir, "fleet_known_hosts") // does not exist yet
	if err := f.s.app.SaveServer(core.ServerRecord{Name: "web-02", Address: "127.0.0.1", Port: 1, Mode: transport.ModeDirect, KeyPath: key}); err != nil {
		t.Fatal(err)
	}
	f.s.app.Config.Crypto.KnownHostsPath = kh // before the first guarded request

	get := func(endpoint string, q url.Values) int {
		t.Helper()
		q.Set("t", f.s.Token())
		res, err := http.Get(f.base + endpoint + "?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	for _, ep := range []string{"/api/read", "/api/download", "/api/checksum"} {
		if code := get(ep, url.Values{"path": {key}}); code != http.StatusForbidden {
			t.Fatalf("GET %s of the server key: %d, want 403", ep, code)
		}
	}
	if code := get("/api/preview", url.Values{"path": {key}, "kind": {"text"}}); code != http.StatusForbidden {
		t.Fatalf("preview of the server key: %d, want 403", code)
	}
	// The folder holding the key can still be browsed; only the key is off-limits.
	if code := get("/api/list", url.Values{"path": {sshDir}}); code != http.StatusOK {
		t.Fatalf("list of the key's folder: %d, want 200", code)
	}
	if code := get("/api/read", url.Values{"path": {filepath.Join(sshDir, "config")}}); code != http.StatusOK {
		t.Fatalf("read of a neighbouring file: %d, want 200", code)
	}

	f.expectRefused(t, "/api/copy", url.Values{"srcPath": {key}, "dstPath": {filepath.Join(f.work, "stolen")}})
	f.expectRefused(t, "/api/copy", url.Values{"srcPath": {sshDir}, "dstPath": {filepath.Join(f.work, "ssh-copy")}, "recursive": {"1"}})
	f.expectRefused(t, "/api/compress", url.Values{"dir": {f.parent}, "name": {"ssh"}, "archive": {"ssh.tar.gz"}, "format": {"tar.gz"}})
	f.expectRefused(t, "/api/rm", url.Values{"path": {sshDir}, "recursive": {"true"}})
	f.expectRefused(t, "/api/mv", url.Values{"from": {key}, "name": {"id_other"}})
	f.expectRefused(t, "/api/copy", url.Values{"srcServer": {"web-01"}, "srcPath": {"/srv/id_fleet"}, "dstPath": {sshDir}})
	// Known-hosts: planting it through any create path is refused, including
	// the name-only endpoints that used to check only `dir`.
	f.expectRefused(t, "/api/touch", url.Values{"dir": {khDir}, "name": {filepath.Base(kh)}})
	f.expectRefused(t, "/api/mkdir", url.Values{"dir": {khDir}, "name": {filepath.Base(kh)}})
	f.expectRefused(t, "/api/upload", url.Values{"dir": {khDir}, "name": {filepath.Base(kh)}})
	f.expectRefused(t, "/api/write", url.Values{"path": {kh}})
	f.expectRefused(t, "/api/mv", url.Values{"from": {filepath.Join(sshDir, "config")}, "to": {kh}})
	f.expectRefused(t, "/api/mkdir", url.Values{"dir": {f.parent}, "name": {filepath.Base(f.cfg)}})
	expectAbsent(t, kh, filepath.Join(f.work, "stolen"), filepath.Join(f.work, "ssh-copy"), filepath.Join(f.parent, "ssh.tar.gz"))
	if b, err := os.ReadFile(key); err != nil || string(b) != sentinelSecret {
		t.Fatalf("server key changed: %q %v", b, err)
	}

	// A hard link to the key is the same file: refused by identity even though
	// no string comparison could tell.
	hard := filepath.Join(f.work, "innocent.txt")
	if err := os.Link(key, hard); err == nil {
		if code := get("/api/read", url.Values{"path": {hard}}); code != http.StatusForbidden {
			t.Fatalf("read of a hard link to the key: %d, want 403", code)
		}
	}
}

// Ordinary work right next to the config dir keeps working: archiving,
// extracting, copying, renaming and deleting sibling folders.
func TestTreeOperationsBesideConfigDirStillWork(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	formats := []string{"tar.gz", "zip"}
	for _, tool := range []string{"tar", "zip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed: %v", tool, err)
		}
	}
	for _, format := range formats {
		f.expectOK(t, "/api/compress", url.Values{"dir": {f.parent}, "name": {"work"}, "archive": {"work." + format}, "format": {format}})
		if _, err := os.Stat(filepath.Join(f.parent, "work."+format)); err != nil {
			t.Fatalf("%s archive not created: %v", format, err)
		}
	}
	// Extracting straight into the parent works (via the private staging path)
	// and lands exactly where a direct extraction would.
	tgz := filepath.Join(f.parent, "fresh.tar.gz")
	writeTarGz(t, tgz, map[string]string{"fresh/a.txt": "alpha", "top.txt": "top"})
	f.expectOK(t, "/api/extract", url.Values{"path": {tgz}})
	zp := filepath.Join(f.parent, "fresh.zip")
	writeZip(t, zp, map[string]string{"zipped/b.txt": "beta"})
	f.expectOK(t, "/api/extract", url.Values{"path": {zp}})
	for p, want := range map[string]string{"fresh/a.txt": "alpha", "top.txt": "top", "zipped/b.txt": "beta"} {
		if b, err := os.ReadFile(filepath.Join(f.parent, p)); err != nil || string(b) != want {
			t.Fatalf("extracted %s = %q %v", p, b, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(f.parent, ".fleet-extract-*"))
	if len(matches) != 0 {
		t.Fatalf("staged archive copy leaked into the destination: %v", matches)
	}
	expectNoStaging(t, f.cfg)

	snap := waitTransfer(t, f.s, f.expectOK(t, "/api/copy", url.Values{"srcPath": {f.work}, "dstPath": {filepath.Join(f.parent, "work-copy")}, "recursive": {"1"}}))
	if snap.Error != "" {
		t.Fatalf("copy failed: %s", snap.Error)
	}
	if b, err := os.ReadFile(filepath.Join(f.parent, "work-copy", "nested", "note.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("copied file = %q %v", b, err)
	}
	f.expectOK(t, "/api/mv", url.Values{"from": {filepath.Join(f.parent, "work-copy")}, "name": {"work-renamed"}})
	f.expectOK(t, "/api/duplicate", url.Values{"path": {filepath.Join(f.parent, "work-renamed")}})
	f.expectOK(t, "/api/rm", url.Values{"path": {filepath.Join(f.parent, "work-renamed")}, "recursive": {"true"}})
	f.expectOK(t, "/api/mkdir", url.Values{"dir": {f.parent}, "name": {"new-folder"}})
	f.expectOK(t, "/api/touch", url.Values{"dir": {f.parent}, "name": {"new.txt"}})
	f.expectOK(t, "/api/upload", url.Values{"dir": {f.parent}, "name": {"uploaded.txt"}})
	f.expectIntact(t)
}

func TestLocalGuardTreeSemantics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := filepath.Join(root, "home", ".fleet")
	if err := os.MkdirAll(filepath.Join(cfg, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(root, "ssh")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(keyDir, "id")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "later", "known_hosts") // neither exists yet
	g := newLocalGuard(cfg, key, missing)

	cases := []struct {
		path      string
		check     error
		checkTree error
	}{
		{cfg, errProtectedPath, errProtectedPath},
		{filepath.Join(cfg, "keys", "new"), errProtectedPath, errProtectedPath},
		{filepath.Join(root, "home"), nil, errContainsProtected},
		{root, nil, errContainsProtected},
		{filepath.Join(root, "home", "other"), nil, nil},
		{cfg + "-sibling", nil, nil},
		{key, errProtectedPath, errProtectedPath},
		{keyDir, nil, errContainsProtected},
		{filepath.Join(keyDir, "id.pub"), nil, nil},
		{missing, errProtectedPath, errProtectedPath},
		{filepath.Join(root, "later"), nil, errContainsProtected},
		{filepath.Join(root, "later2"), nil, nil},
	}
	for _, c := range cases {
		if err := g.check(c.path); !errors.Is(err, c.check) && (err != nil || c.check != nil) {
			t.Fatalf("check(%q) = %v, want %v", c.path, err, c.check)
		}
		if err := g.checkTree(c.path); !errors.Is(err, c.checkTree) && (err != nil || c.checkTree != nil) {
			t.Fatalf("checkTree(%q) = %v, want %v", c.path, err, c.checkTree)
		}
	}

	// Symlinks are judged by where they lead: a link to an ancestor of a
	// protected root contains it, a link into one is inside it.
	other := t.TempDir()
	up := filepath.Join(other, "up")
	in := filepath.Join(other, "in")
	if err := os.Symlink(filepath.Join(root, "home"), up); err == nil {
		if err := os.Symlink(filepath.Join(cfg, "keys"), in); err != nil {
			t.Fatal(err)
		}
		if err := g.checkTree(up); !errors.Is(err, errContainsProtected) {
			t.Fatalf("checkTree(link to ancestor) = %v", err)
		}
		if err := g.check(filepath.Join(up, ".fleet", "keys", "id")); !errors.Is(err, errProtectedPath) {
			t.Fatalf("check(through link to ancestor) = %v", err)
		}
		if err := g.check(filepath.Join(in, "new")); !errors.Is(err, errProtectedPath) {
			t.Fatalf("check(through link into root) = %v", err)
		}
	}
}

// On filesystems that treat differently-spelled names as the same entry
// (macOS: Unicode NFC vs NFD, case-insensitive volumes), every spelling the
// filesystem accepts is refused. Skipped where the filesystem is byte-exact.
func TestLocalGuardEquivalentSpellings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	nfc := filepath.Join(root, "café", ".Fleet")
	if err := os.MkdirAll(filepath.Join(nfc, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	g := newLocalGuard(nfc)
	variants := map[string]string{
		"nfd":   filepath.Join(root, "café", ".Fleet"),
		"upper": filepath.Join(root, "CAFÉ", ".FLEET"),
	}
	tested := 0
	for name, v := range variants {
		if _, err := os.Stat(v); err != nil {
			continue // the filesystem distinguishes this spelling: nothing to protect
		}
		tested++
		if err := g.check(filepath.Join(v, "keys", "x")); !errors.Is(err, errProtectedPath) {
			t.Fatalf("%s spelling inside the root: %v", name, err)
		}
		if err := g.checkTree(filepath.Dir(v)); !errors.Is(err, errContainsProtected) {
			t.Fatalf("%s spelling of the parent: %v", name, err)
		}
	}
	if tested == 0 {
		t.Skip("filesystem distinguishes NFC/NFD and case; identity checks not exercised")
	}
}
