// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := core.Initialize(core.InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	app, err := core.Open(configDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	s, err := New(app)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestWebUIRequiresToken(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)

	res, err := http.Get(ts.URL + "/api/servers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", res.StatusCode)
	}

	res2, err := http.Get(ts.URL + "/api/servers?t=" + s.Token())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res2.Body)
		t.Fatalf("expected 200 with token, got %d: %s", res2.StatusCode, body)
	}
}

func TestWebUIRejectsBadToken(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/servers?t=deadbeef")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with bad token, got %d", res.StatusCode)
	}
}

func TestWebUIServesIndex(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for index, got %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) == 0 {
		t.Fatalf("index body empty")
	}
}

func TestCleanLocalPath(t *testing.T) {
	t.Parallel()
	if got, err := cleanLocalPath(""); err != nil || got != "/" {
		t.Fatalf("empty path: got %q err %v, want /", got, err)
	}
	if got, err := cleanLocalPath("/a/b/../c"); err != nil || got != "/a/c" {
		t.Fatalf("clean: got %q err %v, want /a/c", got, err)
	}
	if _, err := cleanLocalPath("relative/path"); err == nil {
		t.Fatalf("expected relative path to be rejected")
	}
}

func TestListLocalDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".dotfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := listLocalDir(dir, false)
	if err != nil {
		t.Fatalf("listLocalDir: %v", err)
	}
	if res.Path != filepath.Clean(dir) {
		t.Fatalf("path: got %q want %q", res.Path, filepath.Clean(dir))
	}
	names := entryNames(res.Entries)
	if names[".dotfile"] {
		t.Fatalf("dotfile should be hidden without hidden=1")
	}
	if !names["visible.txt"] || !names["sub"] {
		t.Fatalf("missing entries: %v", names)
	}
	for _, e := range res.Entries {
		if e.Name == "sub" && !e.IsDir {
			t.Fatalf("sub should be a directory")
		}
	}

	resH, err := listLocalDir(dir, true)
	if err != nil {
		t.Fatalf("listLocalDir hidden: %v", err)
	}
	if !entryNames(resH.Entries)[".dotfile"] {
		t.Fatalf("dotfile should appear with showHidden")
	}
}

func entryNames(ents []proto.FileEntry) map[string]bool {
	m := make(map[string]bool, len(ents))
	for _, e := range ents {
		m[e.Name] = true
	}
	return m
}

// TestWebUILocalListing exercises the /api/list endpoint with an empty server
// (Local source) end-to-end through the guard.
func TestWebUILocalListing(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	u := ts.URL + "/api/list?t=" + s.Token() + "&path=" + url.QueryEscape(dir)
	res, err := http.Get(u)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("expected 200, got %d: %s", res.StatusCode, body)
	}
	var out proto.FileListResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !entryNames(out.Entries)["a.txt"] {
		t.Fatalf("expected a.txt in local listing, got %+v", out.Entries)
	}
}

// TestWebUILocalMutations exercises mkdir/mv/rm against the Local source via the
// POST-gated endpoints, confirming local routing works behind the guard.
func TestWebUILocalMutations(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	client := ts.Client()

	post := func(p string, params url.Values) int {
		params.Set("t", s.Token())
		req, _ := http.NewRequest(http.MethodPost, ts.URL+p+"?"+params.Encode(), nil)
		req.Header.Set("Origin", ts.URL)
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", p, err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	newDir := filepath.Join(dir, "made")
	if code := post("/api/mkdir", url.Values{"path": {newDir}}); code != http.StatusOK {
		t.Fatalf("mkdir status %d", code)
	}
	if fi, err := os.Stat(newDir); err != nil || !fi.IsDir() {
		t.Fatalf("mkdir did not create dir: %v", err)
	}

	renamed := filepath.Join(dir, "renamed")
	if code := post("/api/mv", url.Values{"from": {newDir}, "to": {renamed}}); code != http.StatusOK {
		t.Fatalf("mv status %d", code)
	}
	if _, err := os.Stat(renamed); err != nil {
		t.Fatalf("rename target missing: %v", err)
	}

	if code := post("/api/rm", url.Values{"path": {renamed}, "recursive": {"true"}}); code != http.StatusOK {
		t.Fatalf("rm status %d", code)
	}
	if _, err := os.Stat(renamed); !os.IsNotExist(err) {
		t.Fatalf("rm did not remove dir: %v", err)
	}
}

func TestTransferLocalToLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	// copy
	cp := filepath.Join(dir, "copy.txt")
	if err := transferLocalToLocal(src, cp, false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if b, _ := os.ReadFile(cp); string(b) != "payload" {
		t.Fatalf("copy content mismatch: %q", b)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("copy must not remove source: %v", err)
	}

	// move (rename)
	mv := filepath.Join(dir, "moved.txt")
	if err := transferLocalToLocal(src, mv, true); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("move must remove source")
	}

	// recursive tree copy
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "nested", "f.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	dstTree := filepath.Join(dir, "tree-copy")
	info, _ := os.Lstat(tree)
	if !info.IsDir() {
		t.Fatal("tree should be dir")
	}
	if err := copyLocalTree(tree, dstTree); err != nil {
		t.Fatalf("copyLocalTree: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dstTree, "nested", "f.txt")); string(b) != "deep" {
		t.Fatalf("tree copy content mismatch: %q", b)
	}
}

func TestListLocalDirRejectsRelative(t *testing.T) {
	t.Parallel()
	if _, err := listLocalDir("not/absolute", false); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute-path error, got %v", err)
	}
}

// TestWebUIReadWrite exercises the editor endpoints against the Local source:
// touch creates an empty file, write saves content, read returns it.
func TestWebUIReadWrite(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	client := ts.Client()

	post := func(p string, params url.Values, body string) (int, string) {
		params.Set("t", s.Token())
		req, _ := http.NewRequest(http.MethodPost, ts.URL+p+"?"+params.Encode(), strings.NewReader(body))
		req.Header.Set("Origin", ts.URL)
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", p, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	file := filepath.Join(dir, "note.txt")
	if code, body := post("/api/touch", url.Values{"path": {file}}, ""); code != http.StatusOK {
		t.Fatalf("touch status %d: %s", code, body)
	}
	if fi, err := os.Stat(file); err != nil || fi.Size() != 0 {
		t.Fatalf("touch did not create empty file: %v", err)
	}

	want := "hello\nworld\n"
	if code, body := post("/api/write", url.Values{"path": {file}}, want); code != http.StatusOK {
		t.Fatalf("write status %d: %s", code, body)
	}
	if b, _ := os.ReadFile(file); string(b) != want {
		t.Fatalf("write content mismatch: %q", b)
	}

	u := ts.URL + "/api/read?t=" + s.Token() + "&path=" + url.QueryEscape(file)
	res, err := http.Get(u)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("read status %d: %s", res.StatusCode, body)
	}
	var out struct {
		Content string `json:"content"`
		Size    int    `json:"size"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if out.Content != want {
		t.Fatalf("read content mismatch: %q", out.Content)
	}
}

// TestWebUIReadRejectsBinary confirms a file with NUL bytes is refused.
func TestWebUIReadRejectsBinary(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(bin, []byte{0x00, 0x01, 0x02, 0x03}, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(ts.URL + "/api/read?t=" + s.Token() + "&path=" + url.QueryEscape(bin))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatalf("expected binary file to be rejected, got 200")
	}
}

func TestLooksBinary(t *testing.T) {
	t.Parallel()
	if looksBinary([]byte("plain text\nwith newlines\t and tabs")) {
		t.Fatalf("plain text classified as binary")
	}
	if !looksBinary([]byte("has a \x00 nul")) {
		t.Fatalf("NUL byte should be binary")
	}
	if looksBinary([]byte("")) {
		t.Fatalf("empty should not be binary")
	}
}

// TestWebUIChecksum confirms /api/checksum returns the SHA-256 of a local file.
func TestWebUIChecksum(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// sha256("hello")
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

	res, err := http.Get(ts.URL + "/api/checksum?t=" + s.Token() + "&path=" + url.QueryEscape(file))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("checksum status %d: %s", res.StatusCode, body)
	}
	var out struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SHA256 != want {
		t.Fatalf("checksum mismatch: got %q want %q", out.SHA256, want)
	}
}

// TestWebUIChmodAndDuplicate exercises /api/chmod and /api/duplicate against the
// Local source via the POST-gated endpoints.
func TestWebUIChmodAndDuplicate(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	client := ts.Client()

	post := func(p string, params url.Values) (int, string) {
		params.Set("t", s.Token())
		req, _ := http.NewRequest(http.MethodPost, ts.URL+p+"?"+params.Encode(), nil)
		req.Header.Set("Origin", ts.URL)
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", p, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, body := post("/api/chmod", url.Values{"path": {file}, "mode": {"600"}}); code != http.StatusOK {
		t.Fatalf("chmod status %d: %s", code, body)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("chmod did not apply 0600: mode=%v err=%v", fi.Mode().Perm(), err)
	}

	if code, body := post("/api/duplicate", url.Values{"path": {file}}); code != http.StatusOK {
		t.Fatalf("duplicate status %d: %s", code, body)
	}
	dup := filepath.Join(dir, "note copy.txt")
	if b, err := os.ReadFile(dup); err != nil || string(b) != "payload" {
		t.Fatalf("duplicate did not create sibling copy: %q err=%v", b, err)
	}
}

// TestWebUICompressExtract round-trips /api/compress then /api/extract against the
// Local source, confirming the archive is created and unpacks back to the file.
func TestWebUICompressExtract(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	client := ts.Client()

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Compress sends repeated `name` params in the form body; the token rides the
	// header (as the frontend's postForm does), not the body.
	form := url.Values{"server": {""}, "dir": {dir}, "archive": {"bundle.tar.gz"}, "format": {"tar.gz"}, "name": {"a.txt"}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/compress", strings.NewReader(form.Encode()))
	req.Header.Set("X-Fleet-Token", s.Token())
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("compress status %d: %s", res.StatusCode, body)
	}
	archive := filepath.Join(dir, "bundle.tar.gz")
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("archive not created: %v", err)
	}

	// Remove the original, then extract and confirm it reappears.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	ep := url.Values{"path": {archive}}
	ep.Set("t", s.Token())
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/extract?"+ep.Encode(), nil)
	req2.Header.Set("Origin", ts.URL)
	res2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	b2, _ := io.ReadAll(res2.Body)
	res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("extract status %d: %s", res2.StatusCode, b2)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(b) != "alpha" {
		t.Fatalf("extract did not restore file: %q err=%v", b, err)
	}
}

// TestDuplicateName checks the "<name> copy.<ext>" derivation.
func TestDuplicateName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/a/b/note.txt": "/a/b/note copy.txt",
		"/a/b/folder":   "/a/b/folder copy",
		"/x/archive.gz": "/x/archive copy.gz",
	}
	for in, want := range cases {
		if got := duplicateName(in); got != want {
			t.Fatalf("duplicateName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureLoopbackAddr(t *testing.T) {
	t.Parallel()
	ok := []string{"127.0.0.1:9445", "localhost:8080", "[::1]:9445"}
	for _, a := range ok {
		if err := ensureLoopbackAddr(a); err != nil {
			t.Fatalf("expected %q to be allowed: %v", a, err)
		}
	}
	bad := []string{"0.0.0.0:9445", "192.168.1.5:9445", "example.com:80"}
	for _, a := range bad {
		if err := ensureLoopbackAddr(a); err == nil {
			t.Fatalf("expected %q to be rejected", a)
		}
	}
}
