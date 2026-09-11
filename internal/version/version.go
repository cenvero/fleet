// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package version

import (
	"strings"

	"golang.org/x/mod/semver"
)

const ProductName = "Cenvero Fleet"
const BinaryName = "fleet"
const Domain = "fleet.cenvero.org"

var Version = "dev"

// Canonical returns v with a leading "v" prefix, e.g. "1.6.1" → "v1.6.1".
// Already-prefixed strings and "dev" are returned unchanged.
func Canonical(v string) string {
	if v == "" || v == "dev" || v[0] == 'v' {
		return v
	}
	return "v" + v
}

// NormalizeSemVer trims a release version, validates strict semantic versioning,
// and returns one consistent v-prefixed representation. Sentinel, development,
// and malformed values are unavailable rather than misleading display values.
func NormalizeSemVer(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	switch strings.ToLower(value) {
	case "", "-", "dev", "unknown", "unavailable", "n/a":
		return "", false
	}
	if strings.HasPrefix(value, "V") {
		return "", false
	}
	if !strings.HasPrefix(value, "v") {
		value = "v" + value
	}
	core := strings.TrimPrefix(value, "v")
	if suffix := strings.IndexAny(core, "-+"); suffix >= 0 {
		core = core[:suffix]
	}
	if strings.Count(core, ".") != 2 || !semver.IsValid(value) {
		return "", false
	}
	return value, true
}

// DisplaySemVer returns a consistent release version for human-facing output.
func DisplaySemVer(raw string) string {
	if normalized, ok := NormalizeSemVer(raw); ok {
		return normalized
	}
	return "-"
}
