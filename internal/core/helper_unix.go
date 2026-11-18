// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package core

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// processLooksLikeHelper double-checks, where the OS lets us, that pid is a
// fleet process running __helper: Linux through /proc/<pid>/cmdline, other
// systems through ps. When neither can answer, the helper lock is the guard.
func processLooksLikeHelper(pid int) (bool, string) {
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline") // #nosec G304 -- procfs path built from an integer pid
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, "the process no longer exists"
			}
			return true, "" // procfs hidden from us
		}
		args := strings.Split(string(bytes.TrimRight(raw, "\x00")), "\x00")
		if len(args) > 1 && slices.Contains(args[1:], HelperCommand) {
			return true, ""
		}
		return false, fmt.Sprintf("its command line (%q) is not the fleet background helper", strings.Join(args, " "))
	}
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output() // #nosec G204 -- fixed program, integer pid
	if err != nil {
		return true, "" // ps unavailable, or the process just exited
	}
	if line := strings.TrimSpace(string(out)); line != "" && !slices.Contains(strings.Fields(line), HelperCommand) {
		return false, fmt.Sprintf("its command line (%q) is not the fleet background helper", line)
	}
	return true, ""
}
