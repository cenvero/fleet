// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func relLuminance(r, g, b int) float64 {
	lin := func(c int) float64 {
		v := float64(c) / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrast(a, b [3]int) float64 {
	la, lb := relLuminance(a[0], a[1], a[2]), relLuminance(b[0], b[1], b[2])
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

var sgr256 = regexp.MustCompile(`(38|48);5;(\d+)`)

// colours256 extracts the fg/bg xterm-256 colours of a style's SGR prefix.
func colours256(t *testing.T, on string) (fg, bg [3]int, hasFG, hasBG bool) {
	t.Helper()
	for _, m := range sgr256.FindAllStringSubmatch(on, -1) {
		idx, _ := strconv.Atoi(m[2])
		r, g, b := dashXterm256(idx)
		if m[1] == "38" {
			fg, hasFG = [3]int{r, g, b}, true
		} else {
			bg, hasBG = [3]int{r, g, b}, true
		}
	}
	return
}

// Regression: termenv quantised #e7ecef (header text, pageStyle) to index 232,
// near-black, so text vanished on TERM=xterm-256color. Every palette colour
// must now land close to its intended value, and text must stay readable.
func TestPalette256ColoursStayFaithfulAndReadable(t *testing.T) {
	for _, dark := range []bool{true, false} {
		shade := 0
		termBG := [3]int{0, 0, 0}
		if !dark {
			shade = 1
			termBG = [3]int{255, 255, 255}
		}
		p := dashBuildPalette(termenv.ANSI256, dark)
		for i, spec := range dashStyleSpecs {
			for _, hex := range []string{spec.fg[shade], spec.bg[shade]} {
				if hex == "" {
					continue
				}
				r, g, b, _ := dashParseHex(hex)
				idx, _ := dashNearest256(hex)
				qr, qg, qb := dashXterm256(idx)
				d := math.Sqrt(float64((r-qr)*(r-qr) + (g-qg)*(g-qg) + (b-qb)*(b-qb)))
				if d > 48 {
					t.Errorf("dark=%v style %d: %s quantises to #%02x%02x%02x (distance %.0f)", dark, i, hex, qr, qg, qb, d)
				}
			}
			fg, bg, hasFG, hasBG := colours256(t, p.on[i])
			if !hasFG {
				continue
			}
			against := termBG
			if hasBG {
				against = bg
			}
			min := 3.0
			switch dstyle(i) {
			case sHdr, sSel, sTabActive, sPrompt, sHdrBrand:
				min = 4.5 // body text on a chip or bar
			case sBorder, sTrack, sDim, sTabCount, sColHead, sSelBlur:
				min = 1.5 // decoration, not text
			}
			if c := contrast(fg, against); c < min {
				t.Errorf("dark=%v style %d: contrast %.2f < %.1f (%q)", dark, i, c, min, p.on[i])
			}
		}
	}
	// The header text specifically must be light on the dark theme.
	fg, _, _, _ := colours256(t, dashBuildPalette(termenv.ANSI256, true).on[sHdr])
	if relLuminance(fg[0], fg[1], fg[2]) < 0.5 {
		t.Fatalf("header text quantised to a dark colour %v", fg)
	}
}

func TestPageStyleSurvives256Colours(t *testing.T) {
	fg, ok := pageStyle.GetForeground().(lipgloss.Color)
	if !ok {
		t.Fatalf("pageStyle foreground is %T", pageStyle.GetForeground())
	}
	col, ok := termenv.ANSI256.Color(string(fg)).(termenv.ANSI256Color)
	if !ok {
		t.Fatalf("unexpected colour type")
	}
	r, g, b := dashXterm256(int(col))
	if int(col) < 16 || relLuminance(r, g, b) < 0.5 {
		t.Fatalf("pageStyle text %s becomes 256-colour index %d (#%02x%02x%02x): not readable on its dark page", fg, col, r, g, b)
	}
}

func TestPalette16ColoursUseReverseChipsAndNoTintedBars(t *testing.T) {
	p := dashBuildPalette(termenv.ANSI, true)
	for _, s := range []dstyle{sSel, sTabActive, sPrompt, sHdrBrand} {
		if !strings.Contains(p.on[s], ";7") && !strings.Contains(p.on[s], "[7") {
			t.Errorf("style %d should use reverse video on 16 colours: %q", s, p.on[s])
		}
	}
	bgSeq := regexp.MustCompile(`(^|;)(4[0-7]|10[0-7])(;|m)`)
	for _, s := range []dstyle{sHdr, sHdrMuted, sHdrOK, sSelBlur} {
		if bgSeq.MatchString(p.on[s]) {
			t.Errorf("style %d keeps a background on 16 colours: %q", s, p.on[s])
		}
	}
	if !strings.Contains(p.on[sSelBlur], "4") {
		t.Errorf("unfocused selection should fall back to an underline: %q", p.on[sSelBlur])
	}
}

// Under NO_COLOR the cursor row and the focused pane must stay identifiable.
func TestNoColorKeepsCursorAndFocusVisible(t *testing.T) {
	prev := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	t.Setenv("NO_COLOR", "1")
	lipgloss.SetColorProfile(termenv.Ascii)

	m := newTestDash(t, 12, 160, 45)
	for _, tab := range []string{"2", "3", "4", "5", "6"} {
		frame := press(m, tab).View()
		if !strings.Contains(frame, "\x1b[1;7m›") && !strings.Contains(frame, "\x1b[7m›") {
			t.Fatalf("tab %s: cursor row is not reverse video under NO_COLOR", tab)
		}
	}
	// Focusing the log viewer: the list cursor switches to an underline and
	// the viewer's title turns reverse video.
	m = press(m, "4", "enter")
	frame := m.View()
	if !strings.Contains(frame, "\x1b[4m›") {
		t.Fatalf("unfocused list cursor should be underlined")
	}
	lp := m.selectedLog()
	if !strings.Contains(frame, "\x1b[1;7m"+lp.Server+" / "+lp.Service) {
		t.Fatalf("focused viewer title should be reverse video")
	}
	// The filter caret is a glyph, visible without colour.
	m = press(m, "esc", "2", "/")
	if footer := plainLines(m.View())[44]; !strings.Contains(footer, "▏") {
		t.Fatalf("filter caret missing: %q", footer)
	}
}
