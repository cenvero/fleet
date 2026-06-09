// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import "embed"

//go:embed assets/index.html
var indexHTML []byte

//go:embed assets/app.js assets/app.css
var assets embed.FS
