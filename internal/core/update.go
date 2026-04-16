// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
)

type FleetUpdateResult struct {
	Controller update.ApplyResult       `json:"controller"`
	Agents     []FleetUpdateAgentResult `json:"agents"`
	Attempted  int                      `json:"attempted"`
	Succeeded  int                      `json:"succeeded"`
	Failed     int                      `json:"failed"`
}

type FleetUpdateAgentResult struct {
	Server            string `json:"server"`
	Channel           string `json:"channel"`
	CurrentVersion    string `json:"current_version"`
	Version           string `json:"version"`
	Applied           bool   `json:"applied"`
	SHA256Verified    bool   `json:"sha256_verified"`
	SignatureVerified bool   `json:"signature_verified"`
	RestartScheduled  bool   `json:"restart_scheduled"`
	ServiceName       string `json:"service_name,omitempty"`
	Error             string `json:"error,omitempty"`
}

// IsHomebrewInstall reports whether the controller binary is managed by Homebrew.
// When true, the controller must be updated via `brew upgrade cenvero-fleet`
// rather than the built-in self-updater.
func IsHomebrewInstall(executablePath string) bool {
	p := strings.ToLower(executablePath)
	return strings.Contains(p, "/homebrew/") ||
		strings.Contains(p, "/cellar/") ||
		strings.Contains(p, "/linuxbrew/")
}

// RuntimeIsHomebrewInstall checks the currently running binary path, not the stored config value.
func RuntimeIsHomebrewInstall() bool {
	exec, err := os.Executable()
	if err != nil {
		return false
	}
	return IsHomebrewInstall(exec)
}

type homebrewHintCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// HomebrewUpdateHint returns a non-empty latest version string when a newer stable
// release is available and the user has not disabled update notifications.
// It caches the manifest result for 10 minutes to avoid hammering the CDN on every command.
func HomebrewUpdateHint(configDir, manifestURL string, policy update.Policy) string {
	if policy == update.PolicyDisabled {
		return ""
	}
	cacheFile := filepath.Join(configDir, "data", "homebrew-update.json")
	var cache homebrewHintCache
	if data, err := os.ReadFile(cacheFile); err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	if time.Since(cache.CheckedAt) > 10*time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		manifest, err := update.Fetch(ctx, manifestURL)
		if err == nil {
			if ch, ok := manifest.Channels["stable"]; ok {
				cache = homebrewHintCache{CheckedAt: time.Now().UTC(), Latest: ch.Version}
				if data, err := json.Marshal(cache); err == nil {
					_ = os.WriteFile(cacheFile, data, 0o600)
				}
			}
		}
	}
	current := version.Version
	if !strings.HasPrefix(current, "v") {
		current = "v" + current
	}
	if cache.Latest != "" && cache.Latest != current {
		return cache.Latest
	}
	return ""
}

func (a *App) ApplyUpdate(ctx context.Context) (update.ApplyResult, error) {
	apply := a.ControllerUpdater
	if apply == nil {
		apply = update.Apply
	}
	result, err := apply(ctx, update.ApplyOptions{
		ManifestURL:    a.Config.ManifestURL,
		Channel:        a.Config.Updates.Channel,
		ConfigDir:      a.ConfigDir,
		ExecutablePath: a.ExecutablePath,
		CurrentVersion: version.Version,
	})
	if err != nil {
		return update.ApplyResult{}, err
	}
	if result.Applied {
		if err := a.AuditLog.Append(logs.AuditEntry{
			Action:   "update.apply",
			Target:   result.Version,
			Operator: a.operator(),
			Details:  result.BackupPath,
		}); err != nil {
			return update.ApplyResult{}, err
		}
	}
	return result, nil
}

func (a *App) ApplyFleetUpdate(ctx context.Context, serverNames []string) (FleetUpdateResult, error) {
	var controllerResult update.ApplyResult

	if IsHomebrewInstall(a.ExecutablePath) {
		// Controller is managed by Homebrew — skip self-update entirely.
		// Agents are still updated below.
		controllerResult = update.ApplyResult{
			Applied: false,
			Version: version.Version,
			Note:    "managed by Homebrew — run `brew upgrade cenvero-fleet` to update the controller",
		}
	} else {
		var err error
		controllerResult, err = a.ApplyUpdate(ctx)
		if err != nil {
			return FleetUpdateResult{}, err
		}
	}

	targets, err := a.updateTargets(serverNames)
	if err != nil {
		return FleetUpdateResult{}, err
	}

	result := FleetUpdateResult{
		Controller: controllerResult,
		Agents:     make([]FleetUpdateAgentResult, 0, len(targets)),
	}
	for _, server := range targets {
		targetResult := a.applyAgentUpdate(ctx, server)
		result.Agents = append(result.Agents, targetResult)
		result.Attempted++
		if targetResult.Error == "" {
			result.Succeeded++
		} else {
			result.Failed++
		}
	}
	return result, nil
}

func (a *App) applyAgentUpdate(ctx context.Context, server ServerRecord) FleetUpdateAgentResult {
	serviceName := agentServiceName(server)
	response, err := a.callRPC(server, proto.Envelope{
		Action: "update.apply",
		Payload: proto.UpdateApplyPayload{
			ManifestURL: a.Config.ManifestURL,
			Channel:     a.Config.Updates.Channel,
			ServiceName: serviceName,
		},
	})
	if err != nil {
		_ = a.AuditLog.Append(logs.AuditEntry{
			Action:   "agent.update.failed",
			Target:   server.Name,
			Operator: a.operator(),
			Details:  err.Error(),
		})
		return FleetUpdateAgentResult{
			Server:         server.Name,
			Channel:        a.Config.Updates.Channel,
			CurrentVersion: server.Observed.AgentVersion,
			ServiceName:    serviceName,
			Error:          err.Error(),
		}
	}
	if response.Error != nil {
		message := response.Error.Code + ": " + response.Error.Message
		_ = a.AuditLog.Append(logs.AuditEntry{
			Action:   "agent.update.failed",
			Target:   server.Name,
			Operator: a.operator(),
			Details:  message,
		})
		return FleetUpdateAgentResult{
			Server:         server.Name,
			Channel:        a.Config.Updates.Channel,
			CurrentVersion: server.Observed.AgentVersion,
			ServiceName:    serviceName,
			Error:          message,
		}
	}

	applied, err := proto.DecodePayload[proto.UpdateApplyResult](response.Payload)
	if err != nil {
		_ = a.AuditLog.Append(logs.AuditEntry{
			Action:   "agent.update.failed",
			Target:   server.Name,
			Operator: a.operator(),
			Details:  err.Error(),
		})
		return FleetUpdateAgentResult{
			Server:         server.Name,
			Channel:        a.Config.Updates.Channel,
			CurrentVersion: server.Observed.AgentVersion,
			ServiceName:    serviceName,
			Error:          err.Error(),
		}
	}

	if strings.TrimSpace(applied.Version) != "" {
		server.Observed.AgentVersion = applied.Version
		server.Observed.LastSeen = time.Now().UTC()
		server.Observed.LastError = ""
		server.Agent.UpdatedAt = time.Now().UTC()
		if server.Agent.ServiceName == "" && applied.ServiceName != "" {
			server.Agent.ServiceName = applied.ServiceName
		}
		_ = a.SaveServer(server)
	}

	details := fmt.Sprintf("version=%s applied=%t restart=%t", applied.Version, applied.Applied, applied.RestartScheduled)
	if applied.BackupPath != "" {
		details += " backup=" + applied.BackupPath
	}
	_ = a.AuditLog.Append(logs.AuditEntry{
		Action:   "agent.update.apply",
		Target:   server.Name,
		Operator: a.operator(),
		Details:  details,
	})

	return FleetUpdateAgentResult{
		Server:            server.Name,
		Channel:           applied.Channel,
		CurrentVersion:    applied.CurrentVersion,
		Version:           applied.Version,
		Applied:           applied.Applied,
		SHA256Verified:    applied.SHA256Verified,
		SignatureVerified: applied.SignatureVerified,
		RestartScheduled:  applied.RestartScheduled,
		ServiceName:       applied.ServiceName,
	}
}

func (a *App) updateTargets(serverNames []string) ([]ServerRecord, error) {
	if len(serverNames) == 0 {
		return a.ListServers()
	}
	targets := make([]ServerRecord, 0, len(serverNames))
	for _, name := range serverNames {
		server, err := a.GetServer(name)
		if err != nil {
			return nil, err
		}
		targets = append(targets, server)
	}
	return targets, nil
}

func agentServiceName(server ServerRecord) string {
	if server.Agent.ServiceName != "" {
		return server.Agent.ServiceName
	}
	if strings.EqualFold(server.User, defaultAgentUser) || server.Agent.Managed {
		return defaultServiceName
	}
	return ""
}

func (a *App) RollbackUpdate() (update.RollbackResult, error) {
	result, err := update.Rollback(a.ConfigDir, a.ExecutablePath)
	if err != nil {
		return update.RollbackResult{}, err
	}
	if result.Restored {
		if err := a.AuditLog.Append(logs.AuditEntry{
			Action:   "update.rollback",
			Target:   result.Version,
			Operator: a.operator(),
			Details:  result.RestoredFrom,
		}); err != nil {
			return update.RollbackResult{}, err
		}
	}
	return result, nil
}
