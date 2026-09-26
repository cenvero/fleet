// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package core

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestUploadIgnoresSidecarPlantedByAnotherUser drives a whole chunked upload
// into a shared directory where another local user pre-created the upload's
// predictable temp file: the upload must succeed, install the agent-owned
// file with the right bytes, and leave the planted file untouched.
func TestUploadIgnoresSidecarPlantedByAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a file owned by another user")
	}
	t.Parallel()
	rig := quietRig(t)
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o1777); err != nil {
		t.Fatal(err)
	}
	data := randomBytes(t, 300<<10)
	src := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(shared, "payload.bin")
	planted := remote + ".fleet-" + transferIDFor(remote, info.Size(), fileIdentity(info)) + ".part"
	evil := bytes.Repeat([]byte{'E'}, len(data))
	copy(evil[64<<10:], data[64<<10:]) // correct bytes after the first chunk, to tempt a resume
	if err := os.WriteFile(planted, evil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(planted, 65534, 65534); err != nil {
		t.Fatal(err)
	}

	res, err := rig.app.UploadFile("loopback", src, remote, FileTransferOptions{ChunkSize: 64 << 10}, nil)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if res.SHA256 != hexSum(data) {
		t.Fatalf("reported sha256 %s", res.SHA256)
	}
	if got, _ := os.ReadFile(remote); !bytes.Equal(got, data) {
		t.Fatal("installed bytes differ from the source")
	}
	st, err := os.Lstat(remote)
	if err != nil {
		t.Fatal(err)
	}
	if uid := st.Sys().(*syscall.Stat_t).Uid; uid != 0 {
		t.Fatalf("installed file owned by uid %d", uid)
	}
	if got, _ := os.ReadFile(planted); !bytes.Equal(got, evil) {
		t.Fatal("the planted file was modified")
	}
}
