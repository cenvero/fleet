// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

type previewText struct {
	Binary    bool   `json:"binary"`
	Size      int64  `json:"size"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

func getPreview(t *testing.T, s *Server, base string, q url.Values) (*http.Response, []byte) {
	t.Helper()
	q.Set("t", s.Token())
	res, err := http.Get(base + "/api/preview?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func TestPreviewTextLocal(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()

	small := filepath.Join(dir, "page.html")
	html := `<script>alert(1)</script><b>hi</b>`
	if err := os.WriteFile(small, []byte(html), 0o600); err != nil {
		t.Fatal(err)
	}
	res, body := getPreview(t, s, ts.URL, url.Values{"path": {small}, "kind": {"text"}})
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var out previewText
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != html || out.Truncated || out.Binary {
		t.Fatalf("html preview = %+v (must be returned as inert JSON text)", out)
	}

	// A large file is cut at the limit on a rune boundary.
	big := filepath.Join(dir, "big.txt")
	content := strings.Repeat("é", maxPreviewTextBytes) // 2 bytes per rune
	if err := os.WriteFile(big, []byte("x"+content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, body = getPreview(t, s, ts.URL, url.Values{"path": {big}})
	out = previewText{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || out.Size != int64(1+len(content)) || len(out.Content) > maxPreviewTextBytes || !utf8.ValidString(out.Content) || strings.ContainsRune(out.Content, utf8.RuneError) {
		t.Fatalf("truncated preview: truncated=%v size=%d len=%d", out.Truncated, out.Size, len(out.Content))
	}

	bin := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(bin, []byte{0, 1, 2, 3, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, body = getPreview(t, s, ts.URL, url.Values{"path": {bin}})
	out = previewText{}
	_ = json.Unmarshal(body, &out)
	if !out.Binary || out.Content != "" {
		t.Fatalf("binary preview = %+v", out)
	}
}

func TestPreviewImageLocalIsSafe(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	png := filepath.Join(dir, "pic.png")
	data := []byte("\x89PNG\r\n\x1a\nnot-really")
	if err := os.WriteFile(png, data, 0o600); err != nil {
		t.Fatal(err)
	}
	res, body := getPreview(t, s, ts.URL, url.Values{"path": {png}, "kind": {"image"}})
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type %q", ct)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("image preview without nosniff")
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
		t.Fatalf("image CSP %q", csp)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Fatalf("Content-Disposition %q", cd)
	}

	// Active document formats are never served inline as images.
	for _, name := range []string{"logo.svg", "page.html", "x.xhtml", "doc.pdf", "noext"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), 0o600); err != nil {
			t.Fatal(err)
		}
		res, _ := getPreview(t, s, ts.URL, url.Values{"path": {p}, "kind": {"image"}})
		if res.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("%s image preview status %d, want 415", name, res.StatusCode)
		}
	}

	large := filepath.Join(dir, "huge.png")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxPreviewImageBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if res, _ := getPreview(t, s, ts.URL, url.Values{"path": {large}, "kind": {"image"}}); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized image status %d", res.StatusCode)
	}
	if res, _ := getPreview(t, s, ts.URL, url.Values{"path": {png}, "kind": {"script"}}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown kind status %d", res.StatusCode)
	}
	if res, _ := getPreview(t, s, ts.URL, url.Values{"path": {"relative/pic.png"}, "kind": {"image"}}); res.StatusCode == http.StatusOK {
		t.Fatalf("relative path accepted")
	}
}

func TestPreviewRemote(t *testing.T) {
	text := []byte(strings.Repeat("line of text\n", 1000))
	fake := newFakeRemote(text)
	s, base := newStreamTestServer(t, fake)
	res, body := getPreview(t, s, base, url.Values{"server": {"node"}, "path": {"/srv/notes.txt"}, "kind": {"text"}})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", res.StatusCode, body)
	}
	var out previewText
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != string(text) || out.Truncated {
		t.Fatalf("remote text preview mismatch: truncated=%v len=%d", out.Truncated, len(out.Content))
	}

	img := newFakeRemote([]byte("GIF89a...."))
	s.files = img
	res, body = getPreview(t, s, base, url.Values{"server": {"node"}, "path": {"/srv/anim.GIF"}, "kind": {"image"}})
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/gif" || string(body) != "GIF89a...." {
		t.Fatalf("remote image: %d %q %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
}
