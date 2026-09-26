// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import "embed"

//go:embed assets/index.html
var indexHTML []byte

// Every asset the page loads is embedded: the UI never reaches a CDN or any
// external origin, which is what lets the CSP stay at 'self'.
//
//go:embed assets/app.js assets/app.css assets/theme.js assets/favicon.svg
var assets embed.FS
