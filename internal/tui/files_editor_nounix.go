// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !unix

package tui

import "os"

// localLinkCount is not available here; files are treated as singly linked.
func localLinkCount(os.FileInfo) uint64 { return 1 }

// checkLocalWritable proves the file could be saved with an ordinary write
// (a read-only file fails to open); opening it for writing changes nothing.
func checkLocalWritable(root *os.Root, name string) error {
	w, err := root.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return w.Close()
}

// keepLocalOwner has nothing to carry over: ownership here is not a uid/gid
// pair, and the new file takes the folder's inherited permissions.
func keepLocalOwner(*os.File, os.FileInfo) error { return nil }
