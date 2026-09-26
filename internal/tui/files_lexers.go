// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"embed"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
)

// ============================================================================
// Lazily-built syntax lexers
//
// Importing github.com/alecthomas/chroma/v2/lexers registers ~280 lexers in a
// package initialiser: it parses the header of every embedded XML definition
// and compiles Go-coded lexers at process start. Because the fleet binary
// links this package, that cost (≈7 ms and ≈2.7 MB of allocations) was paid
// by every fleet command, even `fleet version`.
//
// Instead, the file manager embeds only the definitions it needs (copied
// verbatim from chroma v2.27.0 under its MIT licence — see
// files_lexers/LICENSE-chroma) and builds a private registry the first time
// something is highlighted. Rules are compiled lazily per lexer by chroma
// itself, so opening a Go file compiles only the Go rules.
// ============================================================================

//go:embed files_lexers/*.xml
var fmLexerFS embed.FS

var (
	fmLexOnce sync.Once
	fmLexReg  *chroma.LexerRegistry
	fmLexFall chroma.Lexer
)

// fmLexers returns the file manager's lexer registry, building it on first use.
func fmLexers() *chroma.LexerRegistry {
	fmLexOnce.Do(func() {
		reg := chroma.NewLexerRegistry()
		paths, _ := fs.Glob(fmLexerFS, "files_lexers/*.xml")
		for _, p := range paths {
			if l, err := chroma.NewXMLLexer(fmLexerFS, p); err == nil {
				reg.Register(l)
			}
		}
		if tpl, err := chroma.NewXMLLexer(fmLexerFS, "files_lexers/go_template.xml"); err == nil {
			tpl.SetConfig(&chroma.Config{Name: "Go Text Template", Aliases: []string{"go-text-template"}})
			reg.Register(tpl)
			reg.Register(newGoLexer(tpl))
		}
		reg.Register(&fmMarkdownLexer{Lexer: chroma.MustNewLexer(&chroma.Config{
			Name:      "markdown",
			Aliases:   []string{"md", "mkd"},
			Filenames: []string{"*.md", "*.mkd", "*.markdown"},
			MimeTypes: []string{"text/x-markdown"},
		}, markdownRules), reg: reg})
		fmLexFall = chroma.MustNewLexer(&chroma.Config{
			Name:      "fallback",
			Filenames: []string{"*"},
			Priority:  -1,
		}, plaintextRules)
		fmLexReg = reg
	})
	return fmLexReg
}

// fmExtAliases maps names chroma's globs do not cover (or that belong to a
// lexer we do not ship) onto a close relative that we do.
var fmExtAliases = map[string]string{
	".scss": "CSS", ".sass": "CSS", ".less": "CSS",
	".jsx": "JavaScript", ".mts": "TypeScript", ".cts": "TypeScript",
	".cjs": "JavaScript", ".vue": "HTML", ".svelte": "HTML",
	".conf": "", ".cnf": "INI", ".env": "Bash", ".service": "SYSTEMD",
	".kts": "Java", ".kt": "Java", ".gradle": "Java", ".groovy": "Java",
	".tpl": "Go Text Template", ".tmpl": "Go Text Template",
	".jsonc": "JSON", ".json5": "JSON", ".geojson": "JSON",
	".sls": "YAML", ".j2": "", ".log": "",
}

// pickLexer selects a lexer by filename, falling back to content analysis,
// then to a plain-text lexer.
func pickLexer(filename, content string) chroma.Lexer {
	reg := fmLexers()
	base := filepath.Base(filename)
	lower := strings.ToLower(base)
	switch {
	case lower == "go.mod" || lower == "go.work":
		return reg.Get("Go")
	case strings.HasPrefix(lower, "dockerfile") || strings.HasSuffix(lower, ".dockerfile") || lower == "containerfile":
		return reg.Get("Docker")
	case lower == "jenkinsfile":
		return reg.Get("Java")
	}
	if l := reg.Match(base); l != nil {
		return l
	}
	ext := strings.ToLower(filepath.Ext(base))
	if name, ok := fmExtAliases[ext]; ok && name != "" {
		if l := reg.Get(name); l != nil {
			return l
		}
	}
	if ext == ".conf" {
		// nginx and Apache configs both use .conf; pick by content.
		if strings.Contains(content, "server {") || strings.Contains(content, "location ") ||
			strings.Contains(content, "upstream ") || strings.Contains(content, "http {") {
			return reg.Get("Nginx configuration file")
		}
		if strings.Contains(content, "<VirtualHost") || strings.Contains(content, "<Directory") {
			return reg.Get("ApacheConf")
		}
		return reg.Get("INI")
	}
	if l := reg.Analyse(content); l != nil {
		return l
	}
	return fmLexFall
}

func plaintextRules() chroma.Rules {
	return chroma.Rules{
		"root": []chroma.Rule{
			{Pattern: `.+`, Type: chroma.Text},
			{Pattern: `\n`, Type: chroma.Text},
		},
	}
}

// ---- Go (ported from chroma/lexers/go.go, MIT) ----

func newGoLexer(textTemplate chroma.Lexer) chroma.Lexer {
	return chroma.MustNewLexer(
		&chroma.Config{
			Name:      "Go",
			Aliases:   []string{"go", "golang"},
			Filenames: []string{"*.go"},
			MimeTypes: []string{"text/x-gosrc"},
		},
		func() chroma.Rules { return goRules(textTemplate) },
	).SetAnalyser(func(text string) float32 {
		if strings.Contains(text, "fmt.") && strings.Contains(text, "package ") {
			return 0.5
		}
		if strings.Contains(text, "package ") {
			return 0.1
		}
		return 0.0
	})
}

func goRules(textTemplate chroma.Lexer) chroma.Rules {
	type R = chroma.Rule
	W := chroma.Words
	return chroma.Rules{
		"root": {
			R{Pattern: `\n`, Type: chroma.TextWhitespace},
			R{Pattern: `\s+`, Type: chroma.TextWhitespace},
			R{Pattern: `//[^\s\n\r][^\n\r]*`, Type: chroma.CommentPreproc},
			R{Pattern: `//[^\n\r]*`, Type: chroma.CommentSingle},
			R{Pattern: `/(\\\n)?[*](.|\n)*?[*](\\\n)?/`, Type: chroma.CommentMultiline},
			R{Pattern: `(import|package)\b`, Type: chroma.KeywordNamespace},
			R{Pattern: `(var|func|struct|map|chan|type|interface|const)\b`, Type: chroma.KeywordDeclaration},
			R{Pattern: W(``, `\b`, `break`, `default`, `select`, `case`, `defer`, `go`, `else`, `goto`, `switch`, `fallthrough`, `if`, `range`, `continue`, `for`, `return`), Type: chroma.Keyword},
			R{Pattern: `(true|false|iota|nil)\b`, Type: chroma.KeywordConstant},
			R{Pattern: W(``, `\b(\()`, `uint`, `uint8`, `uint16`, `uint32`, `uint64`, `int`, `int8`, `int16`, `int32`, `int64`, `float`, `float32`, `float64`, `complex64`, `complex128`, `byte`, `rune`, `string`, `bool`, `error`, `uintptr`, `print`, `println`, `panic`, `recover`, `close`, `complex`, `real`, `imag`, `len`, `cap`, `append`, `copy`, `delete`, `new`, `make`, `clear`, `min`, `max`), Type: chroma.ByGroups(chroma.NameBuiltin, chroma.Punctuation)},
			R{Pattern: W(``, `\b`, `uint`, `uint8`, `uint16`, `uint32`, `uint64`, `int`, `int8`, `int16`, `int32`, `int64`, `float`, `float32`, `float64`, `complex64`, `complex128`, `byte`, `rune`, `string`, `bool`, `error`, `uintptr`, `any`), Type: chroma.KeywordType},
			R{Pattern: `\d+i`, Type: chroma.LiteralNumber},
			R{Pattern: `\d+\.\d*([Ee][-+]\d+)?i`, Type: chroma.LiteralNumber},
			R{Pattern: `\.\d+([Ee][-+]\d+)?i`, Type: chroma.LiteralNumber},
			R{Pattern: `\d+[Ee][-+]\d+i`, Type: chroma.LiteralNumber},
			R{Pattern: `\d+(\.\d+[eE][+\-]?\d+|\.\d*|[eE][+\-]?\d+)`, Type: chroma.LiteralNumberFloat},
			R{Pattern: `\.\d+([eE][+\-]?\d+)?`, Type: chroma.LiteralNumberFloat},
			R{Pattern: `0[0-7]+`, Type: chroma.LiteralNumberOct},
			R{Pattern: `0[xX][0-9a-fA-F_]+`, Type: chroma.LiteralNumberHex},
			R{Pattern: `0b[01_]+`, Type: chroma.LiteralNumberBin},
			R{Pattern: `(0|[1-9][0-9_]*)`, Type: chroma.LiteralNumberInteger},
			R{Pattern: `'(\\['"\\abfnrtv]|\\x[0-9a-fA-F]{2}|\\[0-7]{1,3}|\\u[0-9a-fA-F]{4}|\\U[0-9a-fA-F]{8}|[^\\])'`, Type: chroma.LiteralStringChar},
			R{Pattern: "(`)([^`]*)(`)", Type: chroma.ByGroups(chroma.LiteralString,
				chroma.UsingLexer(chroma.TypeRemappingLexer(textTemplate, chroma.TypeMapping{{From: chroma.Other, To: chroma.LiteralString}})),
				chroma.LiteralString)},
			R{Pattern: `"(\\\\|\\"|[^"])*"`, Type: chroma.LiteralString},
			R{Pattern: `(<<=|>>=|<<|>>|<=|>=|&\^=|&\^|\+=|-=|\*=|/=|%=|&=|\|=|&&|\|\||<-|\+\+|--|==|!=|:=|\.\.\.|[+\-*/%&])`, Type: chroma.Operator},
			R{Pattern: `([a-zA-Z_]\w*)(\s*)(\()`, Type: chroma.ByGroups(chroma.NameFunction, chroma.UsingSelf("root"), chroma.Punctuation)},
			R{Pattern: `[|^<>=!()\[\]{}.,;:~]`, Type: chroma.Punctuation},
			R{Pattern: `[^\W\d]\w*`, Type: chroma.NameOther},
		},
	}
}

// ---- Markdown (ported from chroma/lexers/markdown.go, MIT) ----

// fmMarkdownLexer highlights a leading YAML front-matter block with the YAML
// lexer from our registry, then the rest as Markdown.
type fmMarkdownLexer struct {
	chroma.Lexer
	reg *chroma.LexerRegistry
}

func (m *fmMarkdownLexer) Tokenise(options *chroma.TokeniseOptions, text string) (chroma.Iterator, error) {
	front, rest, ok := splitFrontmatter(text)
	if !ok {
		return m.Lexer.Tokenise(options, text)
	}
	yaml := m.reg.Get("YAML")
	if yaml == nil {
		return m.Lexer.Tokenise(options, text)
	}
	yt, err := yaml.Tokenise(options, front)
	if err != nil {
		return nil, err
	}
	mt, err := m.Lexer.Tokenise(options, rest)
	if err != nil {
		return nil, err
	}
	return chroma.Concaterator(yt, mt), nil
}

func splitFrontmatter(text string) (front, rest string, ok bool) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return "", text, false
	}
	lineEnd := strings.IndexByte(text, '\n')
	if lineEnd < 0 || strings.TrimSuffix(text[:lineEnd], "\r") != "---" {
		return "", text, false
	}
	for pos := lineEnd + 1; pos < len(text); {
		next := strings.IndexByte(text[pos:], '\n')
		if next < 0 {
			break
		}
		lineEnd = pos + next
		if strings.TrimSuffix(text[pos:lineEnd], "\r") == "---" {
			return text[:lineEnd+1], text[lineEnd+1:], true
		}
		pos = lineEnd + 1
	}
	return "", text, false
}

func markdownRules() chroma.Rules {
	type R = chroma.Rule
	BG := chroma.ByGroups
	return chroma.Rules{
		"root": {
			R{Pattern: `<!--[\w\W]*?-->`, Type: chroma.CommentMultiline},
			R{Pattern: `^(#[^#].+\n)`, Type: BG(chroma.GenericHeading)},
			R{Pattern: `^(#{2,6}.+\n)`, Type: BG(chroma.GenericSubheading)},
			R{Pattern: `^(\s*)([*-] )(\[[ xX]\])( .+\n)`, Type: BG(chroma.Text, chroma.Keyword, chroma.Keyword, chroma.UsingSelf("inline"))},
			R{Pattern: `^(\s*)([*-])(\s)(.+\n)`, Type: BG(chroma.Text, chroma.Keyword, chroma.Text, chroma.UsingSelf("inline"))},
			R{Pattern: `^(\s*)([0-9]+\.)( .+\n)`, Type: BG(chroma.Text, chroma.Keyword, chroma.UsingSelf("inline"))},
			R{Pattern: `^(\s*>\s)(.+\n)`, Type: BG(chroma.Keyword, chroma.GenericEmph)},
			R{Pattern: "^(```\\n)([\\w\\W]*?)(^```$)", Type: BG(chroma.String, chroma.Text, chroma.String)},
			R{Pattern: "^(```)(\\w+)(\\n)([\\w\\W]*?)(^```$)",
				Type: chroma.UsingByGroup(2, 4, chroma.String, chroma.String, chroma.String, chroma.Text, chroma.String)},
			chroma.Include("inline"),
		},
		"inline": {
			R{Pattern: `<!--[\w\W]*?-->`, Type: chroma.CommentMultiline},
			R{Pattern: `\\.`, Type: chroma.Text},
			R{Pattern: `(\s)(\*|_)((?:(?!\2).)*)(\2)((?=\W|\n))`, Type: BG(chroma.Text, chroma.GenericEmph, chroma.GenericEmph, chroma.GenericEmph, chroma.Text)},
			R{Pattern: `(\s)((\*\*|__).*?)\3((?=\W|\n))`, Type: BG(chroma.Text, chroma.GenericStrong, chroma.GenericStrong, chroma.Text)},
			R{Pattern: `(\s)(~~[^~]+~~)((?=\W|\n))`, Type: BG(chroma.Text, chroma.GenericDeleted, chroma.Text)},
			R{Pattern: "`[^`]+`", Type: chroma.LiteralStringBacktick},
			R{Pattern: `[@#][\w/:]+`, Type: chroma.NameEntity},
			R{Pattern: `(!?\[)([^]]+)(\])(\()([^)]+)(\))`, Type: BG(chroma.Text, chroma.NameTag, chroma.Text, chroma.Text, chroma.NameAttribute, chroma.Text)},
			R{Pattern: `.|\n`, Type: chroma.Text},
		},
	}
}
