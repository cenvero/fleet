// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/rivo/uniseg"
)

// unpaintedCells reports the screen cells of frame (w columns wide) that are
// drawn without a background colour. The file manager paints its own dark
// theme, so every such cell shows the terminal's own background — on a light
// terminal, a white hole or light-grey text on white. SGR state carries across
// lines, as it does in a terminal.
func unpaintedCells(frame string, w int) []string {
	var holes []string
	bg := false
	for row, line := range strings.Split(frame, "\n") {
		col := 0
		start := -1
		var text strings.Builder
		flush := func(end int) {
			if start >= 0 {
				holes = append(holes, fmt.Sprintf("row %d cols %d-%d %q", row, start, end-1, strings.TrimSpace(text.String())))
				start = -1
				text.Reset()
			}
		}
		cell := func(s string, width int) {
			if !bg {
				if start < 0 {
					start = col
				}
				text.WriteString(s)
			} else {
				flush(col)
			}
			col += width
		}
		for i := 0; i < len(line); {
			if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
				j := i + 2
				for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
					j++
				}
				if j < len(line) && line[j] == 'm' {
					bg = applySGRBackground(bg, line[i+2:j])
				}
				i = j + 1
				continue
			}
			gr := uniseg.NewGraphemes(line[i:])
			gr.Next()
			s := gr.Str()
			cell(s, uniseg.StringWidth(s))
			i += len(s)
		}
		for col < w {
			cell(" ", 1)
		}
		flush(col)
	}
	return holes
}

// applySGRBackground reports whether a background colour is set after the SGR
// parameters params are applied.
func applySGRBackground(bg bool, params string) bool {
	if params == "" {
		return false
	}
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		n, _ := strconv.Atoi(parts[i])
		switch {
		case n == 0 || n == 49:
			bg = false
		case n >= 40 && n <= 47, n >= 100 && n <= 107:
			bg = true
		case n == 7:
			bg = true // reverse video paints the cell with the foreground colour
		case n == 48:
			bg = true
			if i+1 < len(parts) && parts[i+1] == "5" {
				i += 2
			} else if i+1 < len(parts) && parts[i+1] == "2" {
				i += 4
			}
		case n == 38:
			if i+1 < len(parts) && parts[i+1] == "5" {
				i += 2
			} else if i+1 < len(parts) && parts[i+1] == "2" {
				i += 4
			}
		}
	}
	return bg
}

func pressKeys(t *testing.T, m filesModel, keys ...tea.KeyMsg) filesModel {
	t.Helper()
	for _, k := range keys {
		next, _ := m.Update(k)
		m = next.(filesModel)
	}
	return m
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// TestFilesFramesPaintEveryCell is a regression test for the file manager on
// light terminals: text drawn with a foreground-only style (the help screen's
// descriptions, dialog labels, the footer's tail) fell back to the terminal's
// own background, which on a light terminal left light-grey text on white and
// white gaps in the frame.
func TestFilesFramesPaintEveryCell(t *testing.T) {
	// Not parallel: SetColorProfile is process-global.
	for _, prof := range []termenv.Profile{termenv.TrueColor, termenv.ANSI256} {
		prev := lipgloss.ColorProfile()
		lipgloss.SetColorProfile(prof)
		t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

		for _, dim := range [][2]int{{120, 40}, {150, 40}, {150, 42}, {80, 24}, {200, 60}} {
			w, h := dim[0], dim[1]
			states := map[string]func() filesModel{
				"base": func() filesModel { return sampleFilesModel(w, h) },
				"help": func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("?")) },
				"delete-confirm": func() filesModel {
					return pressKeys(t, sampleFilesModel(w, h), runes("d"))
				},
				"new-folder": func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("n")) },
				"rename":     func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("r")) },
				"properties": func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("i")) },
				"filter":     func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("/")) },
				"goto":       func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes(":")) },
				"jump":       func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("f")) },
				"places":     func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("'")) },
				"compress":   func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("z")) },
				"source-picker": func() filesModel {
					m := sampleFilesModel(w, h)
					m.overlay = overlaySourcePicker
					m.pickerSide = 0
					m.pickerItems = []string{"Local", "web-01"}
					return m
				},
				"context-menu": func() filesModel {
					m := sampleFilesModel(w, h)
					next, _ := m.Update(tea.MouseMsg{X: 20, Y: 6, Action: tea.MouseActionPress, Button: tea.MouseButtonRight})
					return next.(filesModel)
				},
				"grid": func() filesModel { return pressKeys(t, sampleFilesModel(w, h), runes("v")) },
				"editor": func() filesModel {
					m := sampleFilesModel(w, h)
					m.overlay = overlayEditor
					m.editor = &editorState{active: true, path: "/x/main.go", name: "main.go"}
					next, _ := m.onEditorLoaded(editorLoadedMsg{path: "/x/main.go", name: "main.go", content: "package main\n\n// hi\nfunc main() {}\n"})
					return next.(filesModel)
				},
				"editor-edit": func() filesModel {
					m := sampleFilesModel(w, h)
					m.overlay = overlayEditor
					m.editor = &editorState{active: true, path: "/x/a.txt", name: "a.txt"}
					next, _ := m.onEditorLoaded(editorLoadedMsg{path: "/x/a.txt", name: "a.txt", content: "one\ntwo\n"})
					next, _ = next.(filesModel).toggleEditorMode()
					return next.(filesModel)
				},
				"copy-move-menu": func() filesModel {
					m := sampleFilesModel(w, h)
					m.overlay = overlayCopyMove
					m.cmX, m.cmY = 30, 10
					return m
				},
			}
			for name, build := range states {
				m := build()
				if name != "base" && name != "grid" && m.overlay == overlayNone {
					t.Errorf("%v %dx%d %s: the state did not open an overlay", prof, w, h, name)
					continue
				}
				if holes := unpaintedCells(m.View(), w); len(holes) > 0 {
					if len(holes) > 8 {
						holes = append(holes[:8], fmt.Sprintf("… %d more", len(holes)-8))
					}
					t.Errorf("%v %dx%d %s: cells without a background:\n  %s", prof, w, h, name, strings.Join(holes, "\n  "))
				}
			}
		}
	}
}
