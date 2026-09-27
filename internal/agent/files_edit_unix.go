// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build unix

package agent

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"

	"github.com/cenvero/fleet/pkg/proto"
)

// linkCount returns how many directory entries name the file.
func linkCount(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) // #nosec G115 -- a link count is never negative
	}
	return 1
}

// checkEditWritable refuses an edit the agent could not make with an ordinary
// write. The new content is installed by renaming a temp file over the
// original, which needs only write access to the directory — so without this
// check an agent could replace a file it has no permission to write. Opening
// the file for writing proves the permission and changes nothing (no
// O_TRUNC). ETXTBSY (a running executable) is only reported after the
// permission check passed, and renaming over a running program is safe.
func checkEditWritable(root *os.Root, rel string) *RPCError {
	w, err := root.OpenFile(rel, os.O_WRONLY|oNoFollow|oNonBlock, 0)
	if err == nil {
		_ = w.Close()
		return nil
	}
	if errors.Is(err, syscall.ETXTBSY) {
		return nil
	}
	if errors.Is(err, os.ErrPermission) {
		return &RPCError{Code: "permission_denied", Message: fmt.Sprintf("the agent (uid %d) may not write this file; nothing was changed", os.Geteuid())}
	}
	return &RPCError{Code: "open_failed", Message: err.Error()}
}

// preserveMetadata gives the temp file tf the owner, group, extended
// attributes and permission bits of the original. The order matters: chown
// clears set-id bits and file capabilities, so it runs first; extended
// attributes (ACLs, SELinux labels, capabilities) next; and the exact mode
// last, which also leaves an ACL mask matching the original. Anything that
// cannot be carried over makes the edit fail rather than install a file with
// different permissions.
func preserveMetadata(tf, orig *os.File, origInfo os.FileInfo) ([]string, *RPCError) {
	st, ok := origInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, &RPCError{Code: "stat_failed", Message: "cannot read the file's owner"}
	}
	uid, gid := int(st.Uid), int(st.Gid)
	kept := []string{"owner"}
	if uid != os.Geteuid() || gid != os.Getegid() {
		if err := tf.Chown(uid, gid); err != nil {
			return nil, &RPCError{Code: "cannot_preserve_owner", Message: fmt.Sprintf("cannot give the new file the original owner %d:%d (%v); nothing was changed", uid, gid, err)}
		}
	}
	xattrKinds, rerr := copyXattrs(tf, orig)
	if rerr != nil {
		return nil, rerr
	}
	kept = append(kept, xattrKinds...)
	mode := origInfo.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if err := tf.Chmod(mode); err != nil {
		return nil, &RPCError{Code: "cannot_preserve_mode", Message: fmt.Sprintf("cannot give the new file the original mode %04o (%v); nothing was changed", uint32(mode.Perm()), err)}
	}
	kept = append(kept, "mode")
	// Check the result instead of trusting the calls.
	got, err := tf.Stat()
	if err != nil {
		return nil, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	gst, ok := got.Sys().(*syscall.Stat_t)
	if !ok || int(gst.Uid) != uid || int(gst.Gid) != gid || got.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != mode || gst.Nlink != 1 {
		return nil, &RPCError{Code: "cannot_preserve_mode", Message: "the new file's owner or mode did not come out as the original's; nothing was changed"}
	}
	return kept, nil
}

// installEditedTemp renames the finished temp file over the original.
func installEditedTemp(root *os.Root, tempRel, finalRel string, tempInfo os.FileInfo) ([]string, *RPCError) {
	return nil, installTemp(root, tempRel, finalRel, tempInfo)
}

// describeFile fills in the mode, owner and group reported for an edit.
func describeFile(res *proto.FileEditResult, info os.FileInfo) {
	res.Mode = unixModeBits(info.Mode())
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	gid := strconv.FormatUint(uint64(st.Gid), 10)
	res.Owner, res.Group = uid, gid
	if u, err := user.LookupId(uid); err == nil {
		res.Owner = u.Username
	}
	if g, err := user.LookupGroupId(gid); err == nil {
		res.Group = g.Name
	}
}
