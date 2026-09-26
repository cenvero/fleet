// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build linux || darwin

package tui

import (
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// localDiskFree reports the bytes available to an unprivileged user on the
// filesystem holding path.
func localDiskFree(path string) (int64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	free := uint64(st.Bavail) * uint64(st.Bsize) // #nosec G115 -- block counts are non-negative
	if free > 1<<62 {
		return 0, false
	}
	return int64(free), true
}

var (
	ownerMu    sync.Mutex
	ownerNames = map[string]string{}
)

// fileOwner renders "user:group" for a local file, resolving ids to names
// once and caching them (the listing preview asks for the same few ids over
// and over).
func fileOwner(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return ""
	}
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	gid := strconv.FormatUint(uint64(st.Gid), 10)
	return lookupName("u"+uid, uid, true) + ":" + lookupName("g"+gid, gid, false)
}

func lookupName(key, id string, isUser bool) string {
	ownerMu.Lock()
	defer ownerMu.Unlock()
	if name, ok := ownerNames[key]; ok {
		return name
	}
	name := id
	if isUser {
		if u, err := user.LookupId(id); err == nil && u.Username != "" {
			name = u.Username
		}
	} else if g, err := user.LookupGroupId(id); err == nil && g.Name != "" {
		name = g.Name
	}
	ownerNames[key] = name
	return name
}
