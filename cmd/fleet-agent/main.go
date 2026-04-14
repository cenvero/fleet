// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package main

import (
	"os"

	"github.com/cenvero/fleet/internal/agent"
)

func main() {
	if err := agent.NewRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
