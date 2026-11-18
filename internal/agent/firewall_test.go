// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
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

// recordingFirewall records each Enable call instead of touching a firewall.
type recordingFirewall struct {
	unsupportedFirewallManager
	mu    sync.Mutex
	calls []bool
}

func (f *recordingFirewall) Enable(_ context.Context, enable bool) (proto.FirewallInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, enable)
	return proto.FirewallInfo{Enabled: enable}, nil
}

// TestFirewallEnableIgnoresActionCase: the action is dispatched without regard
// to case, so the enable/disable decision must be too. "Firewall.Enable" used
// to reach the handler and then disable the firewall.
func TestFirewallEnableIgnoresActionCase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		action string
		enable bool
	}{
		{"firewall.enable", true},
		{"Firewall.Enable", true},
		{"FIREWALL.ENABLE", true},
		{"firewall.disable", false},
		{"Firewall.Disable", false},
	}
	for _, tc := range cases {
		fw := &recordingFirewall{}
		session, _ := startRPCAgent(t, Server{FirewallManager: fw})
		resp := callAgent(t, session, proto.Envelope{Action: tc.action}, 5*time.Second)
		if resp.Error != nil {
			t.Fatalf("%s: error %+v", tc.action, resp.Error)
		}
		if resp.Action != tc.action {
			t.Errorf("%s: reply action = %q, want the request's action echoed", tc.action, resp.Action)
		}
		fw.mu.Lock()
		calls := slices.Clone(fw.calls)
		fw.mu.Unlock()
		if len(calls) != 1 || calls[0] != tc.enable {
			t.Errorf("%s: Enable calls = %v, want [%v]", tc.action, calls, tc.enable)
		}
	}
}
