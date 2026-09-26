// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestLazyLexersCoverCommonFiles checks that every file type the file manager
// promises to highlight resolves to the right lexer from the embedded,
// lazily-built registry (not the plain-text fallback).
func TestLazyLexersCoverCommonFiles(t *testing.T) {
	cases := map[string]string{
		"main.go":            "Go",
		"go.mod":             "Go",
		"app.py":             "Python",
		"app.js":             "JavaScript",
		"app.mjs":            "JavaScript",
		"app.ts":             "TypeScript",
		"app.tsx":            "TypeScript",
		"config.json":        "JSON",
		"config.yaml":        "YAML",
		"compose.yml":        "YAML",
		"Cargo.toml":         "TOML",
		"deploy.sh":          "Bash",
		".bashrc":            "Bash",
		"README.md":          "markdown",
		"index.html":         "HTML",
		"style.css":          "CSS",
		"theme.scss":         "CSS",
		"query.sql":          "SQL",
		"Dockerfile":         "Docker",
		"Dockerfile.prod":    "Docker",
		"nginx.conf":         "Nginx configuration file",
		"settings.ini":       "INI",
		"setup.cfg":          "INI",
		"fleet.service":      "SYSTEMD",
		"Makefile":           "Makefile",
		"pom.xml":            "XML",
		"fix.diff":           "Diff",
		"main.rs":            "Rust",
		"main.c":             "C",
		"main.cpp":           "C++",
		"Main.java":          "Java",
		"Gemfile":            "Ruby",
		"init.lua":           "Lua",
		"app.properties":     "properties",
		"main.tf":            "Terraform",
		"api.proto":          "Protocol Buffer",
		"setup.ps1":          "PowerShell",
		"index.php":          "PHP",
		"script.pl":          "Perl",
		"data.csv":           "CSV",
		"Program.cs":         "C#",
		"page.gotmpl":        "Go Template",
		"notes.txt":          "plaintext",
		"site.conf":          "INI",
		"httpd-vhosts.conf":  "INI",
		"upstream-site.conf": "Nginx configuration file",
	}
	contentFor := map[string]string{
		"upstream-site.conf": "upstream app { server 127.0.0.1:8080; }\nserver {\n listen 80;\n}\n",
	}
	for name, want := range cases {
		l := pickLexer(name, contentFor[name])
		if l == nil {
			t.Fatalf("%s: no lexer", name)
		}
		if got := l.Config().Name; got != want {
			t.Errorf("%s: lexer = %q, want %q", name, got, want)
		}
	}
}

// TestHighlightUsesSeveralStyles makes sure highlighting still distinguishes
// token categories for common languages (not just one colour for everything).
func TestHighlightUsesSeveralStyles(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	samples := map[string]string{
		"main.go":     "package main\n\n// hi\nfunc main() { x := \"s\"; _ = 42 }\n",
		"app.py":      "def f(x):\n    # c\n    return \"s\" + str(1)\n",
		"app.ts":      "interface A { b: number }\nconst c: A = { b: 1 }; // x\n",
		"c.json":      "{\"a\": 1, \"b\": true, \"c\": \"s\"}\n",
		"c.yaml":      "a: 1\nb: \"s\"\n# c\n",
		"c.toml":      "[s]\na = 1\nb = \"x\"\n",
		"d.sh":        "#!/bin/sh\necho \"hi\" # c\nif [ -f x ]; then exit 1; fi\n",
		"README.md":   "# Title\n\nSome *em* and `code`.\n\n```go\nfunc x() {}\n```\n",
		"i.html":      "<html><body class=\"x\"><script>var a = 1;</script></body></html>\n",
		"s.css":       "body { color: #fff; margin: 0 auto; }\n",
		"q.sql":       "SELECT id FROM users WHERE a = 1; -- c\n",
		"Dockerfile":  "FROM golang:1.26\nRUN go build ./...\nCMD [\"./app\"]\n",
		"nginx.conf":  "server {\n    listen 80;\n    location / { proxy_pass http://x; }\n}\n",
		"s.ini":       "[core]\nkey = value\n; comment\n",
		"Makefile":    "all:\n\tgo build ./...\n",
		"config.json": "{\n  \"nested\": {\"ok\": null}\n}\n",
	}
	for name, src := range samples {
		lines := highlightLines(name, src)
		styles := map[string]bool{}
		for _, ln := range lines {
			for _, part := range strings.Split(ln, "\x1b[") {
				if i := strings.IndexByte(part, 'm'); i > 0 && part[:i] != "0" {
					styles[part[:i]] = true
				}
			}
		}
		if len(styles) < 2 {
			t.Errorf("%s: only %d distinct styles in highlighted output", name, len(styles))
		}
		if got := stripANSI(strings.Join(lines, "\n")); strings.TrimRight(got, "\n") != strings.TrimRight(normaliseForDisplay(src), "\n") {
			t.Errorf("%s: highlighting changed the text:\n%q\nwant\n%q", name, got, src)
		}
	}
}

// TestHighlightNeutralisesControlCharacters is a regression test: file
// contents are untrusted, and an ESC sequence inside a text file must not be
// forwarded to the terminal by the viewer or the preview.
func TestHighlightNeutralisesControlCharacters(t *testing.T) {
	src := "echo hi\x1b[2J\x1b]52;c;cHduZWQ=\x07 done\r\nnext\tline\n"
	for _, name := range []string{"x.sh", "x.unknownext"} {
		out := strings.Join(highlightLines(name, src), "\n")
		plain := stripANSI(out)
		if strings.ContainsAny(plain, "\x1b\x07\r\t") {
			t.Fatalf("%s: control characters leaked into output: %q", name, plain)
		}
		if !strings.Contains(plain, "␛[2J") {
			t.Fatalf("%s: escape should be shown as a visible placeholder: %q", name, plain)
		}
		if !strings.Contains(plain, "next    line") {
			t.Fatalf("%s: tabs should expand to 4 columns: %q", name, plain)
		}
	}
}
