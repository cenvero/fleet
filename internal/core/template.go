// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/cenvero/fleet/internal/logs"
)

type ServerTemplate struct {
	Name        string            `toml:"name" json:"name,omitempty"`
	Description string            `toml:"description" json:"description,omitempty"`
	Services    []TemplateService `toml:"services" json:"services,omitempty"`
	Firewall    TemplateFirewall  `toml:"firewall" json:"firewall"`
}

type TemplateService struct {
	Name     string `toml:"name" json:"name"`
	LogPath  string `toml:"log_path" json:"log_path,omitempty"`
	Critical bool   `toml:"critical" json:"critical"`
	Action   string `toml:"action" json:"action,omitempty"`
}

type TemplateFirewall struct {
	Enabled   *bool    `toml:"enabled" json:"enabled,omitempty"`
	OpenPorts []int    `toml:"open_ports" json:"open_ports,omitempty"`
	Rules     []string `toml:"rules" json:"rules,omitempty"`
}

func LoadTemplate(path string) (ServerTemplate, error) {
	var tpl ServerTemplate
	if _, err := toml.DecodeFile(path, &tpl); err != nil {
		return ServerTemplate{}, fmt.Errorf("decode template %s: %w", path, err)
	}
	return tpl, tpl.Validate()
}

func (t ServerTemplate) Validate() error {
	for _, service := range t.Services {
		if strings.TrimSpace(service.Name) == "" {
			return fmt.Errorf("template service name is required")
		}
		switch strings.TrimSpace(service.Action) {
		case "", "start", "stop", "restart":
		default:
			return fmt.Errorf("unsupported template service action %q for %s", service.Action, service.Name)
		}
	}
	for _, port := range t.Firewall.OpenPorts {
		if port <= 0 || port > 65535 {
			return fmt.Errorf("template port %d is out of range", port)
		}
	}
	return nil
}

func (a *App) ApplyTemplate(serverName, templateName string) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	if err := validateSafeName(templateName); err != nil {
		return fmt.Errorf("invalid template name: %w", err)
	}
	templatePath := filepath.Join(a.ConfigDir, "templates", templateName)
	tpl, err := LoadTemplate(templatePath)
	if err != nil {
		return err
	}

	for _, service := range tpl.Services {
		if err := a.AddService(serverName, service.Name, service.LogPath, service.Critical); err != nil {
			return err
		}
		if action := strings.TrimSpace(service.Action); action != "" {
			if err := a.ControlService(serverName, service.Name, action); err != nil {
				return err
			}
		}
	}

	if tpl.Firewall.Enabled != nil {
		if err := a.SetFirewall(serverName, *tpl.Firewall.Enabled); err != nil {
			return err
		}
	}
	for _, port := range tpl.Firewall.OpenPorts {
		if err := a.SetPort(serverName, port, true); err != nil {
			return err
		}
	}
	for _, rule := range tpl.Firewall.Rules {
		if err := a.AddFirewallRule(serverName, rule); err != nil {
			return err
		}
	}

	server, err = a.GetServer(serverName)
	if err != nil {
		return err
	}
	server.LastTemplate = templateName
	server.OpenPorts = uniqueSortedPorts(server.OpenPorts)
	if err := a.SaveServer(server); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "template.apply",
		Target:   serverName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("%s services=%d rules=%d ports=%d", templateName, len(tpl.Services), len(tpl.Firewall.Rules), len(tpl.Firewall.OpenPorts)),
	})
}

func uniqueSortedPorts(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[int]struct{}, len(values))
	out := make([]int, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	slices.Sort(out)
	return out
}
