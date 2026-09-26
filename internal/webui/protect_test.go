// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The controller's config dir (keys, tokens, secrets, audit log) must never
// be listable, readable, downloadable or writable through the Local source —
// directly, via a symlink, or as a copy/move endpoint.
func TestLocalSourceRefusesConfigDir(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	cfg := s.app.ConfigDir
	secret := filepath.Join(cfg, "keys", "planted.txt")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(outside, "cfg-link")
	if err := os.Symlink(cfg, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "ok.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}

	get := func(endpoint string, q url.Values) (int, string) {
		q.Set("t", s.Token())
		res, err := http.Get(ts.URL + endpoint + "?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	for _, target := range []string{cfg, filepath.Join(cfg, "keys"), link, filepath.Join(link, "keys")} {
		if code, body := get("/api/list", url.Values{"path": {target}}); code != http.StatusForbidden || !strings.Contains(body, "configuration directory") {
			t.Fatalf("list %s: %d %s", target, code, body)
		}
	}
	for _, target := range []string{secret, filepath.Join(link, "keys", "planted.txt")} {
		for _, ep := range []string{"/api/read", "/api/download", "/api/checksum"} {
			if code, body := get(ep, url.Values{"path": {target}}); code != http.StatusForbidden {
				t.Fatalf("%s %s: %d %s", ep, target, code, body)
			}
		}
	}

	posts := []struct {
		endpoint string
		params   url.Values
	}{
		{"/api/mkdir", url.Values{"dir": {cfg}, "name": {"x"}}},
		{"/api/touch", url.Values{"dir": {link}, "name": {"x"}}},
		{"/api/write", url.Values{"path": {filepath.Join(cfg, "servers", "evil.toml")}}},
		{"/api/rm", url.Values{"path": {secret}}},
		{"/api/mv", url.Values{"from": {secret}, "name": {"moved.txt"}}},
		{"/api/mv", url.Values{"from": {filepath.Join(outside, "ok.txt")}, "to": {filepath.Join(cfg, "ok.txt")}}},
		{"/api/copy", url.Values{"srcPath": {secret}, "dstPath": {filepath.Join(outside, "stolen.txt")}}},
		{"/api/copy", url.Values{"srcPath": {filepath.Join(outside, "ok.txt")}, "dstPath": {filepath.Join(link, "ok.txt")}}},
		{"/api/move", url.Values{"srcPath": {filepath.Join(outside, "ok.txt")}, "dstPath": {filepath.Join(cfg, "ok.txt")}}},
		{"/api/chmod", url.Values{"path": {secret}, "mode": {"644"}}},
		{"/api/duplicate", url.Values{"path": {secret}}},
		{"/api/extract", url.Values{"path": {filepath.Join(cfg, "a.zip")}}},
		{"/api/upload", url.Values{"dir": {cfg}, "name": {"up.txt"}}},
	}
	for _, p := range posts {
		if code, body := postAPI(t, s, ts.URL, p.endpoint, p.params, ts.URL); code != http.StatusForbidden {
			t.Fatalf("POST %s %v: %d %s", p.endpoint, p.params, code, body)
		}
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "private" {
		t.Fatalf("protected file changed: %q %v", b, err)
	}
	for _, leaked := range []string{filepath.Join(outside, "stolen.txt"), filepath.Join(cfg, "ok.txt"), filepath.Join(cfg, "up.txt"), filepath.Join(cfg, "x")} {
		if _, err := os.Lstat(leaked); err == nil {
			t.Fatalf("%s was created", leaked)
		}
	}
	// Ordinary locations keep working.
	if code, body := get("/api/list", url.Values{"path": {outside}}); code != http.StatusOK {
		t.Fatalf("list outside: %d %s", code, body)
	}
}

func TestLocalGuard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := filepath.Join(root, "cfg")
	if err := os.MkdirAll(filepath.Join(cfg, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	g := newLocalGuard(cfg)
	cases := map[string]bool{
		cfg:                                   true,
		filepath.Join(cfg, "keys", "id"):      true,
		filepath.Join(cfg, "new", "deep.txt"): true,
		root:                                  false,
		cfg + "-sibling":                      false,
		filepath.Join(root, "cfgx", "a"):      false,
	}
	for p, blocked := range cases {
		if got := g.check(p) != nil; got != blocked {
			t.Fatalf("check(%q) blocked=%v, want %v", p, got, blocked)
		}
	}
	if err := (localGuard{}).check(cfg); err != nil {
		t.Fatalf("an empty guard must allow everything: %v", err)
	}
}

func TestInitialLocalRootAvoidsConfigDir(t *testing.T) {
	// Not parallel: it changes the process working directory.
	s, _ := newTestServer(t)
	cfg := s.app.ConfigDir
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(filepath.Join(cfg, "keys")); err != nil {
		t.Skipf("chdir: %v", err)
	}
	got := s.initialLocalRoot()
	if s.localGuard.check(got) != nil {
		t.Fatalf("initial root %q is protected", got)
	}
	resolvedCfg, _ := filepath.EvalSymlinks(cfg)
	if got != filepath.Dir(cfg) && got != filepath.Dir(resolvedCfg) {
		t.Fatalf("initial root = %q, want the config dir's parent", got)
	}
}
