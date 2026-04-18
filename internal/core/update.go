// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	if cache.Latest != "" && isNewerVersion(cache.Latest, version.Version) {
		return cache.Latest
	}
	return ""
}

// isNewerVersion returns true if candidate is strictly newer than current.
// Both strings may or may not carry a leading "v".
func isNewerVersion(candidate, current string) bool {
	return semverCompare(candidate, current) > 0
}

func semverCompare(a, b string) int {
	av := strings.TrimPrefix(a, "v")
	bv := strings.TrimPrefix(b, "v")
	ap := strings.SplitN(av, "-", 2)
	bp := strings.SplitN(bv, "-", 2)
	ac := strings.Split(ap[0], ".")
	bc := strings.Split(bp[0], ".")
	for len(ac) < 3 {
		ac = append(ac, "0")
	}
	for len(bc) < 3 {
		bc = append(bc, "0")
	}
	for i := 0; i < 3; i++ {
		an, _ := strconv.Atoi(ac[i])
		bn, _ := strconv.Atoi(bc[i])
		if an != bn {
			if an > bn {
				return 1
			}
			return -1
		}
	}
	// Numeric parts are equal. A release (no pre-release suffix) is newer than
	// any pre-release of the same version: 1.0.0 > 1.0.0-alpha > 1.0.0-beta.
	aHasPre := len(ap) > 1 && ap[1] != ""
	bHasPre := len(bp) > 1 && bp[1] != ""
	switch {
	case !aHasPre && bHasPre:
		return 1 // a is a release, b is a pre-release
	case aHasPre && !bHasPre:
		return -1 // a is a pre-release, b is a release
	case aHasPre && bHasPre:
		return strings.Compare(ap[1], bp[1]) // lexicographic pre-release comparison
	}
	return 0
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
		// Best-effort: the update was already applied; don't fail the call if
		// the audit log write fails (e.g. disk full after the binary replace).
		_ = a.AuditLog.Append(logs.AuditEntry{
			Action:   "update.apply",
			Target:   result.Version,
			Operator: a.operator(),
			Details:  result.BackupPath,
		})
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

type SyncAgentResult struct {
	Server         string `json:"server"`
	AgentVersion   string `json:"agent_version"`
	WantedVersion  string `json:"wanted_version"`
	AlreadySynced  bool   `json:"already_synced,omitempty"`
	Updated        bool   `json:"updated,omitempty"`
	RestartHandled bool   `json:"restart_handled,omitempty"`
	Error          string `json:"error,omitempty"`
}

type FleetSyncAgentResult struct {
	ControllerVersion string            `json:"controller_version"`
	Agents            []SyncAgentResult `json:"agents"`
	Synced            int               `json:"synced"`
	AlreadyUpToDate   int               `json:"already_up_to_date"`
	Failed            int               `json:"failed"`
}

// SyncAgent checks whether each managed server's agent version matches the
// controller version and, if not, triggers an update + service restart.
// Pass serverNames=nil to target every registered server.
func (a *App) SyncAgent(ctx context.Context, serverNames []string) (FleetSyncAgentResult, error) {
	targets, err := a.updateTargets(serverNames)
	if err != nil {
		return FleetSyncAgentResult{}, err
	}

	result := FleetSyncAgentResult{
		ControllerVersion: version.Canonical(version.Version),
		Agents:            make([]SyncAgentResult, 0, len(targets)),
	}

	for _, server := range targets {
		r := a.syncAgentOne(ctx, server)
		result.Agents = append(result.Agents, r)
		switch {
		case r.Error != "":
			result.Failed++
		case r.AlreadySynced:
			result.AlreadyUpToDate++
		default:
			result.Synced++
		}
	}
	return result, nil
}

func (a *App) syncAgentOne(ctx context.Context, server ServerRecord) SyncAgentResult {
	agentVer := version.Canonical(strings.TrimSpace(server.Observed.AgentVersion))
	want := version.Canonical(version.Version)

	base := SyncAgentResult{
		Server:        server.Name,
		AgentVersion:  agentVer,
		WantedVersion: want,
	}

	if agentVer != "" && !isNewerVersion(want, agentVer) && !isNewerVersion(agentVer, want) {
		base.AlreadySynced = true
		return base
	}

	applied := a.applyAgentUpdate(ctx, server)
	if applied.Error != "" {
		base.Error = applied.Error
		return base
	}
	base.AgentVersion = applied.Version
	base.Updated = applied.Applied
	base.RestartHandled = applied.RestartScheduled
	return base
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
