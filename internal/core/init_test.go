// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"strings"
	"testing"
)

func TestInitConfigChoicesLinuxUnchanged(t *testing.T) {
	t.Parallel()

	choices := initConfigChoices("linux", nil, "/home/operator")
	wantPaths := []string{
		"/home/operator/.cenvero-fleet",
		"/etc/cenvero-fleet",
		"/opt/cenvero-fleet",
	}
	for i, want := range wantPaths {
		if choices[i].Path != want {
			t.Fatalf("choice %d path = %q, want %q", i+1, choices[i].Path, want)
		}
	}
	if choices[1].Description != "system-wide, requires sudo" {
		t.Fatalf("Linux system-wide guidance changed: %q", choices[1].Description)
	}

	var menu bytes.Buffer
	writeInitConfigChoices(&menu, "linux", choices)
	wantMenu := "  [1] /home/operator/.cenvero-fleet              (recommended, per-user)\n" +
		"  [2] /etc/cenvero-fleet            (system-wide, requires sudo)\n" +
		"  [3] /opt/cenvero-fleet\n" +
		"  [4] Custom path\n"
	if got := menu.String(); got != wantMenu {
		t.Fatalf("Linux menu changed:\n%s\nwant:\n%s", got, wantMenu)
	}
}

func TestInitConfigChoicesWindowsUseOnlyWindowsLocations(t *testing.T) {
	t.Parallel()

	home := `C:\Users\O'Brien Operator`
	choices := initConfigChoices("windows", map[string]string{
		"LOCALAPPDATA": `D:\Local Data\O'Brien`,
		"PROGRAMDATA":  `E:\Shared Program Data`,
	}, home)
	wantPaths := []string{
		`D:\Local Data\O'Brien\Cenvero Fleet`,
		`C:\Users\O'Brien Operator\.cenvero-fleet`,
		`E:\Shared Program Data\Cenvero Fleet`,
	}
	for i, want := range wantPaths {
		if choices[i].Path != want {
			t.Fatalf("choice %d path = %q, want %q", i+1, choices[i].Path, want)
		}
	}

	var menu bytes.Buffer
	writeInitConfigChoices(&menu, "windows", choices)
	output := menu.String()
	for _, forbidden := range []string{"/etc", "/opt", "sudo"} {
		if strings.Contains(strings.ToLower(output), forbidden) {
			t.Fatalf("Windows menu contains Linux guidance %q:\n%s", forbidden, output)
		}
	}
	if !strings.Contains(output, "requires administrator") {
		t.Fatalf("Windows menu does not use administrator guidance:\n%s", output)
	}
}

func TestInitConfigChoicesWindowsFallbacks(t *testing.T) {
	t.Parallel()

	choices := initConfigChoices("windows", map[string]string{"SystemDrive": "Z:"}, `Z:\Users\operator`)
	if got, want := choices[0].Path, `Z:\Users\operator\AppData\Local\Cenvero Fleet`; got != want {
		t.Fatalf("LOCALAPPDATA fallback = %q, want %q", got, want)
	}
	if got, want := choices[2].Path, `Z:\ProgramData\Cenvero Fleet`; got != want {
		t.Fatalf("PROGRAMDATA fallback = %q, want %q", got, want)
	}
}

func TestInitConfigChoicesDarwinUseApplicationSupport(t *testing.T) {
	t.Parallel()

	choices := initConfigChoices("darwin", nil, "/Users/O'Brien Operator")
	var menu bytes.Buffer
	writeInitConfigChoices(&menu, "darwin", choices)
	output := menu.String()

	for _, want := range []string{
		"/Users/O'Brien Operator/Library/Application Support/Cenvero Fleet",
		"/Library/Application Support/Cenvero Fleet",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("macOS menu missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{"/etc", "/opt", "sudo"} {
		if strings.Contains(strings.ToLower(output), forbidden) {
			t.Fatalf("macOS menu contains Linux guidance %q:\n%s", forbidden, output)
		}
	}
}
