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

// BenchmarkViewCold50k is a full frame build for a 50k-entry directory. Only
// the visible window should be rendered, so this must stay close to the 1k
// case.
func BenchmarkViewCold50k(b *testing.B) {
	zone.NewGlobal()
	m := benchModel(50000)
	b.ReportAllocs()
	for b.Loop() {
		m.frames.valid = false
		_ = m.View()
	}
}

// BenchmarkScroll50k is holding ↓ in a 50k-entry directory: every step moves
// the cursor and renders a new frame.
func BenchmarkScroll50k(b *testing.B) {
	zone.NewGlobal()
	m := benchModel(50000)
	_ = m.View()
	b.ReportAllocs()
	for b.Loop() {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = mm.(filesModel)
		if m.left.index >= len(m.left.entries)-1 {
			m.left.index, m.left.scroll = 0, 0
		}
		_ = m.View()
	}
}

// BenchmarkMouseMotionAcrossRows50k sweeps the pointer over different rows of
// a 50k-entry pane, so every event changes the hover target and re-renders.
func BenchmarkMouseMotionAcrossRows50k(b *testing.B) {
	zone.NewGlobal()
	m := benchModel(50000)
	_ = m.View()
	b.ReportAllocs()
	y := 0
	for b.Loop() {
		mm, _ := m.Update(tea.MouseMsg{
			X: 20, Y: 8 + y%20, Action: tea.MouseActionMotion, Button: tea.MouseButtonNone,
		})
		m = mm.(filesModel)
		_ = m.View()
		y++
	}
}

// BenchmarkReapply50k measures re-sorting/filtering a 50k listing (sort key
// change, filter keystroke).
func BenchmarkReapply50k(b *testing.B) {
	m := benchModel(50000)
	b.ReportAllocs()
	for b.Loop() {
		m.reapplyPane(0)
	}
}
