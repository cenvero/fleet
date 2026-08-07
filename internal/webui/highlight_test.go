// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestHTMLXMLHighlighterEscapesMalformedTags(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	data, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	start := strings.Index(src, "function escHTML")
	end := strings.Index(src[start:], "// ----------------------------------------------------------------- editor")
	if start < 0 || end < 0 {
		t.Fatal("could not isolate highlighter functions")
	}
	highlighter := src[start : start+end]
	inputs := []string{
		`<div title='"><img src=x onerror=alert(1)>'>ok</div>`,
		`<svg><g data-x="</span><script>alert(1)</script>">`,
		`<!-- malformed <img src=x onerror=alert(1)>`,
		`<a href="x" <iframe srcdoc='<script>x</script>'>`,
	}
	payload, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	script := highlighter + "\nconst inputs=" + string(payload) + "; console.log(JSON.stringify(inputs.map(x => hlXML(x))));\n"
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("run highlighter: %v\n%s", err, out)
	}
	var highlighted []string
	if err := json.Unmarshal(out, &highlighted); err != nil {
		t.Fatalf("decode node output: %v: %s", err, out)
	}
	if len(highlighted) != len(inputs) {
		t.Fatalf("got %d highlighted values, want %d", len(highlighted), len(inputs))
	}
	allowedSpan := regexp.MustCompile(`</?span(?: class="hl-[a-z]+")?>`)
	for i, got := range highlighted {
		withoutFixedMarkup := allowedSpan.ReplaceAllString(got, "")
		if strings.ContainsAny(withoutFixedMarkup, "<>") {
			t.Errorf("case %d introduced a non-highlighter element: %s", i, got)
		}
		for _, dangerous := range []string{"<script", "<img", "<iframe", "<svg", "<div", "<a "} {
			if strings.Contains(strings.ToLower(got), dangerous) {
				t.Errorf("case %d left dangerous literal %q in output: %s", i, dangerous, got)
			}
		}
		if !strings.Contains(got, "&lt;") {
			t.Errorf("case %d did not preserve escaped source markup: %s", i, got)
		}
	}
}
