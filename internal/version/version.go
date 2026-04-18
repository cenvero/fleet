// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package version

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
