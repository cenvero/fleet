// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"slices"
	"testing"
)

func TestParseUFWStatus(t *testing.T) {
	t.Parallel()

	output := `
Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
80/tcp                     ALLOW       Anywhere
443/tcp (v6)               ALLOW       Anywhere (v6)
Nginx Full                 ALLOW       Anywhere
`

	info, err := parseUFWStatus(output)
	if err != nil {
		t.Fatalf("parseUFWStatus() error = %v", err)
	}
	if !info.Enabled {
		t.Fatalf("expected active firewall status")
	}
	if len(info.Rules) != 4 {
		t.Fatalf("expected 4 parsed rules, got %d", len(info.Rules))
	}
	if !slices.Equal(info.OpenPorts, []int{22, 80, 443}) {
		t.Fatalf("unexpected parsed ports %#v", info.OpenPorts)
	}
}
