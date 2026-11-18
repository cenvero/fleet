// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/pkg/proto"
	tea "github.com/charmbracelet/bubbletea"
)

var (
	keyTab   = tea.KeyMsg{Type: tea.KeyTab}
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
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

// TestEditorEscAfterToggleNeedsSecondPress: with unsaved edits, leaving edit
// mode with Tab and pressing Esc once must not discard them.
func TestEditorEscAfterToggleNeedsSecondPress(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := openInEditor(t, localPaneModel(dir, "f.txt"))
	m, _ = sendKey(m, keyTab)
	m, _ = sendKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !m.editor.dirty {
		t.Fatal("typing should mark the file dirty")
	}
	m, _ = sendKey(m, keyTab) // back to the viewer
	m, _ = sendKey(m, keyEsc)
	if m.editor == nil || m.overlay != overlayEditor {
		t.Fatal("one Esc from the viewer discarded unsaved edits")
	}
	if !strings.Contains(m.editor.status, "Esc again") {
		t.Fatalf("first Esc should warn, got %q", m.editor.status)
	}
	// Any other key disarms: Esc then has to be pressed twice again.
	m, _ = sendKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m, _ = sendKey(m, keyEsc)
	if m.editor == nil {
		t.Fatal("Esc after another key should warn again, not discard")
	}
	m, _ = sendKey(m, keyEsc)
	if m.editor != nil || m.overlay == overlayEditor {
		t.Fatal("second consecutive Esc should close the editor")
	}

	// Edit mode keeps its two-press behaviour.
	m = openInEditor(t, localPaneModel(dir, "f.txt"))
	m, _ = sendKey(m, keyTab)
	m, _ = sendKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m, _ = sendKey(m, keyEsc)
	if m.editor == nil || m.editor.mode != editorView {
		t.Fatal("first Esc in edit mode should return to the viewer")
	}
	m, _ = sendKey(m, keyEsc)
	if m.editor != nil {
		t.Fatal("second Esc should discard and close")
	}

	// A clean file still closes on one Esc.
	m = openInEditor(t, localPaneModel(dir, "f.txt"))
	m, _ = sendKey(m, keyEsc)
	if m.editor != nil {
		t.Fatal("Esc on an unmodified file should close at once")
	}
}

// TestLocalEditSaveIsAtomicAndChecked covers the local save path: it replaces
// the file atomically, keeps its mode, and refuses when the file on disk is no
// longer the one that was opened.
func TestLocalEditSaveIsAtomicAndChecked(t *testing.T) {
	t.Parallel()

	t.Run("read-only file", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root may write a read-only file")
		}
		dir := t.TempDir()
		p := filepath.Join(dir, "ro.txt")
		if err := os.WriteFile(p, []byte("v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o400); err != nil {
			t.Fatal(err)
		}
		err := saveFileFromEdit(nil, "", p, []byte("v2\n"), []byte("v1\n"), false)
		if err == nil || !strings.Contains(err.Error(), "may not write") {
			t.Fatalf("saving a read-only file should be refused like a write, got %v", err)
		}
		if got, _ := os.ReadFile(p); string(got) != "v1\n" {
			t.Fatalf("read-only file replaced: %q", got)
		}
	})

	t.Run("unchanged save writes nothing", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "same.txt")
		if err := os.WriteFile(p, []byte("same\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Stat(p)
		if err := saveFileFromEdit(nil, "", p, []byte("same\n"), []byte("same\n"), false); err != nil {
			t.Fatalf("save: %v", err)
		}
		after, _ := os.Stat(p)
		if !os.SameFile(before, after) {
			t.Fatal("saving unchanged text replaced the file")
		}
	})

	t.Run("atomic replace keeps mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, "run.sh")
		if err := os.WriteFile(p, []byte("echo 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o750); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Stat(p)
		if err := saveFileFromEdit(nil, "", p, []byte("echo 2\n"), []byte("echo 1\n"), false); err != nil {
			t.Fatalf("save: %v", err)
		}
		after, _ := os.Stat(p)
		if got, _ := os.ReadFile(p); string(got) != "echo 2\n" {
			t.Fatalf("content %q", got)
		}
		if after.Mode().Perm() != 0o750 {
			t.Fatalf("mode %v, want 0750", after.Mode().Perm())
		}
		if os.SameFile(before, after) {
			t.Fatal("file was rewritten in place, not replaced atomically")
		}
		left, _ := os.ReadDir(dir)
		if len(left) != 1 {
			t.Fatalf("temp debris left behind: %v", left)
		}
	})

	t.Run("changed on disk", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(p, []byte("theirs\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := saveFileFromEdit(nil, "", p, []byte("mine\n"), []byte("original\n"), false)
		if err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("want a changed-on-disk refusal, got %v", err)
		}
		if got, _ := os.ReadFile(p); string(got) != "theirs\n" {
			t.Fatalf("file overwritten: %q", got)
		}
	})

	t.Run("deleted since opened", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "gone.txt")
		if err := saveFileFromEdit(nil, "", p, []byte("mine\n"), []byte("original\n"), false); err == nil {
			t.Fatal("saving a file that was deleted meanwhile should be refused")
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatal("save recreated a deleted file")
		}
	})

	t.Run("swapped for a symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, "f.txt")
		other := filepath.Join(dir, "other.txt")
		if err := os.WriteFile(other, []byte("precious\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// f.txt was opened with "original\n" and has since become a link.
		if err := os.Symlink(other, p); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
		if err := saveFileFromEdit(nil, "", p, []byte("mine\n"), []byte("original\n"), false); err == nil {
			t.Fatal("save through a swapped-in symlink should be refused")
		}
		if got, _ := os.ReadFile(other); string(got) != "precious\n" {
			t.Fatalf("symlink target overwritten: %q", got)
		}
	})

	t.Run("symlinked file edits its target", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "real.conf")
		link := filepath.Join(dir, "link.conf")
		if err := os.WriteFile(target, []byte("a=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
		if err := saveFileFromEdit(nil, "", link, []byte("a=2\n"), []byte("a=1\n"), false); err != nil {
			t.Fatalf("save through a symlink: %v", err)
		}
		if got, _ := os.ReadFile(target); string(got) != "a=2\n" {
			t.Fatalf("target %q", got)
		}
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the symlink was replaced instead of its target edited")
		}
	})

	t.Run("symlinked directory", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.Mkdir(real, 0o750); err != nil {
			t.Fatal(err)
		}
		via := filepath.Join(base, "via")
		if err := os.Symlink(real, via); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
		if err := os.WriteFile(filepath.Join(real, "f.txt"), []byte("v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := saveFileFromEdit(nil, "", filepath.Join(via, "f.txt"), []byte("v2\n"), []byte("v1\n"), false); err != nil {
			t.Fatalf("save in a symlinked folder: %v", err)
		}
		if got, _ := os.ReadFile(filepath.Join(real, "f.txt")); string(got) != "v2\n" {
			t.Fatalf("content %q", got)
		}
	})
}

// TestEditorReportsLocalConflict: a file changed by someone else after it was
// opened is not overwritten; the footer says why and the edits stay unsaved.
func TestEditorReportsLocalConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := openInEditor(t, localPaneModel(dir, "f.txt"))
	m, _ = sendKey(m, keyTab)
	m, _ = sendKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if err := os.WriteFile(p, []byte("theirs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ = saveAndDeliver(t, m)
	if got, _ := os.ReadFile(p); string(got) != "theirs\n" {
		t.Fatalf("the other change was overwritten: %q", got)
	}
	if !strings.Contains(m.editor.status, "changed on disk") || !m.editor.dirty {
		t.Fatalf("want a conflict and unsaved edits, got status %q dirty=%v", m.editor.status, m.editor.dirty)
	}
}

// TestEditorLateSaveIgnoredAfterReopen: a save result that arrives after the
// editor was closed and another file (or the same file again) was opened must
// not touch the new session.
func TestEditorLateSaveIgnoredAfterReopen(t *testing.T) {
	t.Parallel()

	t.Run("other file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := openInEditor(t, localPaneModel(dir, "a.txt", "b.txt"))
		m, _ = sendKey(m, keyTab)
		m, _ = sendKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
		m, saveA := sendKey(m, keyCtrlS)
		if saveA == nil {
			t.Fatal("no save command")
		}
		m, _ = sendKey(m, keyEsc)
		m, _ = sendKey(m, keyEsc)
		if m.editor != nil {
			t.Fatal("editor should be closed")
		}
		m.left.index = 2 // b.txt
		m = openInEditor(t, m)
		mm, _ := m.Update(saveA())
		m = mm.(filesModel)
		if m.editor == nil || m.editor.name != "b.txt" {
			t.Fatal("b.txt should still be open")
		}
		if m.editor.content != "B\n" {
			t.Fatalf("late save of a.txt replaced b.txt's base with %q", m.editor.content)
		}
		if strings.Contains(m.editor.status, "saved") {
			t.Fatalf("b.txt shows a.txt's save result: %q", m.editor.status)
		}
		if !strings.Contains(m.status, "saved a.txt") {
			t.Fatalf("the late result should still be reported, status %q", m.status)
		}
	})

	t.Run("same file reopened", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, "a.txt")
		if err := os.WriteFile(p, []byte("A\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := openInEditor(t, localPaneModel(dir, "a.txt"))
		m, _ = sendKey(m, keyTab)
		m, _ = sendKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
		m, saveOld := sendKey(m, keyCtrlS)
		if saveOld == nil {
			t.Fatal("no save command")
		}
		m, _ = sendKey(m, keyEsc)
		m, _ = sendKey(m, keyEsc)
		// Reopen a.txt; the load sees the content from before the old save.
		m = openInEditor(t, m)
		// The old session's save now lands on disk and its result arrives.
		mm, _ := m.Update(saveOld())
		m = mm.(filesModel)
		if m.editor.content != "A\n" {
			t.Fatalf("late save from the closed session moved the new session's base to %q", m.editor.content)
		}
		// Saving the reopened (stale) text must now be refused, not silently
		// overwrite what the earlier session wrote.
		m, _ = sendKey(m, keyTab)
		m, _ = sendKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
		m, _ = saveAndDeliver(t, m)
		if got, _ := os.ReadFile(p); string(got) != "A\nx" {
			t.Fatalf("the earlier session's save was overwritten: %q", got)
		}
	})
}

// runOne runs a command and returns its message.
func runOne(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	return cmd()
}

// TestRenameRefusesHiddenTarget: renaming onto a name that exists but is not
// listed (hidden files off) must not replace the existing file.
func TestRenameRefusesHiddenTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := localPaneModel(dir, "a.txt") // .env is hidden from the listing
	m = m.openRenamePrompt(0)
	m.promptValue = ".env"
	mm, cmd := m.submitPrompt()
	m = mm.(filesModel)
	if msg := runOne(t, cmd); msg != nil {
		mm, _ = m.Update(msg)
		m = mm.(filesModel)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".env")); string(got) != "SECRET=1\n" {
		t.Fatalf("rename replaced the hidden .env: %q", got)
	}
	if !strings.Contains(m.status, "already exists") {
		t.Fatalf("status should say the name is taken, got %q", m.status)
	}
}

// TestRenameCaseOnlyStillWorks: on a case-insensitive filesystem the target
// "A.txt" resolves to the source "a.txt"; that is a rename, not a clobber.
func TestRenameCaseOnlyStillWorks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := localPaneModel(dir, "a.txt")
	m = m.openRenamePrompt(0)
	m.promptValue = "A.txt"
	mm, cmd := m.submitPrompt()
	m = mm.(filesModel)
	msg, ok := runOne(t, cmd).(fileOpDoneMsg)
	if !ok || msg.err != nil {
		t.Fatalf("case-only rename failed: %#v (status %q)", msg, m.status)
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 1 || names[0].Name() != "A.txt" {
		t.Fatalf("after rename: %v", names)
	}
}

// TestDuplicateRefusesUnlistedName: Duplicate picked its "<name> copy" name
// from the filtered listing, so an existing copy hidden by the filter was
// replaced.
func TestDuplicateRefusesUnlistedName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a copy.txt"), []byte("KEEP\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := localPaneModel(dir, "a.txt", "a copy.txt")
	m.left.filter = "a.txt" // matches a.txt only
	m.reapplyPane(0)
	m.left.index = 1
	if it := m.focusedItem(0); it.name != "a.txt" {
		t.Fatalf("focused %q", it.name)
	}
	mm, cmd := m.duplicateFocused(0)
	m = mm.(filesModel)
	msg := runOne(t, cmd)
	if done, ok := msg.(fileOpDoneMsg); !ok || done.err != nil {
		t.Fatalf("duplicate: %#v", msg)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a copy.txt")); string(got) != "KEEP\n" {
		t.Fatalf("duplicate replaced the existing 'a copy.txt': %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "a copy 2.txt")); err != nil || string(got) != "A\n" {
		t.Fatalf("expected the copy at 'a copy 2.txt': %q, %v", got, err)
	}
}

// fakeLookup stands in for an agent's file.stat and file.list. Like the real
// agent, every failed stat looks the same, and the listing leaves out
// dotfiles unless asked for them.
type fakeLookup struct {
	dirs            map[string][]string
	caseInsensitive bool
	listErr         error
}

func (f *fakeLookup) match(a, b string) bool {
	return a == b || (f.caseInsensitive && strings.EqualFold(a, b))
}

func (f *fakeLookup) StatRemoteFile(_, p string) (proto.FileStatResult, error) {
	for _, n := range f.dirs[path.Dir(p)] {
		if f.match(n, path.Base(p)) {
			return proto.FileStatResult{Entry: proto.FileEntry{Name: path.Base(p), Path: p}}, nil
		}
	}
	return proto.FileStatResult{}, errors.New("stat_failed: lstat " + p + ": no such file or directory")
}

func (f *fakeLookup) ListRemoteDirHidden(_, dir string, showHidden bool) (proto.FileListResult, error) {
	if f.listErr != nil {
		return proto.FileListResult{}, f.listErr
	}
	res := proto.FileListResult{Path: dir}
	for _, n := range f.dirs[dir] {
		if showHidden || !strings.HasPrefix(n, ".") {
			res.Entries = append(res.Entries, proto.FileEntry{Name: n, Path: path.Join(dir, n)})
		}
	}
	return res, nil
}

// TestRemoteCreateRefusesUnlistedName drives N, new folder and rename on a
// remote pane whose listing hides the existing name. The controller is nil:
// had any of them gone ahead to upload, mkdir or rename, the command would
// panic instead of returning the refusal.
func TestRemoteCreateRefusesUnlistedName(t *testing.T) {
	// Not parallel: it swaps the package-level lookupFor.
	fake := &fakeLookup{dirs: map[string][]string{"/srv": {".env", ".cache", "app.conf"}}}
	prev := lookupFor
	lookupFor = func(*core.App) remoteLookup { return fake }
	t.Cleanup(func() { lookupFor = prev })

	base := sampleFilesModel(120, 40)
	base.right.cwd, base.right.listedCwd = "/srv", "/srv"
	base.right.pathStyle = core.TargetPathPOSIX
	base.right.allItems = []fileItem{{name: "app.conf"}} // hidden files off
	base.reapplyPane(1)
	base.right.index = 1
	base.focus = 1

	run := func(t *testing.T, m filesModel, cmd tea.Cmd) (msg tea.Msg) {
		t.Helper()
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("the operation went ahead instead of refusing: %v", p)
			}
		}()
		return runOne(t, cmd)
	}
	for _, tc := range []struct {
		name  string
		open  func(filesModel) filesModel
		value string
	}{
		{"new file over .env", func(m filesModel) filesModel { return m.openNewFilePrompt(1) }, ".env"},
		{"new folder over .cache", func(m filesModel) filesModel { return m.openNewFolderPrompt(1) }, ".cache"},
		{"rename onto .env", func(m filesModel) filesModel { return m.openRenamePrompt(1) }, ".env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.open(base)
			m.promptValue = tc.value
			mm, cmd := m.submitPrompt()
			m = mm.(filesModel)
			msg := run(t, m, cmd)
			done, ok := msg.(fileOpDoneMsg)
			var taken *nameTakenError
			if !ok || !errors.As(done.err, &taken) {
				t.Fatalf("want a name-taken refusal, got %#v", msg)
			}
			mm, _ = m.Update(done)
			m = mm.(filesModel)
			if !strings.Contains(m.status, tc.value+" already exists") {
				t.Fatalf("status %q", m.status)
			}
		})
	}
}

// TestRemoteNameChecks covers the remote check's edges directly.
func TestRemoteNameChecks(t *testing.T) {
	t.Parallel()
	const srv = "web-01"
	posix := core.TargetPathPOSIX

	free := &fakeLookup{dirs: map[string][]string{"/srv": {".env"}}}
	if err := ensureNameFree(free, srv, posix, "/srv", "new.txt", ""); err != nil {
		t.Fatalf("a free name was refused: %v", err)
	}

	broken := &fakeLookup{listErr: errors.New("list_failed: permission denied")}
	err := ensureNameFree(broken, srv, posix, "/srv", "new.txt", "")
	var taken *nameTakenError
	if err == nil || errors.As(err, &taken) || !strings.Contains(err.Error(), "could not check") {
		t.Fatalf("a failed listing must refuse as unchecked, got %v", err)
	}

	ci := &fakeLookup{dirs: map[string][]string{"/srv": {"Readme.md", "a.txt"}}, caseInsensitive: true}
	if err := ensureNameFree(ci, srv, posix, "/srv", "README.md", ""); !errors.As(err, &taken) {
		t.Fatalf("a name the server folds onto an existing one must be refused, got %v", err)
	}
	if err := ensureNameFree(ci, srv, posix, "/srv", "A.txt", "a.txt"); err != nil {
		t.Fatalf("a case-only rename was refused: %v", err)
	}

	// The fake's stat cannot split a Windows path, so the listing decides.
	win := &fakeLookup{dirs: map[string][]string{`C:\app`: {"Config.ini"}}}
	if err := ensureNameFree(win, srv, core.TargetPathWindows, `C:\app`, "config.INI", ""); !errors.As(err, &taken) {
		t.Fatalf("Windows targets compare names without case, got %v", err)
	}

	dup := &fakeLookup{dirs: map[string][]string{"/srv": {"a.txt", ".a.txt", "a copy.txt"}}}
	if got, err := pickDuplicateName(dup, srv, posix, "/srv", "a.txt"); err != nil || got != "a copy 2.txt" {
		t.Fatalf("duplicate name = %q, %v; want \"a copy 2.txt\"", got, err)
	}
	dupCI := &fakeLookup{dirs: map[string][]string{"/srv": {"a.txt", "A Copy.txt"}}, caseInsensitive: true}
	if got, err := pickDuplicateName(dupCI, srv, posix, "/srv", "a.txt"); err != nil || got != "a copy 2.txt" {
		t.Fatalf("duplicate name on a case-insensitive server = %q, %v", got, err)
	}
}
