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

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestValidatePathComponent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		style core.TargetPathStyle
		value string
		valid bool
	}{
		{"posix ordinary", core.TargetPathPOSIX, "notes.txt", true},
		{"posix Windows reserved", core.TargetPathPOSIX, "CON", true},
		{"posix trailing dot", core.TargetPathPOSIX, "trail.", true},
		{"empty", core.TargetPathPOSIX, "", false},
		{"dot", core.TargetPathPOSIX, ".", false},
		{"dot dot", core.TargetPathPOSIX, "..", false},
		{"slash", core.TargetPathPOSIX, "a/b", false},
		{"backslash", core.TargetPathPOSIX, `a\b`, false},
		{"drive absolute", core.TargetPathPOSIX, `C:\Temp`, false},
		{"UNC absolute", core.TargetPathPOSIX, `\\host\share`, false},
		{"control", core.TargetPathPOSIX, "bad\x01name", false},
		{"Windows ordinary", core.TargetPathWindows, "notes.txt", true},
		{"Windows colon", core.TargetPathWindows, "bad:name", false},
		{"Windows angle", core.TargetPathWindows, "bad<name", false},
		{"Windows quote", core.TargetPathWindows, `bad"name`, false},
		{"Windows pipe", core.TargetPathWindows, "bad|name", false},
		{"Windows question", core.TargetPathWindows, "bad?name", false},
		{"Windows asterisk", core.TargetPathWindows, "bad*name", false},
		{"Windows trailing dot", core.TargetPathWindows, "trail.", false},
		{"Windows trailing space", core.TargetPathWindows, "trail ", false},
		{"Windows reserved", core.TargetPathWindows, "CON", false},
		{"Windows reserved extension", core.TargetPathWindows, "con.txt", false},
		{"Windows reserved port", core.TargetPathWindows, "LPT9.log", false},
		{"Windows reserved superscript", core.TargetPathWindows, "COM¹", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePathComponent(tt.style, tt.value)
			if (err == nil) != tt.valid {
				t.Fatalf("validatePathComponent(%q, %q) error = %v, valid=%v", tt.style, tt.value, err, tt.valid)
			}
		})
	}
}

func TestNameOnlyHandlersRejectInvalidComponents(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.app.SaveServer(core.ServerRecord{
		Name:     "windows-node",
		Observed: core.ServerObservation{Reachable: true, OS: "windows"},
	}); err != nil {
		t.Fatal(err)
	}

	post := func(endpoint string, params url.Values) (int, string) {
		t.Helper()
		params.Set("t", s.Token())
		req, err := http.NewRequest(http.MethodPost, ts.URL+endpoint+"?"+params.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", ts.URL)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", endpoint, err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}

	tests := []struct {
		name     string
		endpoint string
		params   url.Values
	}{
		{"mkdir", "/api/mkdir", url.Values{"dir": {dir}, "name": {"nested/escape"}}},
		{"mkdir Windows reserved", "/api/mkdir", url.Values{"server": {"windows-node"}, "dir": {`C:\\Data`}, "name": {"CON.txt"}}},
		{"touch", "/api/touch", url.Values{"dir": {dir}, "name": {`nested\escape`}}},
		{"rename", "/api/mv", url.Values{"from": {source}, "name": {"../escape"}}},
		{"rename full-path bypass", "/api/mv", url.Values{"from": {source}, "to": {filepath.Join(dir, `bad\\name`)}}},
		{"archive output", "/api/compress", url.Values{"dir": {dir}, "archive": {"nested/archive.zip"}, "format": {"zip"}, "name": {"source.txt"}}},
		{"archive selected name", "/api/compress", url.Values{"dir": {dir}, "archive": {"archive.zip"}, "format": {"zip"}, "name": {"nested/source.txt"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := post(tt.endpoint, tt.params)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", status, body)
			}
		})
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("invalid rename changed source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested")); !os.IsNotExist(err) {
		t.Fatalf("invalid component created a nested path: %v", err)
	}
}

func TestDuplicateHandlerRejectsInvalidSourceComponent(t *testing.T) {
	t.Parallel()
	if core.NativePathStyle().IsWindows() {
		t.Skip("Windows cannot create the invalid source component used by this handler test")
	}
	s, ts := newTestServer(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, `bad\name.txt`)
	if err := os.WriteFile(bad, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	params := url.Values{"path": {bad}, "t": {s.Token()}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/duplicate?"+params.Encode(), nil)
	req.Header.Set("Origin", ts.URL)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 400; body=%s", res.StatusCode, body)
	}
}

func TestDuplicateRemotePathDispatchesByStat(t *testing.T) {
	t.Parallel()
	for _, isDir := range []bool{false, true} {
		isDir := isDir
		t.Run(map[bool]string{false: "file", true: "directory"}[isDir], func(t *testing.T) {
			source := `C:\Data\item.txt`
			var copiedFile, copiedDir bool
			stat := func(candidate string) (proto.FileStatResult, error) {
				if candidate == source {
					return proto.FileStatResult{Entry: proto.FileEntry{IsDir: isDir}}, nil
				}
				return proto.FileStatResult{}, errors.New("not found")
			}
			dst, err := duplicateRemotePath(
				core.TargetPathWindows,
				source,
				stat,
				func(src, target string) error {
					copiedFile = src == source && target == `C:\Data\item copy.txt`
					return nil
				},
				func(src, target string) error {
					copiedDir = src == source && target == `C:\Data\item copy.txt`
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if dst != `C:\Data\item copy.txt` {
				t.Fatalf("destination = %q", dst)
			}
			if copiedFile != !isDir || copiedDir != isDir {
				t.Fatalf("dispatch: copiedFile=%v copiedDir=%v isDir=%v", copiedFile, copiedDir, isDir)
			}
		})
	}
}

func TestNameOnlyPathUsesManagedSourceStyle(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	if err := s.app.SaveServer(core.ServerRecord{
		Name:     "windows-node",
		Observed: core.ServerObservation{Reachable: true, OS: "windows"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.nameOnlyPath("windows-node", `\\host\share\folder`, "child.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := `\\host\share\folder\child.txt`; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if _, err := s.nameOnlyPath("windows-node", `C:\Data`, "CON.txt"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected Windows reserved-name error, got %v", err)
	}
}

func TestUploadHandlerRejectsRawInvalidNames(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	if err := s.app.SaveServer(core.ServerRecord{
		Name: "windows-upload", Observed: core.ServerObservation{Reachable: true, OS: "windows"},
	}); err != nil {
		t.Fatal(err)
	}
	localDir := t.TempDir()
	tests := []struct {
		name   string
		server string
		dir    string
		file   string
	}{
		{name: "local nested", dir: localDir, file: "nested/file.txt"},
		{name: "local backslash", dir: localDir, file: `nested\file.txt`},
		{name: "Windows reserved", server: "windows-upload", dir: `C:\Data`, file: "CON.txt"},
		{name: "Windows trailing dot", server: "windows-upload", dir: `C:\Data`, file: "report."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := url.Values{"t": {s.Token()}, "server": {tc.server}, "dir": {tc.dir}, "name": {tc.file}}
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/upload?"+params.Encode(), strings.NewReader("payload"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Origin", ts.URL)
			res, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(res.Body)
				t.Fatalf("status = %d, want 400; body=%s", res.StatusCode, body)
			}
		})
	}
	if entries, err := os.ReadDir(localDir); err != nil || len(entries) != 0 {
		t.Fatalf("invalid local upload mutated destination: entries=%v err=%v", entries, err)
	}
}
