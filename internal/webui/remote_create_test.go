// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/agent"
	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/testutil"
	"github.com/cenvero/fleet/internal/transport"
)

// newRemoteTestServer is newTestServer with a real (in-memory) fleet-agent
// registered as server "web", serving this machine's filesystem.
func newRemoteTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, ts := newTestServer(t)
	pub, err := os.ReadFile(filepath.Join(s.app.ConfigDir, "keys", "id_ed25519.pub"))
	if err != nil {
		t.Fatal(err)
	}
	authorized := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(authorized, pub, 0o600); err != nil {
		t.Fatal(err)
	}
	agentSrv := agent.Server{
		Mode:               transport.ModeDirect,
		HostKeyPath:        filepath.Join(t.TempDir(), "agent_host_key"),
		AuthorizedKeysPath: authorized,
	}
	s.app.NetworkDialContext = func(context.Context, string, string) (net.Conn, error) {
		client, server := testutil.NewBufferedConnPair("127.0.0.1:40200", "127.0.0.1:2222")
		go func() { _ = agentSrv.ServeConn(server) }()
		return client, nil
	}
	if err := s.app.AddServer(core.ServerRecord{
		Name: "web", Address: "127.0.0.1", Port: 2222, Mode: transport.ModeDirect, User: "cenvero-agent",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.app.DisconnectPooledSessions)
	return s, ts
}

// remoteDir is a temp dir as the agent reports it (symlinks resolved).
func remoteDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// "New file" on a server must never empty an existing file, even a hidden one
// the browser is not showing: the agent's upload replaces whatever is there.
func TestRemoteNewFileNeverOverwrites(t *testing.T) {
	s, ts := newRemoteTestServer(t)
	dir := remoteDir(t)
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, body := postAPI(t, s, ts.URL, "/api/touch", url.Values{"server": {"web"}, "dir": {dir}, "name": {".env"}}, ts.URL)
	if code != http.StatusConflict || !strings.Contains(body, "already exists") {
		t.Fatalf("touch over an existing hidden file: %d %s, want 409", code, body)
	}
	if got, _ := os.ReadFile(env); string(got) != "SECRET=1\n" {
		t.Fatalf(".env was changed: %q", got)
	}

	code, body = postAPI(t, s, ts.URL, "/api/touch", url.Values{"server": {"web"}, "dir": {dir}, "name": {"new.txt"}}, ts.URL)
	if code != http.StatusOK {
		t.Fatalf("touch a new file: %d %s", code, body)
	}
	if info, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil || info.Size() != 0 {
		t.Fatalf("new.txt not created empty: %v %v", info, err)
	}
}

// Rename and same-pane move never replace an existing file, locally or on a
// server; renaming to a new name still works.
func TestMoveNeverReplacesAnExistingFile(t *testing.T) {
	s, ts := newRemoteTestServer(t)
	for _, server := range []string{"", "web"} {
		dir := remoteDir(t)
		a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
		_ = os.WriteFile(a, []byte("A"), 0o600)
		_ = os.WriteFile(b, []byte("B"), 0o600)
		for name, params := range map[string]url.Values{
			"rename": {"server": {server}, "from": {a}, "name": {"b.txt"}},
			"move":   {"server": {server}, "from": {a}, "to": {b}},
		} {
			code, body := postAPI(t, s, ts.URL, "/api/mv", params, ts.URL)
			if code != http.StatusConflict {
				t.Fatalf("server %q %s onto an existing file: %d %s, want 409", server, name, code, body)
			}
			if got, _ := os.ReadFile(b); string(got) != "B" {
				t.Fatalf("server %q %s replaced b.txt: %q", server, name, got)
			}
		}
		code, body := postAPI(t, s, ts.URL, "/api/mv", url.Values{"server": {server}, "from": {a}, "name": {"c.txt"}}, ts.URL)
		if code != http.StatusOK {
			t.Fatalf("server %q rename to a free name: %d %s", server, code, body)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "c.txt")); string(got) != "A" {
			t.Fatalf("server %q: c.txt = %q", server, got)
		}
	}
}

// An upload named like an existing folder is refused instead of landing inside
// that folder under the spool file's temporary name.
func TestRemoteUploadOntoFolderIsRefused(t *testing.T) {
	s, ts := newRemoteTestServer(t)
	dir := remoteDir(t)
	folder := filepath.Join(dir, "site")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"server": {"web"}, "dir": {dir}, "name": {"site"}, "t": {s.Token()}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/upload?"+q.Encode(), strings.NewReader("payload"))
	req.Header.Set("Origin", ts.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("upload onto a folder: status %d, want 409", res.StatusCode)
	}
	if entries, _ := os.ReadDir(folder); len(entries) != 0 {
		t.Fatalf("something landed inside the folder: %v", entries)
	}
}
