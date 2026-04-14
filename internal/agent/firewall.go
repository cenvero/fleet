// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/cenvero/fleet/pkg/proto"
)

type FirewallManager interface {
	Status(context.Context) (proto.FirewallInfo, error)
	Enable(context.Context, bool) (proto.FirewallInfo, error)
	AddRule(context.Context, string) (proto.FirewallInfo, error)
	ListOpenPorts(context.Context) ([]int, error)
	SetPort(context.Context, int, bool) (proto.FirewallInfo, error)
}

type unsupportedFirewallManager struct {
	OS string
}

func (m unsupportedFirewallManager) Status(context.Context) (proto.FirewallInfo, error) {
	return proto.FirewallInfo{}, &RPCError{
		Code:    "unsupported_capability",
		Message: fmt.Sprintf("firewall management is not implemented for %s agents yet", m.OS),
	}
}

func (m unsupportedFirewallManager) Enable(context.Context, bool) (proto.FirewallInfo, error) {
	return proto.FirewallInfo{}, &RPCError{
		Code:    "unsupported_capability",
		Message: fmt.Sprintf("firewall management is not implemented for %s agents yet", m.OS),
	}
}

func (m unsupportedFirewallManager) AddRule(context.Context, string) (proto.FirewallInfo, error) {
	return proto.FirewallInfo{}, &RPCError{
		Code:    "unsupported_capability",
		Message: fmt.Sprintf("firewall management is not implemented for %s agents yet", m.OS),
	}
}

func (m unsupportedFirewallManager) ListOpenPorts(context.Context) ([]int, error) {
	return nil, &RPCError{
		Code:    "unsupported_capability",
		Message: fmt.Sprintf("port management is not implemented for %s agents yet", m.OS),
	}
}

func (m unsupportedFirewallManager) SetPort(context.Context, int, bool) (proto.FirewallInfo, error) {
	return proto.FirewallInfo{}, &RPCError{
		Code:    "unsupported_capability",
		Message: fmt.Sprintf("port management is not implemented for %s agents yet", m.OS),
	}
}

type ufwFirewallManager struct {
	Runner commandRunner
}

func defaultFirewallManager() FirewallManager {
	if runtime.GOOS == "linux" {
		return ufwFirewallManager{Runner: execRunner{}}
	}
	return unsupportedFirewallManager{OS: runtime.GOOS}
}

func (m ufwFirewallManager) Status(ctx context.Context) (proto.FirewallInfo, error) {
	output, err := m.Runner.Run(ctx, "ufw", "status")
	if err != nil {
		return proto.FirewallInfo{}, &RPCError{
			Code:    "firewall_status_failed",
			Message: nonEmptyCommandMessage(output, err),
		}
	}
	info, parseErr := parseUFWStatus(string(output))
	if parseErr != nil {
		return proto.FirewallInfo{}, &RPCError{
			Code:    "firewall_status_failed",
			Message: parseErr.Error(),
		}
	}
	return info, nil
}

func (m ufwFirewallManager) Enable(ctx context.Context, enabled bool) (proto.FirewallInfo, error) {
	args := []string{"disable"}
	code := "firewall_disable_failed"
	if enabled {
		args = []string{"--force", "enable"}
		code = "firewall_enable_failed"
	}
	output, err := m.Runner.Run(ctx, "ufw", args...)
	if err != nil {
		return proto.FirewallInfo{}, &RPCError{
			Code:    code,
			Message: nonEmptyCommandMessage(output, err),
		}
	}
	return m.Status(ctx)
}

func (m ufwFirewallManager) AddRule(ctx context.Context, rule string) (proto.FirewallInfo, error) {
	args := strings.Fields(strings.TrimSpace(rule))
	if len(args) == 0 {
		return proto.FirewallInfo{}, &RPCError{
			Code:    "invalid_rule",
			Message: "firewall rule must not be empty",
		}
	}
	output, err := m.Runner.Run(ctx, "ufw", args...)
	if err != nil {
		return proto.FirewallInfo{}, &RPCError{
			Code:    "firewall_rule_failed",
			Message: nonEmptyCommandMessage(output, err),
		}
	}
	return m.Status(ctx)
}

func (m ufwFirewallManager) ListOpenPorts(ctx context.Context) ([]int, error) {
	info, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	return append([]int(nil), info.OpenPorts...), nil
}

func (m ufwFirewallManager) SetPort(ctx context.Context, port int, open bool) (proto.FirewallInfo, error) {
	if port < 1 || port > 65535 {
		return proto.FirewallInfo{}, &RPCError{
			Code:    "invalid_port",
			Message: fmt.Sprintf("port %d is outside the valid range", port),
		}
	}
	portArg := strconv.Itoa(port)
	args := []string{"--force", "delete", "allow", portArg}
	code := "port_close_failed"
	if open {
		args = []string{"allow", portArg}
		code = "port_open_failed"
	}
	output, err := m.Runner.Run(ctx, "ufw", args...)
	if err != nil {
		return proto.FirewallInfo{}, &RPCError{
			Code:    code,
			Message: nonEmptyCommandMessage(output, err),
		}
	}
	return m.Status(ctx)
}

func parseUFWStatus(output string) (proto.FirewallInfo, error) {
	info := proto.FirewallInfo{}
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	statusFound := false

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Status:") {
			statusFound = true
			status := strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
			info.Enabled = strings.EqualFold(status, "active")
			continue
		}
		if !statusFound || isUFWHeaderLine(line) || strings.HasPrefix(line, "--") {
			continue
		}
		info.Rules = append(info.Rules, line)
		if fields := strings.Fields(line); len(fields) > 0 {
			if port, ok := parseUFWPort(fields[0]); ok && !slices.Contains(info.OpenPorts, port) {
				info.OpenPorts = append(info.OpenPorts, port)
			}
		}
	}

	if !statusFound {
		return proto.FirewallInfo{}, fmt.Errorf("could not parse ufw status output")
	}
	slices.Sort(info.OpenPorts)
	return info, nil
}

func isUFWHeaderLine(line string) bool {
	fields := strings.Fields(line)
	return len(fields) >= 3 && strings.EqualFold(fields[0], "to") && strings.EqualFold(fields[1], "action") && strings.EqualFold(fields[2], "from")
}

func parseUFWPort(token string) (int, bool) {
	token = strings.TrimSpace(token)
	if token == "" || strings.Contains(token, ":") {
		return 0, false
	}
	end := 0
	for end < len(token) && token[end] >= '0' && token[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	port, err := strconv.Atoi(token[:end])
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}
