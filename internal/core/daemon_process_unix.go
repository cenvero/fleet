// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package core

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// detachedProcAttr starts the daemon in its own session, so it has no
// controlling terminal and survives the shell that ran `fleet start`.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func terminateProcess(pid int) error {
	return unix.Kill(pid, unix.SIGTERM)
}

// processLooksLikeDaemon double-checks, where the OS lets us, that pid is a
// fleet binary running `daemon`. On Linux it reads /proc/<pid>/cmdline; other
// systems rely on the daemon lock alone.
func processLooksLikeDaemon(pid int) (bool, string) {
	return processLooksLikeFleet(pid, "daemon")
}

// processLooksLikeFleet double-checks, where the OS lets us, that pid is a
// fleet binary whose arguments include every one of words (`daemon`, or
// `console serve`), so stopping one kind of fleet process can never signal
// another. On Linux it reads /proc/<pid>/cmdline; other systems rely on the
// lock the process holds.
func processLooksLikeFleet(pid int, words ...string) (bool, string) {
	if runtime.GOOS != "linux" {
		return true, ""
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline") // #nosec G304 -- procfs path built from an integer pid
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "the process no longer exists"
		}
		return true, "" // procfs hidden from us: the daemon lock is the guard
	}
	args := strings.Split(string(bytes.TrimRight(raw, "\x00")), "\x00")
	if len(args) == 0 {
		return false, "its command line is empty"
	}
	for _, w := range words {
		if !slices.Contains(args[1:], w) {
			return false, fmt.Sprintf("its command line (%q) is not a fleet %s", strings.Join(args, " "), strings.Join(words, " "))
		}
	}
	if strings.Contains(strings.ToLower(filepath.Base(args[0])), "fleet") {
		return true, ""
	}
	// Renamed binary: accept it when it is this very executable.
	if exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
		exe = strings.TrimSuffix(exe, " (deleted)")
		if self, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(self); err == nil && resolved == exe {
				return true, ""
			}
		}
	}
	return false, fmt.Sprintf("its command line (%q) does not run the fleet binary", strings.Join(args, " "))
}
