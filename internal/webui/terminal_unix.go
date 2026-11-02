// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package webui

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// termSupported reports whether this controller can host browser terminals.
const termSupported = true

type unixPTY struct{ f *os.File }

func (p unixPTY) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p unixPTY) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p unixPTY) Close() error                { return p.f.Close() }

// Resize sets the window size; the kernel then sends SIGWINCH to the
// session's foreground process (fleet ssh), which forwards it to the server.
func (p unixPTY) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}) // #nosec G115 -- clamped to termMaxDimension
}

func startTermPTY(cmd *exec.Cmd, cols, rows int) (termPTY, error) {
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}) // #nosec G115 -- clamped to termMaxDimension
	if err != nil {
		return nil, err
	}
	return unixPTY{f: f}, nil
}
