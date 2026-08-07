// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import _ "embed"

// embeddedSigningPublicKey hardcodes the release verification key into the
// controller binary so update verification does not depend on mutable runtime
// files.
//
//go:embed signing.pub
var embeddedSigningPublicKey string

// SigningPublicKey returns the embedded release verification key. It is exposed
// for controller-side verification of fleet-agent archives before SSH upload.
func SigningPublicKey() string {
	return embeddedSigningPublicKey
}
