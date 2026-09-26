// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"strings"
	"sync/atomic"

	"github.com/alecthomas/chroma/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Syntax-highlight palette: chroma token *categories* mapped onto the existing
// file-manager palette so highlighted code feels native to the app rather than
// like a foreign theme. We deliberately colour by category (Keyword, Name,
// Literal, Comment, …) — not by every fine-grained token type — for a clean,
// readable result with the teal/blue accent family already in use.
var (
	hlKeyword = lipgloss.NewStyle().Foreground(fmColor("#ff9d6b")).Bold(true) // warm orange
	hlName    = lipgloss.NewStyle().Foreground(fmText)
	hlNameFn  = lipgloss.NewStyle().Foreground(fmAccent2)           // teal accent
	hlNameCls = lipgloss.NewStyle().Foreground(fmColor("#7ad7ff"))  // blue (types)
	hlNameBlt = lipgloss.NewStyle().Foreground(fmColor("#c8a8ff"))  // lavender (builtins)
	hlString  = lipgloss.NewStyle().Foreground(fmColor("#7ee787"))  // green
	hlNumber  = lipgloss.NewStyle().Foreground(fmColor("#f0a8d0"))  // pink
	hlComment = lipgloss.NewStyle().Foreground(fmDimC).Italic(true) // dim, italic
	hlOperat  = lipgloss.NewStyle().Foreground(fmColor("#8fd0c8"))  // muted teal
	hlPunct   = lipgloss.NewStyle().Foreground(fmMutedC)            // muted
	hlPreproc = lipgloss.NewStyle().Foreground(fmColor("#ffce6b"))  // amber
	hlError   = lipgloss.NewStyle().Foreground(fmDangerC)           // red
	hlText    = lipgloss.NewStyle().Foreground(fmText)
)

// hlBucket maps a token type onto one of the palette buckets below.
func hlBucket(t chroma.TokenType) int {
	// A few sub-types deserve their own colour for readability.
	switch t {
	case chroma.Error:
		return hlBError
	case chroma.NameFunction, chroma.NameFunctionMagic:
		return hlBNameFn
	case chroma.NameClass, chroma.NameNamespace, chroma.KeywordType:
		return hlBNameCls
	case chroma.NameBuiltin, chroma.NameBuiltinPseudo, chroma.NameException:
		return hlBNameBlt
	}
	// Preprocessor/comment-directives get their own colour (they share the
	// Comment category, so this must be checked before the category switch).
	if t.InSubCategory(chroma.CommentPreproc) {
		return hlBPreproc
	}
	switch t.Category() {
	case chroma.Keyword:
		return hlBKeyword
	case chroma.Name:
		return hlBName
	case chroma.Literal:
		// Strings vs numbers split by sub-category (both live in the Literal
		// category, so Category() alone can't tell them apart).
		if t.InSubCategory(chroma.LiteralNumber) {
			return hlBNumber
		}
		return hlBString
	case chroma.Operator:
		return hlBOperat
	case chroma.Punctuation:
		return hlBPunct
	case chroma.Comment:
		return hlBComment
	}
	return hlBText
}

const (
	hlBText = iota
	hlBKeyword
	hlBName
	hlBNameFn
	hlBNameCls
	hlBNameBlt
	hlBString
	hlBNumber
	hlBComment
	hlBOperat
	hlBPunct
	hlBPreproc
	hlBError
	hlBCount
)

var hlStyles = [hlBCount]lipgloss.Style{
	hlBText: hlText, hlBKeyword: hlKeyword, hlBName: hlName, hlBNameFn: hlNameFn,
	hlBNameCls: hlNameCls, hlBNameBlt: hlNameBlt, hlBString: hlString, hlBNumber: hlNumber,
	hlBComment: hlComment, hlBOperat: hlOperat, hlBPunct: hlPunct, hlBPreproc: hlPreproc,
	hlBError: hlError,
}

// hlPaintSet caches the escape sequences of hlStyles for the current colour
// profile, so highlighting a large file does not pay lipgloss.Render per token.
type hlPaintSet struct {
	profile termenv.Profile
	p       [hlBCount]fmPaint
}

var hlPaintCache atomic.Pointer[hlPaintSet]

func hlPaints() *hlPaintSet {
	prof := lipgloss.ColorProfile()
	if c := hlPaintCache.Load(); c != nil && c.profile == prof {
		return c
	}
	c := &hlPaintSet{profile: prof}
	for i, st := range hlStyles {
		c.p[i] = fmPaintOf(st)
	}
	hlPaintCache.Store(c)
	return c
}

// highlightLines tokenises content with the lexer chosen for filename and returns
// the source split into lines, each already coloured with ANSI styling. On any
// tokenisation failure it falls back to plain (uncoloured) lines so the editor
// still shows the file. The returned slice always has at least one entry.
//
// Tabs are expanded, CRLF line ends normalised and any other control character
// replaced with a visible placeholder, so file contents can never emit raw
// escape sequences to the terminal.
func highlightLines(filename, content string) (lines []string) {
	content = normaliseForDisplay(content)
	defer func() {
		// chroma's emitters panic on some malformed lexer states; a viewer must
		// never take the program down.
		if r := recover(); r != nil {
			lines = splitPlain(content)
		}
	}()
	lexer := pickLexer(filename, content)
	if lexer == nil {
		return splitPlain(content)
	}
	lexer = chroma.Coalesce(lexer)
	iter, err := lexer.Tokenise(nil, content)
	if err != nil {
		return splitPlain(content)
	}

	paints := hlPaints()
	var b strings.Builder
	flush := func() {
		lines = append(lines, b.String())
		b.Reset()
	}
	for _, tok := range iter.Tokens() {
		pt := paints.p[hlBucket(tok.Type)]
		// A token's value may span several lines; style each fragment and break on
		// newlines so every output line carries complete styling.
		parts := strings.Split(tok.Value, "\n")
		for i, part := range parts {
			if i > 0 {
				flush()
			}
			if part != "" {
				b.WriteString(pt.s(fmSanitize(part)))
			}
		}
	}
	flush()
	// Tokenisers add a trailing newline for EnsureNL lexers; keep the line
	// count faithful to the source.
	if want := strings.Count(content, "\n") + 1; len(lines) > want {
		lines = lines[:want]
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// normaliseForDisplay expands tabs (4 columns) and strips CR from CRLF line
// endings, line by line.
func normaliseForDisplay(content string) string {
	if !strings.ContainsAny(content, "\t\r") {
		return content
	}
	raw := strings.Split(content, "\n")
	for i, l := range raw {
		l = strings.TrimSuffix(l, "\r")
		raw[i] = fmExpandTabs(l, 4)
	}
	return strings.Join(raw, "\n")
}

// splitPlain returns content split into lines styled with the default text
// colour, so the viewer looks consistent even without highlighting.
func splitPlain(content string) []string {
	raw := strings.Split(content, "\n")
	out := make([]string, len(raw))
	pt := hlPaints().p[hlBText]
	for i, l := range raw {
		if l == "" {
			out[i] = ""
			continue
		}
		out[i] = pt.s(fmSanitize(l))
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}
