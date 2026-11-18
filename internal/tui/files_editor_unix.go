// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package tui

import (
	"errors"
	"os"
	"syscall"
)

// localLinkCount returns how many directory entries name the file.
func localLinkCount(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) // #nosec G115 -- a link count is never negative
	}
	return 1
}

// checkLocalWritable proves the file could be saved with an ordinary write.
// Renaming a new file over it needs only write access to the folder, so
// without this a read-only file would be replaced. Opening it for writing
// changes nothing (no O_TRUNC). ETXTBSY (a running program) only comes after
// the permission check passed, and renaming over a running program is safe.
func checkLocalWritable(root *os.Root, name string) error {
	w, err := root.OpenFile(name, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		return w.Close()
	}
	if errors.Is(err, syscall.ETXTBSY) {
		return nil
	}
	return err
}

// keepLocalOwner gives the replacement f the original's owner and group, or
// fails, so a save never hands a file to another user or group. What f got
// is read back rather than assumed: a new file takes the folder's group on
// BSD and macOS, and in a setgid folder on Linux. It runs before the mode is
// set, since chown clears set-id bits.
func keepLocalOwner(f *os.File, orig os.FileInfo) error {
	want, ok := orig.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the file's owner")
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	got, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the new file's owner")
	}
	uid, gid := -1, -1 // -1 leaves that id as it is
	if got.Uid != want.Uid {
		uid = int(want.Uid)
	}
	if got.Gid != want.Gid {
		gid = int(want.Gid)
	}
	if uid == -1 && gid == -1 {
		return nil
	}
	return f.Chown(uid, gid)
}
