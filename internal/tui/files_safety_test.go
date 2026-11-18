// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
)

var (
	keyTab   = tea.KeyMsg{Type: tea.KeyTab}
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
	keyCtrlS = tea.KeyMsg{Type: tea.KeyCtrlS}
)

// localPaneModel is a model whose left pane lists dir, focused on name.
func localPaneModel(dir string, names ...string) filesModel {
	m := sampleFilesModel(120, 40)
	m.left.cwd = dir
	m.left.listedCwd = dir
	m.left.pathStyle = core.NativePathStyle()
	m.left.allItems = nil
	for _, n := range names {
		m.left.allItems = append(m.left.allItems, fileItem{name: n})
	}
	m.reapplyPane(0)
	m.left.index = 1 // the first real entry (after "..")
	return m
}

// openInEditor opens the focused file of the left pane and delivers the load.
func openInEditor(t *testing.T, m filesModel) filesModel {
	t.Helper()
	mm, cmd := m.openEditor(0)
	m = mm.(filesModel)
	if cmd == nil {
		t.Fatalf("openEditor returned no load command (status %q)", m.status)
	}
	mm, _ = m.Update(cmd())
	m = mm.(filesModel)
	if m.editor == nil {
		t.Fatalf("editor closed after load: %q", m.status)
	}
	return m
}

func sendKey(m filesModel, k tea.KeyMsg) (filesModel, tea.Cmd) {
	mm, cmd := m.Update(k)
	return mm.(filesModel), cmd
}

// saveAndDeliver presses Ctrl+S, runs the save off the UI thread and delivers
// its result. It reports whether a save command ran at all.
func saveAndDeliver(t *testing.T, m filesModel) (filesModel, bool) {
	t.Helper()
	m, cmd := sendKey(m, keyCtrlS)
	if cmd == nil {
		return m, false
	}
	msg := cmd()
	if _, ok := msg.(editorSavedMsg); !ok {
		t.Fatalf("save command produced %T, want editorSavedMsg", msg)
	}
	mm, _ := m.Update(msg)
	return mm.(filesModel), true
}

// TestEditorArrowThenSaveKeepsBytes is the invariant for the editor: open a
// file, switch to edit mode, press an arrow key, then save. The file must be
// byte-identical, and the arrow key must not mark it dirty. Before the fix the
// textarea's input sanitizer rewrote tabs, CR and control characters on load,
// so the arrow marked the file dirty and Ctrl+S wrote the altered text.
func TestEditorArrowThenSaveKeepsBytes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain ascii":         "hello world\nsecond line\n",
		"no trailing newline": "one\ntwo",
		"empty":               "",
		"multibyte utf8":      "héllo 世界 😀\n",
		"bom":                 string(rune(0xFEFF)) + "x\n",
		"tabs":                "func main() {\n\tprintln(1)\n}\n",
		"tabs mid-line":       "a\tb\t\tc\n",
		"crlf":                "a\r\nb\r\n",
		"crlf no trailing":    "a\r\nb",
		"crlf with tabs":      "all:\r\n\tgo build\r\n",
		"lone cr":             "progress 10%\rprogress 100%\n",
		"trailing cr":         "a\nb\r",
		"mixed endings":       "a\r\nb\n",
		"invalid utf8":        "caf\xe9\n",
		"control char":        "bell\x07\n",
		"del":                 "x\x7fy\n",
		"c1 control":          "x" + string(rune(0x85)) + "y\n",
		"replacement char":    "a" + string(rune(0xFFFD)) + "b\n",
		"tab glyph literal":   "x" + string(rune(0x21E5)) + "y\n",
		"tab and tab glyph":   "\tx" + string(rune(0x21E5)) + "\n",
		"over 10000 lines":    strings.Repeat("l\n", 10001),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := filepath.Join(dir, "f.txt")
			if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			m := openInEditor(t, localPaneModel(dir, "f.txt"))
			m, _ = sendKey(m, keyTab)
			if _, _, why := newEditorArea(raw); why == "" && m.editor.mode != editorEdit {
				t.Fatalf("an editable file did not enter edit mode: %q", m.editor.status)
			}
			m, _ = sendKey(m, keyDown)
			if m.editor.dirty {
				t.Errorf("an arrow key marked the file dirty (mode %v)", m.editor.mode)
			}
			m, _ = saveAndDeliver(t, m)
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != raw {
				t.Fatalf("save changed the file:\n got %s\nwant %s", clip(string(got)), clip(raw))
			}
		})
	}
}

// clip quotes s for a failure message, eliding the middle of long values.
func clip(s string) string {
	if len(s) <= 80 {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q…%q (%d bytes)", s[:30], s[len(s)-30:], len(s))
}

// TestEditorCodecRoundTrip tables what opens editable and what read-only, and
// that an editable file's textarea text decodes back to the exact bytes.
func TestEditorCodecRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, raw string
		editable  bool
		crlf      bool
	}{
		{"plain ascii", "hello\nworld\n", true, false},
		{"no trailing newline", "hello\nworld", true, false},
		{"empty", "", true, false},
		{"tabs", "\tif x {\n\t\treturn\n\t}\n", true, false},
		{"crlf", "a\r\nb\r\n", true, true},
		{"crlf no trailing newline", "a\r\nb", true, true},
		{"crlf and tabs", "\ta\r\n\tb\r\n", true, true},
		{"lone cr", "a\rb\n", false, false},
		{"trailing lone cr", "a\nb\r", false, false},
		{"mixed endings", "a\r\nb\n", false, false},
		{"invalid utf8", "caf\xe9\n", false, false},
		{"nul after the binary sniff", strings.Repeat("x", 9000) + "\x00\n", false, false},
		{"control char", "a\x1bb\n", false, false},
		{"c1 control", "a" + string(rune(0x9b)) + "b\n", false, false},
		{"replacement char", "a" + string(rune(0xFFFD)) + "\n", false, false},
		{"tab glyph", string(rune(0x21E5)) + "\n", false, false},
		{"too many lines", strings.Repeat("\n", textareaMaxLines), false, false},
		{"just under the line cap", strings.Repeat("\n", textareaMaxLines-1), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ta, c, why := newEditorArea(tc.raw)
			if got := why == ""; got != tc.editable {
				t.Fatalf("editable = %v (why %q), want %v", got, why, tc.editable)
			}
			if !tc.editable {
				if ta.Value() != "" {
					t.Fatal("a read-only file's textarea should hold nothing")
				}
				return
			}
			if c.crlf != tc.crlf {
				t.Fatalf("crlf = %v, want %v", c.crlf, tc.crlf)
			}
			if got := c.decode(ta.Value()); got != tc.raw {
				t.Fatalf("round trip %s, want %s", clip(got), clip(tc.raw))
			}
			if strings.ContainsAny(ta.Value(), "\t\r") {
				t.Fatalf("textarea text still has a tab or CR: %q", ta.Value())
			}
		})
	}
}

// TestEditorEditKeepsTabsAndCRLF: tabs and CRLF line endings survive a real
// edit, and lines typed in a CRLF file get CRLF endings too.
func TestEditorEditKeepsTabsAndCRLF(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, raw, typed, want string }{
		{"tabs", "all:\n\tgo build\n", "x", "all:\n\tgo build\nx"},
		{"crlf", "a\r\n\tb\r\n", "c", "a\r\n\tb\r\nc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := filepath.Join(dir, "Makefile")
			if err := os.WriteFile(p, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			m := openInEditor(t, localPaneModel(dir, "Makefile"))
			m, _ = sendKey(m, keyTab)
			if m.editor.mode != editorEdit {
				t.Fatalf("file did not open for editing: %q", m.editor.status)
			}
			m, _ = sendKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.typed)})
			if !m.editor.dirty {
				t.Fatal("typing should mark the file dirty")
			}
			m, ran := saveAndDeliver(t, m)
			if !ran {
				t.Fatalf("Ctrl+S did not save: %q", m.editor.status)
			}
			got, _ := os.ReadFile(p)
			if string(got) != tc.want {
				t.Fatalf("saved %q, want %q", got, tc.want)
			}
			if m.editor.dirty {
				t.Fatal("editor still dirty after a successful save")
			}
		})
	}
}

// TestEditorUnrepresentableOpensReadOnly: content the textarea cannot hold
// exactly opens in the viewer only, says why, and Ctrl+S never writes it.
func TestEditorUnrepresentableOpensReadOnly(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"lone cr":       "a\rb\n",
		"mixed endings": "a\r\nb\n",
		"invalid utf8":  "caf\xe9\n",
		"control char":  "bell\x07\n",
		"many lines":    strings.Repeat("l\n", 10001),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := filepath.Join(dir, "f.txt")
			if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			m := openInEditor(t, localPaneModel(dir, "f.txt"))
			m, _ = sendKey(m, keyTab)
			if m.editor.mode == editorEdit {
				t.Fatal("unrepresentable content entered edit mode")
			}
			if !strings.Contains(m.editor.status, "read-only") || !strings.Contains(m.editor.status, "$EDITOR") {
				t.Fatalf("status should explain the read-only open, got %q", m.editor.status)
			}
			m, _ = saveAndDeliver(t, m)
			if got, _ := os.ReadFile(p); string(got) != raw {
				t.Fatalf("read-only file was rewritten: %q", got)
			}
		})
	}
}
