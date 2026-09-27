// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !unix && !windows

package agent

import (
	"os"

	"github.com/cenvero/fleet/pkg/proto"
)

func linkCount(os.FileInfo) uint64 { return 1 }

func checkEditWritable(*os.Root, string) *RPCError { return nil }

// preserveMetadata refuses: this platform has no way to carry a file's
// ownership over to its replacement, so editing is not offered here.
func preserveMetadata(_, _ *os.File, _ os.FileInfo) ([]string, *RPCError) {
	return nil, &RPCError{Code: "unsupported_platform", Message: "file.edit cannot preserve file metadata on this platform"}
}

func installEditedTemp(root *os.Root, tempRel, finalRel string, tempInfo os.FileInfo) ([]string, *RPCError) {
	return nil, installTemp(root, tempRel, finalRel, tempInfo)
}

func describeFile(res *proto.FileEditResult, info os.FileInfo) {
	res.Mode = unixModeBits(info.Mode())
}
