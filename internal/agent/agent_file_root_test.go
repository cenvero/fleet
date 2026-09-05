// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

func TestHelloCommandAdvertisesFirstNormalizedFileRoot(t *testing.T) {
	SetAllowedFileRoots(nil)
	t.Cleanup(func() { SetAllowedFileRoots(nil) })

	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configuredFirst := "  " + filepath.Join(first, "..", filepath.Base(first)) + "  "

	cmd := NewRootCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{"--file-root", configuredFirst, "--file-root", second, "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hello command: %v\n%s", err, stdout.String())
	}

	var payload proto.HelloPayload
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode hello payload: %v\n%s", err, stdout.String())
	}
	want, err := filepath.EvalSymlinks(filepath.Clean(strings.TrimSpace(configuredFirst)))
	if err != nil {
		t.Fatal(err)
	}
	if payload.FileRoot != want {
		t.Fatalf("hello file root = %q, want first normalized root %q", payload.FileRoot, want)
	}
}

func TestNativeFileRootWithoutConfinementUsesSystemRoot(t *testing.T) {
	SetAllowedFileRoots(nil)
	t.Cleanup(func() { SetAllowedFileRoots(nil) })

	want := "/"
	if runtime.GOOS == "windows" {
		drive := strings.TrimSpace(os.Getenv("SystemDrive"))
		if len(drive) >= 2 && drive[1] == ':' {
			want = strings.ToUpper(drive[:1]) + `:\`
		} else {
			want = `C:\`
		}
	}
	if got := nativeFileRoot(); got != want {
		t.Fatalf("nativeFileRoot() = %q, want native system root %q", got, want)
	}
}
