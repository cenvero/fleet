// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestAppJSPathHelpers(t *testing.T) {
	t.Parallel()
	source, err := assets.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	text := string(source)
	for _, required := range []string{
		`function joinPath(dir, name, style)`,
		`function parentPath(p, style, fallback = "")`,
		`function rootPath(p, style, fallback = "")`,
		`function baseName(p, style)`,
		`function pathBreadcrumbs(p, style, fallback = "")`,
		`function pathWithin(root, target, style)`,
		`function componentNameError(name, style)`,
		`function batchNamespaceError(items, style, caseInsensitive = normalizePathStyle(style) === "windows")`,
		`const srcPath = joinPath(sp.path, item.name, sp.pathStyle);`,
		`const dstPath = joinPath(dstDir, item.name, dp.pathStyle);`,
		`applyPaneSource(p, e.target.value);`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("app.js missing path-style regression marker %q", required)
		}
	}

	start := bytes.Index(source, []byte("function normalizePathStyle"))
	end := bytes.Index(source, []byte("// isArchiveFile reports"))
	if start < 0 || end <= start {
		t.Fatalf("could not isolate app.js path helpers")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Log("node unavailable; static path-helper coverage completed")
		return
	}
	vectors := `
function check(actual, expected, label) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(label + ": got " + JSON.stringify(actual) + ", want " + JSON.stringify(expected));
  }
}
check(joinPath("/srv/files", "a.txt", "posix"), "/srv/files/a.txt", "posix join");
check(parentPath("/srv/files", "posix"), "/srv", "posix parent");
check(rootPath("/srv/files", "posix"), "/", "posix root");
check(baseName("/srv/files/a.txt", "posix"), "a.txt", "posix base");
check(joinPath("C:\\Users\\Mona", "note.txt", "windows"), "C:\\Users\\Mona\\note.txt", "drive join");
check(parentPath("C:\\Users\\Mona", "windows"), "C:\\Users", "drive parent");
check(parentPath("C:\\", "windows"), "C:\\", "drive root parent");
check(rootPath("C:\\Users\\Mona", "windows"), "C:\\", "drive root");
check(baseName("C:\\Users\\Mona\\note.txt", "windows"), "note.txt", "drive base");
check(joinPath("\\\\host\\share\\team", "note.txt", "windows"), "\\\\host\\share\\team\\note.txt", "UNC join");
check(parentPath("\\\\host\\share\\team", "windows"), "\\\\host\\share\\", "UNC parent");
check(parentPath("\\\\host\\share\\", "windows"), "\\\\host\\share\\", "UNC root parent");
check(rootPath("\\\\host\\share\\team", "windows"), "\\\\host\\share\\", "UNC root");
check(baseName("\\\\host\\share\\team\\note.txt", "windows"), "note.txt", "UNC base");
check(pathBreadcrumbs("C:\\Users\\Mona", "windows").map((c) => c.path), ["C:\\", "C:\\Users", "C:\\Users\\Mona"], "drive breadcrumbs");
check(pathBreadcrumbs("\\\\host\\share\\team", "windows").map((c) => c.path), ["\\\\host\\share\\", "\\\\host\\share\\team"], "UNC breadcrumbs");
check(parentPath("/srv/allowed/sub", "posix", "/srv/allowed"), "/srv/allowed", "bounded POSIX parent");
check(parentPath("/srv/allowed", "posix", "/srv/allowed"), "/srv/allowed", "bounded POSIX root");
check(pathBreadcrumbs("/srv/allowed/sub", "posix", "/srv/allowed").map((c) => c.path), ["/srv/allowed", "/srv/allowed/sub"], "bounded POSIX breadcrumbs");
check(parentPath("D:\\Allowed\\sub", "windows", "D:\\Allowed"), "D:\\Allowed", "bounded drive parent");
check(parentPath("D:\\Allowed", "windows", "D:\\Allowed"), "D:\\Allowed", "bounded drive root");
check(pathBreadcrumbs("\\\\host\\share\\allowed\\sub", "windows", "\\\\host\\share\\allowed").map((c) => c.path), ["\\\\host\\share\\allowed", "\\\\host\\share\\allowed\\sub"], "bounded UNC breadcrumbs");
check(Boolean(batchNamespaceError([{name: "README"}, {name: "Readme"}], "windows")), true, "Windows batch case collision");
check(Boolean(batchNamespaceError([{name: "CON"}], "windows")), true, "Windows batch reserved name");
check(batchNamespaceError([{name: "README"}, {name: "Readme"}], "posix"), "", "POSIX case-distinct batch");
check(Boolean(batchNamespaceError([{name: "README"}, {name: "Readme"}], "posix", true)), true, "case-insensitive POSIX batch collision");
check(componentNameError("notes.txt", "posix"), "", "valid POSIX component");
check(componentNameError("CON", "posix"), "", "Windows reserved is valid on POSIX");
check(componentNameError("trailing.", "posix"), "", "trailing dot is valid on POSIX");
for (const [name, label] of [
  ["", "empty"], [".", "dot"], ["..", "dot-dot"], ["a/b", "slash"],
  ["a\\\\b", "backslash"], ["C:\\\\temp", "drive absolute"],
  ["\\\\\\\\host\\\\share", "UNC absolute"], ["bad\u0001name", "control"],
]) {
  check(Boolean(componentNameError(name, "posix")), true, label);
}
for (const [name, label] of [
  ["bad:name", "colon"], ["bad<name", "angle"], ["bad|name", "pipe"],
  ["bad?name", "question"], ["bad*name", "asterisk"], ["bad\"name", "quote"],
  ["CON", "reserved"], ["con.txt", "reserved extension"], ["LPT9.log", "reserved port"],
  ["COM¹", "reserved superscript"], ["trail.", "trailing dot"], ["trail ", "trailing space"],
]) {
  check(Boolean(componentNameError(name, "windows")), true, "windows " + label);
}
`
	script := append([]byte{}, source[start:end]...)
	script = append(script, vectors...)
	cmd := exec.Command(node, "-e", string(script)) // #nosec G204 -- node path is resolved locally and script is the repository's embedded static asset
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("app.js path helper vectors failed: %v\n%s", err, output)
	}
}
