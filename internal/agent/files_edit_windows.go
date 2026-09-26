// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cenvero/fleet/pkg/proto"
)

var procReplaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

// linkCount is not tracked on Windows; ReplaceFileW keeps the original file's
// identity, so hard links need no special handling there.
func linkCount(os.FileInfo) uint64 { return 1 }

// checkEditWritable relies on ReplaceFileW, which fails without write access
// to the original.
func checkEditWritable(*os.Root, string) *RPCError { return nil }

// preserveMetadata has nothing to do before the install on Windows:
// ReplaceFileW carries the original's owner, ACL and attributes over.
func preserveMetadata(_, _ *os.File, _ os.FileInfo) ([]string, *RPCError) { return nil, nil }

// installEditedTemp swaps the temp file in with ReplaceFileW, which keeps the
// replaced file's security descriptor (owner and ACL), attributes and
// alternate data streams. Without REPLACEFILE_IGNORE_MERGE_ERRORS a failure
// to carry any of those over fails the call, and the original stays as it was.
func installEditedTemp(root *os.Root, tempRel, finalRel string, _ os.FileInfo) ([]string, *RPCError) {
	replaced := filepath.Join(root.Name(), finalRel)
	replacement := filepath.Join(root.Name(), tempRel)
	rp, err := windows.UTF16PtrFromString(replaced)
	if err != nil {
		_ = os.Remove(replacement)
		return nil, &RPCError{Code: "rename_failed", Message: err.Error()}
	}
	np, err := windows.UTF16PtrFromString(replacement)
	if err != nil {
		_ = os.Remove(replacement)
		return nil, &RPCError{Code: "rename_failed", Message: err.Error()}
	}
	r1, _, callErr := procReplaceFileW.Call(uintptr(unsafe.Pointer(rp)), uintptr(unsafe.Pointer(np)), 0, 0, 0, 0)
	if r1 == 0 {
		_ = os.Remove(replacement)
		return nil, &RPCError{Code: "rename_failed", Message: fmt.Sprintf("ReplaceFileW: %v", callErr)}
	}
	return []string{"owner", "acl", "attributes"}, nil
}

// describeFile fills in the mode reported for an edit. Owner and group are
// left empty: Windows describes access with ACLs.
func describeFile(res *proto.FileEditResult, info os.FileInfo) {
	res.Mode = unixModeBits(info.Mode())
}
