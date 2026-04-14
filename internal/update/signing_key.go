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
