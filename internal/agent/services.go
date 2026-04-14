// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/cenvero/fleet/pkg/proto"
)

type ServiceManager interface {
	List(context.Context) ([]proto.ServiceInfo, error)
	Control(context.Context, string, string) (proto.ServiceInfo, error)
}

type RPCError struct {
	Code    string
	Message string
}

func (e *RPCError) Error() string {
	return e.Message
}

type commandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

type unsupportedServiceManager struct {
	OS string
}

func (m unsupportedServiceManager) List(context.Context) ([]proto.ServiceInfo, error) {
	return nil, &RPCError{
		Code:    "unsupported_capability",
		Message: fmt.Sprintf("service management is not implemented for %s agents yet", m.OS),
	}
}

func (m unsupportedServiceManager) Control(context.Context, string, string) (proto.ServiceInfo, error) {
	return proto.ServiceInfo{}, &RPCError{
		Code:    "unsupported_capability",
		Message: fmt.Sprintf("service management is not implemented for %s agents yet", m.OS),
	}
}

type systemdServiceManager struct {
	Runner commandRunner
}

func defaultServiceManager() ServiceManager {
	if runtime.GOOS == "linux" {
		return systemdServiceManager{Runner: execRunner{}}
	}
	return unsupportedServiceManager{OS: runtime.GOOS}
}

func (m systemdServiceManager) List(ctx context.Context) ([]proto.ServiceInfo, error) {
	output, err := m.Runner.Run(ctx, "systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return nil, &RPCError{Code: "service_list_failed", Message: nonEmptyCommandMessage(output, err)}
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	services := make([]proto.ServiceInfo, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		services = append(services, proto.ServiceInfo{
			Name:        fields[0],
			LoadState:   fields[1],
			ActiveState: fields[2],
			SubState:    fields[3],
			Description: strings.Join(fields[4:], " "),
		})
	}
	return services, nil
}

func (m systemdServiceManager) Control(ctx context.Context, service, action string) (proto.ServiceInfo, error) {
	if !slices.Contains([]string{"start", "stop", "restart"}, action) {
		return proto.ServiceInfo{}, &RPCError{
			Code:    "invalid_action",
			Message: fmt.Sprintf("service action %q is not supported", action),
		}
	}

	output, err := m.Runner.Run(ctx, "systemctl", action, service)
	if err != nil {
		return proto.ServiceInfo{}, &RPCError{Code: "service_control_failed", Message: nonEmptyCommandMessage(output, err)}
	}

	return m.show(ctx, service)
}

func (m systemdServiceManager) show(ctx context.Context, service string) (proto.ServiceInfo, error) {
	output, err := m.Runner.Run(ctx, "systemctl", "show", service, "--property=Id,LoadState,ActiveState,SubState,Description", "--no-pager")
	if err != nil {
		return proto.ServiceInfo{}, &RPCError{Code: "service_show_failed", Message: nonEmptyCommandMessage(output, err)}
	}

	var info proto.ServiceInfo
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "Id":
			info.Name = value
		case "LoadState":
			info.LoadState = value
		case "ActiveState":
			info.ActiveState = value
		case "SubState":
			info.SubState = value
		case "Description":
			info.Description = value
		}
	}
	if info.Name == "" {
		info.Name = service
	}
	return info, nil
}

func nonEmptyCommandMessage(output []byte, err error) string {
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	return message
}
