// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"
)

func benchModel(n int) filesModel {
	items := make([]fileItem, 0, n)
	for i := range n {
		items = append(items, fileItem{
			name:    fmt.Sprintf("some-file-name-%04d.tar.gz", i),
			isDir:   i%7 == 0,
			size:    int64(i) * 4096,
			mode:    0o644,
			modTime: time.Unix(1700000000+int64(i), 0),
		})
	}
	m := filesModel{
		width: 160, height: 48,
		left:       paneState{source: "", cwd: "/home/user", allItems: items, selected: map[int]bool{}},
		right:      paneState{source: "web-1", remote: true, cwd: "/var/www", allItems: items, selected: map[int]bool{}},
		chans:      make(map[int]*transferChans),
		hoverSide:  0,
		hoverIndex: 3,
		frames:     &frameCache{},
	}
	m.reapplyPane(0)
	m.reapplyPane(1)
	return m
}

// BenchmarkViewCold measures a full frame build (cache miss) — the cost paid
// whenever something actually changes on screen.
func BenchmarkViewCold(b *testing.B) {
	zone.NewGlobal()
	m := benchModel(1000)
	b.ReportAllocs()
	for b.Loop() {
		m.frames.valid = false
		_ = m.View()
	}
}

// BenchmarkMouseMotionFrame is the hot path: WithMouseAllMotion delivers one
// event per cursor movement and Bubble Tea calls View after every Update. Motion
// within the same row must not rebuild the frame.
func BenchmarkMouseMotionFrame(b *testing.B) {
	zone.NewGlobal()
	m := benchModel(1000)
	_ = m.View() // prime the cache
	b.ReportAllocs()
	for b.Loop() {
		mm, _ := m.handleMouseMotion(tea.MouseMsg{
			X: 10, Y: 14, Action: tea.MouseActionMotion, Button: tea.MouseButtonNone,
		})
		m = mm.(filesModel)
		_ = m.View()
	}
}
