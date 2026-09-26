// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateLocalMutationPath(t *testing.T) {
	t.Parallel()
	sep := string(filepath.Separator)
	for _, p := range []string{sep + "x" + sep + "a" + sep + "..", "/x/a/..", "/x/./a", "/x/../y", "/.", "/..", "..", "."} {
		if err := validateLocalMutationPath(p); !errors.Is(err, errDotComponent) {
			t.Fatalf("validateLocalMutationPath(%q) = %v, want errDotComponent", p, err)
		}
	}
	for _, p := range []string{"", "  "} {
		if err := validateLocalMutationPath(p); !errors.Is(err, errEmptyMutationPath) {
			t.Fatalf("validateLocalMutationPath(%q) = %v, want errEmptyMutationPath", p, err)
		}
	}
	for _, p := range []string{"/x/a", "/x/..a/b", "/x/a../b", "/x/.hidden", "/x/.../b", "/x/a/"} {
		if err := validateLocalMutationPath(p); err != nil {
			t.Fatalf("validateLocalMutationPath(%q) = %v, want nil", p, err)
		}
	}
}

// QA B2: a raw Local path such as ".../webrm/a/.." used to be cleaned to
// ".../webrm" before anything checked it, so rm deleted the parent. Every Local
// verb that creates, replaces, moves or removes something must refuse "." and
// ".." components (and an empty path) with a 400 and leave the tree untouched.
func TestLocalMutationsRefuseDotComponents(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	base := t.TempDir()
	victim := filepath.Join(base, "webrm")
	if err := os.MkdirAll(filepath.Join(victim, "a"), 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(victim, "arch.tar.gz")
	writeTarGz(t, archive, map[string]string{"extracted.txt": "x"})

	sep := string(filepath.Separator)
	up := victim + sep + "a" + sep + ".."    // cleans to victim
	dot := victim + sep + "." + sep + "a"    // cleans to victim/a
	otherUp := base + sep + "x" + sep + ".." // cleans to base

	cases := []struct {
		endpoint string
		params   url.Values
	}{
		{"/api/rm", url.Values{"path": {up}, "recursive": {"true"}}},
		{"/api/rm", url.Values{"path": {up}}},
		{"/api/rm", url.Values{"path": {""}, "recursive": {"true"}}},
		{"/api/mv", url.Values{"from": {up}, "name": {"renamed"}}},
		{"/api/mv", url.Values{"from": {up}, "to": {filepath.Join(base, "moved")}}},
		{"/api/mv", url.Values{"from": {sentinel}, "to": {dot + sep + "moved.txt"}}},
		{"/api/mkdir", url.Values{"dir": {up}, "name": {"made"}}},
		{"/api/touch", url.Values{"dir": {dot}, "name": {"touched.txt"}}},
		{"/api/upload", url.Values{"dir": {up}, "name": {"uploaded.txt"}}},
		{"/api/write", url.Values{"path": {dot + sep + ".." + sep + "keep.txt"}}},
		{"/api/duplicate", url.Values{"path": {up}}},
		{"/api/extract", url.Values{"path": {victim + sep + "a" + sep + ".." + sep + "arch.tar.gz"}}},
		{"/api/compress", url.Values{"dir": {up}, "name": {"keep.txt"}, "archive": {"out.tar.gz"}, "format": {"tar.gz"}}},
		{"/api/chmod", url.Values{"path": {up}, "mode": {"700"}}},
		{"/api/copy", url.Values{"srcPath": {up}, "dstPath": {filepath.Join(base, "copied")}, "recursive": {"1"}}},
		{"/api/copy", url.Values{"srcPath": {sentinel}, "dstPath": {otherUp + sep + "copied.txt"}}},
		{"/api/move", url.Values{"srcPath": {up}, "dstPath": {filepath.Join(base, "moved2")}, "recursive": {"1"}}},
		{"/api/move", url.Values{"srcPath": {sentinel}, "dstPath": {otherUp + sep + "moved.txt"}}},
	}
	for _, c := range cases {
		code, body := postAPI(t, s, ts.URL, c.endpoint, c.params, ts.URL)
		if code != http.StatusBadRequest || !(strings.Contains(body, "components are not allowed") || strings.Contains(body, "a path is required")) {
			t.Fatalf("POST %s %v: got %d %s, want 400 refusing the raw path", c.endpoint, c.params, code, body)
		}
	}

	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep" {
		t.Fatalf("victim tree changed: %q %v", b, err)
	}
	if info, err := os.Stat(filepath.Join(victim, "a")); err != nil || !info.IsDir() || info.Mode().Perm() == 0o700 {
		t.Fatalf("victim/a changed: %v %v", info, err)
	}
	expectAbsent(t,
		filepath.Join(base, "renamed"), filepath.Join(base, "moved"), filepath.Join(base, "moved2"),
		filepath.Join(base, "copied"), filepath.Join(base, "copied.txt"), filepath.Join(base, "moved.txt"),
		filepath.Join(victim, "a", "moved.txt"), filepath.Join(victim, "made"), filepath.Join(victim, "a", "touched.txt"),
		filepath.Join(victim, "uploaded.txt"), filepath.Join(victim, "extracted.txt"), filepath.Join(victim, "out.tar.gz"),
		filepath.Join(base, "webrm copy"))

	// Browsing keeps cleaning: listing ".../webrm/a/.." still shows webrm.
	q := url.Values{"path": {up}, "t": {s.Token()}}
	res, err := http.Get(ts.URL + "/api/list?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(b), "keep.txt") {
		t.Fatalf("list of a dotted path: %d %s", res.StatusCode, b)
	}
}
