// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build windows

package webui

import (
	"errors"
	"os/exec"
)

// termSupported reports whether this controller can host browser terminals.
// A Windows controller has no pseudo-terminal support here yet.
const termSupported = false

func startTermPTY(*exec.Cmd, int, int) (termPTY, error) {
	return nil, errors.New("browser terminals need a Linux or macOS controller")
}
