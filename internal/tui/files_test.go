// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"os"
	"strings"
	"testing"

	zone "github.com/lrstanley/bubblezone"
)

func TestMain(m *testing.M) {
	// bubblezone's global manager must exist before any View() that marks zones.
	zone.NewGlobal()
	os.Exit(m.Run())
}

func sampleFilesModel(w, h int) filesModel {
	return filesModel{
		server: "web-01",
		width:  w,
		height: h,
		focus:  0,
		left: paneState{
			cwd: "/home/op/project",
			entries: []fileItem{
				{name: "..", isDir: true},
				{name: "build", isDir: true},
				{name: "app.tar.gz", size: 12 << 20},
				{name: "notes.txt", size: 2048},
			},
			index: 2,
		},
		right: paneState{
			cwd:    "/var",
			remote: true,
			entries: []fileItem{
				{name: "log", isDir: true},
				{name: "backup.sql", size: 88 << 20},
			},
		},
		transfers: []*transferRow{
			{id: 1, label: "↑ app.tar.gz", bytesDone: 8 << 20, total: 12 << 20, rate: 9.6e6, streams: 4},
		},
	}
}

func TestFilesViewRenders(t *testing.T) {
	t.Parallel()
	out := sampleFilesModel(120, 40).View()
	for _, want := range []string{
		"Cenvero Fleet", // header brand
		"web-01",        // server tag
		"LOCAL",
		"REMOTE",
		"app.tar.gz", // a file row
		"build",      // a dir row
		"Transfers",  // progress panel
		"switch",     // footer hint label
		"%",          // progress percentage
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered view missing %q", want)
		}
	}
}

func TestFilesViewSmallTerminalNoPanic(t *testing.T) {
	t.Parallel()
	// Must not panic on a cramped terminal (guards width/Repeat math).
	for _, dim := range [][2]int{{40, 10}, {30, 8}, {80, 24}, {200, 60}} {
		_ = sampleFilesModel(dim[0], dim[1]).View()
	}
}

func TestSmoothBarBounds(t *testing.T) {
	t.Parallel()
	for _, pct := range []int{-10, 0, 1, 37, 50, 99, 100, 150} {
		_ = smoothBar(pct, 22) // must not panic for any input
	}
}
