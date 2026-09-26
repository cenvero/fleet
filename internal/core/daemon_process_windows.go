// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package core

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedProcAttr starts the daemon without a console and in its own process
// group, so closing the terminal or pressing Ctrl-C there does not stop it.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

const stillActive = 259 // STILL_ACTIVE exit code of a running process

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) // #nosec G115 -- pid is a positive process id
	if err != nil {
		// Access denied still means the process exists.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// terminateProcess ends the daemon. Windows has no SIGTERM for a detached
// process, so this is TerminateProcess; the caller removes the pid file.
func terminateProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// processLooksLikeDaemon relies on the daemon lock on Windows.
func processLooksLikeDaemon(int) (bool, string) {
	return true, ""
}
