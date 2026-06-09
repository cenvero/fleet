// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

func TestIsArchiveName(t *testing.T) {
	t.Parallel()
	archives := []string{
		"app.zip", "App.ZIP", "data.tar", "data.tar.gz", "data.tgz",
		"data.tar.bz2", "data.tar.xz", "blob.gz", "site.TAR.GZ",
	}
	for _, n := range archives {
		if !isArchiveName(n) {
			t.Fatalf("isArchiveName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"notes.txt", "main.go", "image.png", "archive", "tar"} {
		if isArchiveName(n) {
			t.Fatalf("isArchiveName(%q) = true, want false", n)
		}
	}
}

func TestDuplicateName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		existing []string
		want     string
	}{
		{"notes.txt", nil, "notes copy.txt"},
		{"notes.txt", []string{"notes copy.txt"}, "notes copy 2.txt"},
		{"notes.txt", []string{"notes copy.txt", "notes copy 2.txt"}, "notes copy 3.txt"},
		{"README", nil, "README copy"},
		{"app.tar.gz", nil, "app copy.tar.gz"},
		{"app.tar.gz", []string{"app copy.tar.gz"}, "app copy 2.tar.gz"},
	}
	for _, c := range cases {
		set := map[string]bool{}
		for _, e := range c.existing {
			set[e] = true
		}
		if got := duplicateName(c.name, set); got != c.want {
			t.Fatalf("duplicateName(%q, %v) = %q, want %q", c.name, c.existing, got, c.want)
		}
	}
}

func TestOctalModeString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode uint32
		want string
	}{
		{0o755, "755"},
		{0o644, "644"},
		{0o600, "600"},
		{0o7, "007"},
	}
	for _, c := range cases {
		if got := octalModeString(c.mode); got != c.want {
			t.Fatalf("octalModeString(%#o) = %q, want %q", c.mode, got, c.want)
		}
	}
}

func TestCompressOverlayCycleAndRender(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	m.left.index = 2 // app.tar.gz
	m = m.openCompress(0)
	if m.overlay != overlayCompress {
		t.Fatalf("openCompress should set overlayCompress, got %v", m.overlay)
	}
	formats := core.ArchiveFormats()
	if m.compressName != core.SuggestArchiveName(m.compressNames, formats[0]) {
		t.Fatalf("default name = %q, want suggested for first format", m.compressName)
	}

	// Cycling the format (while not editing) re-derives the default name so its
	// extension tracks the format.
	m.cycleCompressFormat(1)
	if m.compressFormat != 1 {
		t.Fatalf("cycleCompressFormat should advance to index 1, got %d", m.compressFormat)
	}
	if m.compressName != core.SuggestArchiveName(m.compressNames, formats[1]) {
		t.Fatalf("name should track cycled format, got %q", m.compressName)
	}

	// Once the user edits the name, cycling must not clobber it.
	m.compressEditing = true
	m.compressName = "myarchive.zip"
	m.cycleCompressFormat(1)
	if m.compressName != "myarchive.zip" {
		t.Fatalf("cycling after edit clobbered the name: %q", m.compressName)
	}

	// The overlay renders its title, format, and name.
	out := m.View()
	for _, want := range []string{"Compress", "myarchive.zip"} {
		if !strings.Contains(out, want) {
			t.Fatalf("compress overlay missing %q", want)
		}
	}
}

func TestChecksumDonePopulatesProperties(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	model, _ := m.onChecksumDone(checksumDoneMsg{
		side: 0, name: "notes.txt", path: "/home/op/project/notes.txt",
		sum: "abc123def456",
	})
	fm := model.(filesModel)
	if fm.overlay != overlayProperties {
		t.Fatalf("checksum should open properties overlay, got %v", fm.overlay)
	}
	if !strings.Contains(fm.propsText, "abc123def456") {
		t.Fatalf("properties should show the hash, got %q", fm.propsText)
	}
	if !strings.Contains(fm.status, "abc123def456") {
		t.Fatalf("status should show the hash, got %q", fm.status)
	}
}

func TestChmodPromptSeedsCurrentMode(t *testing.T) {
	t.Parallel()
	m := sampleFilesModel(120, 40)
	m.left.entries[3].mode = uint32(0o644) // notes.txt
	m.left.index = 3
	m = m.openChmodPrompt(0)
	if m.overlay != overlayPrompt || m.prompt != promptChmod {
		t.Fatalf("openChmodPrompt should open a chmod prompt, got overlay=%v prompt=%v", m.overlay, m.prompt)
	}
	if m.promptValue != "644" {
		t.Fatalf("chmod prompt should seed current mode, got %q", m.promptValue)
	}
}
