// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build linux

package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// aclAccessXattr holds a file's POSIX access ACL.
const aclAccessXattr = "system.posix_acl_access"

// copyXattrs gives dst the extended attributes of src: POSIX ACLs, the
// SELinux label, file capabilities and any user/trusted attributes. A value
// the new file already has (a new file usually gets the right SELinux label
// from its directory) is left alone. An access ACL the new file inherited from
// its directory's default ACL, but the original did not have, is removed. Any
// attribute that cannot be copied fails the edit. It returns which kinds of
// metadata were carried over.
func copyXattrs(dst, src *os.File) ([]string, *RPCError) {
	var names []string
	var kinds []string
	var rerr *RPCError
	err := withFds(src, dst, func(srcFd, dstFd int) {
		var err error
		names, err = listXattrs(srcFd)
		if err != nil {
			if errors.Is(err, unix.ENOTSUP) {
				names = nil // the filesystem has no extended attributes
				return
			}
			rerr = &RPCError{Code: "cannot_preserve_xattrs", Message: fmt.Sprintf("cannot list the file's extended attributes (%v); nothing was changed", err)}
			return
		}
		for _, name := range names {
			val, err := getXattr(srcFd, name)
			if errors.Is(err, unix.ENODATA) {
				continue // removed since it was listed
			}
			if err != nil {
				rerr = &RPCError{Code: "cannot_preserve_xattrs", Message: fmt.Sprintf("cannot read extended attribute %s (%v); nothing was changed", name, err)}
				return
			}
			if cur, err := getXattr(dstFd, name); err != nil || !bytes.Equal(cur, val) {
				if err := unix.Fsetxattr(dstFd, name, val, 0); err != nil {
					rerr = &RPCError{Code: "cannot_preserve_xattrs", Message: fmt.Sprintf("cannot copy extended attribute %s to the new file (%v); nothing was changed", name, err)}
					return
				}
			}
			if k := xattrKind(name); !slices.Contains(kinds, k) {
				kinds = append(kinds, k)
			}
		}
		if !slices.Contains(names, aclAccessXattr) {
			if _, err := getXattr(dstFd, aclAccessXattr); err == nil {
				if err := unix.Fremovexattr(dstFd, aclAccessXattr); err != nil {
					rerr = &RPCError{Code: "cannot_preserve_xattrs", Message: fmt.Sprintf("the new file inherited an ACL the original does not have and it could not be removed (%v); nothing was changed", err)}
				}
			}
		}
	})
	if err != nil {
		return nil, &RPCError{Code: "cannot_preserve_xattrs", Message: err.Error()}
	}
	return kinds, rerr
}

func xattrKind(name string) string {
	switch {
	case strings.HasPrefix(name, "system.posix_acl_"):
		return "acl"
	case name == "security.selinux":
		return "selinux"
	case name == "security.capability":
		return "capabilities"
	default:
		return "xattrs"
	}
}

// withFds runs fn with the raw descriptors of a and b.
func withFds(a, b *os.File, fn func(aFd, bFd int)) error {
	ac, err := a.SyscallConn()
	if err != nil {
		return err
	}
	bc, err := b.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	if err := ac.Control(func(aFd uintptr) {
		inner = bc.Control(func(bFd uintptr) { fn(int(aFd), int(bFd)) }) // #nosec G115 -- file descriptors fit in int
	}); err != nil {
		return err
	}
	return inner
}

func listXattrs(fd int) ([]string, error) {
	for range 4 {
		size, err := unix.Flistxattr(fd, nil)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, nil
		}
		buf := make([]byte, size)
		n, err := unix.Flistxattr(fd, buf)
		if errors.Is(err, unix.ERANGE) {
			continue // grew between the two calls
		}
		if err != nil {
			return nil, err
		}
		var names []string
		for _, name := range bytes.Split(buf[:n], []byte{0}) {
			if len(name) > 0 {
				names = append(names, string(name))
			}
		}
		return names, nil
	}
	return nil, unix.ERANGE
}

func getXattr(fd int, name string) ([]byte, error) {
	for range 4 {
		size, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size)
		if size == 0 {
			return buf, nil
		}
		n, err := unix.Fgetxattr(fd, name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	return nil, unix.ERANGE
}
