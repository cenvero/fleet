// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/lipgloss"
)

// Syntax-highlight palette: chroma token *categories* mapped onto the existing
// file-manager palette so highlighted code feels native to the app rather than
// like a foreign theme. We deliberately colour by category (Keyword, Name,
// Literal, Comment, …) — not by every fine-grained token type — for a clean,
// readable result with the teal/blue accent family already in use.
var (
	hlKeyword = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff9d6b")).Bold(true) // warm orange
	hlName    = lipgloss.NewStyle().Foreground(fmText)
	hlNameFn  = lipgloss.NewStyle().Foreground(fmAccent2)                 // teal accent
	hlNameCls = lipgloss.NewStyle().Foreground(lipgloss.Color("#7ad7ff")) // blue (types)
	hlNameBlt = lipgloss.NewStyle().Foreground(lipgloss.Color("#c8a8ff")) // lavender (builtins)
	hlString  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7ee787")) // green
	hlNumber  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f0a8d0")) // pink
	hlComment = lipgloss.NewStyle().Foreground(fmDimC).Italic(true)       // dim, italic
	hlOperat  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8fd0c8")) // muted teal
	hlPunct   = lipgloss.NewStyle().Foreground(fmMutedC)                  // muted
	hlPreproc = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffce6b")) // amber
	hlError   = lipgloss.NewStyle().Foreground(fmDangerC)                 // red
	hlText    = lipgloss.NewStyle().Foreground(fmText)
)

// styleForToken picks a lipgloss style for a chroma token by its category. Using
// Category() collapses the hundreds of token types into a handful of buckets we
// have palette colours for.
func styleForToken(t chroma.TokenType) lipgloss.Style {
	// A few sub-types deserve their own colour for readability.
	switch t {
	case chroma.Error:
		return hlError
	case chroma.NameFunction, chroma.NameFunctionMagic:
		return hlNameFn
	case chroma.NameClass, chroma.NameNamespace, chroma.KeywordType:
		return hlNameCls
	case chroma.NameBuiltin, chroma.NameBuiltinPseudo, chroma.NameException:
		return hlNameBlt
	}
	// Preprocessor/comment-directives get their own colour (they share the
	// Comment category, so this must be checked before the category switch).
	if t.InSubCategory(chroma.CommentPreproc) {
		return hlPreproc
	}
	switch t.Category() {
	case chroma.Keyword:
		return hlKeyword
	case chroma.Name:
		return hlName
	case chroma.Literal:
		// Strings vs numbers split by sub-category (both live in the Literal
		// category, so Category() alone can't tell them apart).
		if t.InSubCategory(chroma.LiteralNumber) {
			return hlNumber
		}
		return hlString
	case chroma.Operator:
		return hlOperat
	case chroma.Punctuation:
		return hlPunct
	case chroma.Comment:
		return hlComment
	case chroma.Generic:
		return hlText
	default:
		return hlText
	}
}

// highlightLines tokenises content with the lexer chosen for filename and returns
// the source split into lines, each already coloured with ANSI/lipgloss styling.
// On any tokenisation failure it falls back to plain (uncoloured) lines so the
// editor still shows the file. The returned slice always has at least one entry.
func highlightLines(filename, content string) []string {
	lexer := pickLexer(filename, content)
	if lexer == nil {
		return splitPlain(content)
	}
	lexer = chroma.Coalesce(lexer)
	iter, err := lexer.Tokenise(nil, content)
	if err != nil {
		return splitPlain(content)
	}

	var lines []string
	var b strings.Builder
	flush := func() {
		lines = append(lines, b.String())
		b.Reset()
	}
	for _, tok := range iter.Tokens() {
		style := styleForToken(tok.Type)
		// A token's value may span several lines; style each fragment and break on
		// newlines so every output line carries complete styling.
		parts := strings.Split(tok.Value, "\n")
		for i, part := range parts {
			if i > 0 {
				flush()
			}
			if part != "" {
				b.WriteString(style.Render(part))
			}
		}
	}
	flush()
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// pickLexer selects a chroma lexer by filename, falling back to content analysis,
// then to the plain-text fallback lexer.
func pickLexer(filename, content string) chroma.Lexer {
	if l := lexers.Match(filename); l != nil {
		return l
	}
	if l := lexers.Analyse(content); l != nil {
		return l
	}
	return lexers.Fallback
}

// splitPlain returns content split into lines styled with the default text
// colour, so the viewer looks consistent even without highlighting.
func splitPlain(content string) []string {
	raw := strings.Split(content, "\n")
	out := make([]string, len(raw))
	for i, l := range raw {
		if l == "" {
			out[i] = ""
			continue
		}
		out[i] = hlText.Render(l)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}
