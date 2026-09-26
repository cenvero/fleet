// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// homebrewHintCache is data/update-available.json. CheckedAt/Latest are the
// original fields (the last SUCCESSFUL check and what it found) and keep their
// meaning, so older binaries keep reading the file correctly. The remaining
// fields are additive (older binaries ignore them): they record the last fetch
// attempt, successful or not, so a failing/unreachable manifest is retried only
// once per updateCheckInterval instead of on every command.
type homebrewHintCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`

	AttemptedAt time.Time `json:"attempted_at,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// updateCheckInterval is how often the daemon refreshes the cached "is a newer
// release available" result — and the TTL the CLI honors for the same cache
// (including after a failed attempt), so neither hammers the CDN.
const updateCheckInterval = 10 * time.Minute

// cliUpdateCheckBudget caps the one synchronous manifest fetch a CLI command
// may make when the cache is stale: a single attempt, no retries. An offline or
// firewalled controller pays this at most once per updateCheckInterval.
const cliUpdateCheckBudget = 1500 * time.Millisecond

// daemonUpdateCheckBudget bounds the daemon's background refresh, which may use
// update.Fetch's retries since nobody is waiting on it.
const daemonUpdateCheckBudget = 30 * time.Second

// Manifest fetchers, replaceable in tests.
var (
	fetchManifestOnce  = update.FetchOnce
	fetchManifestRetry = update.Fetch
)

const (
	ansiYellow = "\033[33m"
	ansiReset  = "\033[0m"
)

// UpdateAvailable returns the latest version for the CONFIGURED channel when it is
// strictly newer than the running binary, or "" if up to date, disabled, a
// dev/unversioned build, or the manifest is unreachable. The manifest result is
// cached for updateCheckInterval (data/update-available.json); when the cache is
// stale, one single-attempt fetch bounded by cliUpdateCheckBudget refreshes it,
// and a failed attempt is recorded so the next one waits a full interval.
func UpdateAvailable(configDir, manifestURL, channel string, policy update.Policy) string {
	return updateAvailable(configDir, manifestURL, channel, policy, false)
}

// updateAvailable implements UpdateAvailable. daemon=true is the background
// refresher (runUpdateChecker): it fetches with retries and a longer budget.
func updateAvailable(configDir, manifestURL, channel string, policy update.Policy, daemon bool) string {
	if policy == update.PolicyDisabled {
		return ""
	}
	cur := strings.TrimSpace(version.Version)
	if cur == "" || cur == "dev" {
		return "" // a dev/unversioned build is never "out of date"
	}
	if strings.TrimSpace(channel) == "" {
		channel = "stable"
	}
	cacheFile := filepath.Join(configDir, "data", "update-available.json")
	var cache homebrewHintCache
	if data, err := os.ReadFile(cacheFile); err == nil { // #nosec G304 -- cache path is fixed beneath the controller data directory
		_ = json.Unmarshal(data, &cache)
	}
	due := updateCheckDue(cache, time.Now())
	if daemon {
		// The daemon's ticker already spaces its calls one interval apart, so it
		// refreshes whenever the last SUCCESS is (almost) an interval old — the
		// slack keeps tick jitter from skipping every other refresh — and it
		// keeps retrying on each tick while the manifest is unreachable.
		due = time.Since(cache.CheckedAt) > updateCheckInterval-time.Minute
	}
	if due {
		fetch, budget := fetchManifestOnce, cliUpdateCheckBudget
		if daemon {
			fetch, budget = fetchManifestRetry, daemonUpdateCheckBudget
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		manifest, err := fetch(ctx, manifestURL)
		cancel()
		now := time.Now().UTC()
		cache.AttemptedAt = now
		if err == nil {
			if ch, ok := manifest.Channels[channel]; ok {
				cache.CheckedAt, cache.Latest = now, ch.Version
				cache.Failures, cache.LastError = 0, ""
			} else {
				err = fmt.Errorf("channel %q not in manifest", channel)
			}
		}
		if err != nil {
			// Keep the last known Latest (and CheckedAt): a transient failure must
			// not hide a notice we already know about.
			cache.Failures++
			cache.LastError = truncateUpdateError(err.Error())
		}
		writeUpdateCache(cacheFile, cache)
	}
	if cache.Latest != "" && isNewerVersion(cache.Latest, version.Version) {
		return cache.Latest
	}
	return ""
}

// updateCheckDue reports whether the CLI should fetch the manifest now: the last
// attempt (or, for caches written by older binaries without attempted_at, the
// last success) is older than updateCheckInterval.
func updateCheckDue(cache homebrewHintCache, now time.Time) bool {
	last := cache.AttemptedAt
	if cache.CheckedAt.After(last) {
		last = cache.CheckedAt
	}
	return now.Sub(last) > updateCheckInterval
}

func truncateUpdateError(msg string) string {
	const limit = 300
	if len(msg) > limit {
		return msg[:limit]
	}
	return msg
}

// writeUpdateCache replaces the cache file atomically (temp file + rename), so
// concurrent fleet processes never observe a torn document. Best-effort.
func writeUpdateCache(path string, cache homebrewHintCache) {
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".update-available-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
	}
}

// AgentVersionMismatch names a managed server whose last-observed agent version
// differs from this controller's version.
type AgentVersionMismatch struct {
	Server       string `json:"server"`
	AgentVersion string `json:"agent_version"`
}

// agentSupportsUnattendedUpdateActivation reports whether Fleet can replace,
// restart, and subsequently observe the managed agent without operator action.
// Windows replacement is supported, but service restart/reverification is not;
// macOS also has no managed service restart path today. Keep unattended updates
// Linux-only until those activation paths exist.
func agentSupportsUnattendedUpdateActivation(goos string) bool {
	return strings.EqualFold(strings.TrimSpace(goos), "linux")
}

// AgentsNeedingSync returns the managed servers whose last-observed agent
// version differs from the controller's, so the caller can nudge the operator
// to run `fleet sync-agent`. It reads only the versions already stored from
// prior connections — no network calls. Servers we have never observed a
// version for (empty AgentVersion) are skipped: we can't claim they're stale.
// When the controller itself is a "dev" build, every comparison is noise, so it
// returns nil. Versions compare canonically (leading-v / whitespace-insensitive).
func (a *App) AgentsNeedingSync() ([]AgentVersionMismatch, error) {
	controller := version.Canonical(version.Version)
	if version.Version == "dev" || controller == "" {
		return nil, nil
	}
	servers, err := a.ListServers()
	if err != nil {
		return nil, err
	}
	var out []AgentVersionMismatch
	for _, s := range servers {
		av := strings.TrimSpace(s.Observed.AgentVersion)
		if av == "" {
			continue
		}
		if version.Canonical(av) != controller {
			out = append(out, AgentVersionMismatch{Server: s.Name, AgentVersion: av})
		}
	}
	return out, nil
}

// UpdateNotice returns a ready-to-print update notice (with the correct upgrade
// command for the install method) when a newer release is available on the
// configured channel, or "" otherwise. When color is true it is wrapped in ANSI
// yellow.
func UpdateNotice(configDir, manifestURL, channel string, policy update.Policy, color bool) string {
	latest := UpdateAvailable(configDir, manifestURL, channel, policy)
	if latest == "" {
		return ""
	}
	msg := fmt.Sprintf("⬆  Update your fleet — Cenvero Fleet %s is available (you have %s). Upgrade with:\n     %s",
		latest, version.Canonical(version.Version), UpgradeCommand())
	if color {
		return ansiYellow + msg + ansiReset
	}
	return msg
}

// runUpdateChecker periodically refreshes the cached "newer release available"
// result on the user's configured channel, so the daemon keeps it warm and any
// CLI command (or the next dashboard render) can surface the yellow update
// notice even with no interactive activity. It logs once per newly-seen version.
func (a *App) runUpdateChecker(ctx context.Context) {
	if a.Config.Updates.Policy == update.PolicyDisabled {
		return
	}
	var lastSeen string
	check := func() {
		latest := updateAvailable(a.ConfigDir, a.Config.ManifestURL, a.Config.Updates.Channel, a.Config.Updates.Policy, true)
		if latest != "" && latest != lastSeen {
			lastSeen = latest
			_ = a.AuditLog.Append(logs.AuditEntry{
				Action:   "update.available",
				Target:   "controller",
				Operator: a.operator(),
				Details:  fmt.Sprintf("%s available on channel %q (running %s); upgrade: %s", latest, a.Config.Updates.Channel, version.Version, UpgradeCommand()),
			})
		}
	}
	check()
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// isNewerVersion returns true only when both inputs are strict SemVer values and
// candidate has greater SemVer precedence. Malformed inputs fail closed.
func isNewerVersion(candidate, current string) bool {
	comparison, err := update.CompareSemVer(candidate, current)
	return err == nil && comparison > 0
}

func (a *App) ApplyUpdate(ctx context.Context, allowUnsigned, allowDowngrade bool) (update.ApplyResult, error) {
	apply := a.ControllerUpdater
	if apply == nil {
		apply = update.Apply
	}
	currentVersion := version.Version
	if currentVersion == "dev" {
		currentVersion = ""
	}
	result, err := apply(ctx, update.ApplyOptions{
		ManifestURL:    a.Config.ManifestURL,
		Channel:        a.Config.Updates.Channel,
		ConfigDir:      a.ConfigDir,
		ExecutablePath: a.ExecutablePath,
		CurrentVersion: currentVersion,
		AllowUnsigned:  allowUnsigned,
		AllowDowngrade: allowDowngrade,
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

func (a *App) ApplyFleetUpdate(ctx context.Context, serverNames []string, allowUnsigned, allowDowngrade bool) (FleetUpdateResult, error) {
	var controllerResult update.ApplyResult

	executablePath := a.ExecutablePath
	if executablePath == "" {
		executablePath, _ = os.Executable()
	}
	manager := DetectInstallManager(executablePath)
	if manager.ManagesController() {
		// The package manager owns the controller executable. Never mutate it
		// behind the manager's package database; managed agents remain a separate
		// Fleet-owned rollout below.
		controllerResult = update.ApplyResult{
			Applied: false,
			Version: version.Version,
			Note: fmt.Sprintf("managed by %s — run `%s` to update the controller",
				manager.DisplayName(), manager.UpgradeCommand()),
		}
	} else {
		var err error
		controllerResult, err = a.ApplyUpdate(ctx, allowUnsigned, allowDowngrade)
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
		if applied.Applied {
			server.Agent.UpdatedAt = time.Now().UTC()
		}
		// A replacement without a managed restart is only delivered, not yet
		// observed live. Preserve the prior observed version until reconnect.
		if !applied.Applied || applied.RestartScheduled {
			server.Observed.AgentVersion = applied.Version
			server.Observed.LastSeen = time.Now().UTC()
			server.Observed.LastError = ""
		}
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
	Server            string `json:"server"`
	AgentVersion      string `json:"agent_version"`
	WantedVersion     string `json:"wanted_version"`
	AlreadySynced     bool   `json:"already_synced,omitempty"`
	Updated           bool   `json:"updated,omitempty"`
	RestartHandled    bool   `json:"restart_handled,omitempty"`
	ActivationPending bool   `json:"activation_pending,omitempty"`
	Error             string `json:"error,omitempty"`
}

type FleetSyncAgentResult struct {
	ControllerVersion string            `json:"controller_version"`
	Agents            []SyncAgentResult `json:"agents"`
	Synced            int               `json:"synced"`
	AlreadyUpToDate   int               `json:"already_up_to_date"`
	Failed            int               `json:"failed"`
}

// syncAgentParallelism bounds how many servers SyncAgent updates concurrently.
const syncAgentParallelism = 8

// SyncAgentProgress is one streamed status update for a single server during a
// SyncAgent run. State is one of: "start", "updated", "pending-activation",
// "uptodate", "error".
type SyncAgentProgress struct {
	Server string
	State  string
	From   string // current agent version
	To     string // wanted (controller) version
	Err    string
}

// SyncAgent checks whether each managed server's agent version matches the
// controller version and, if not, triggers verified update delivery. Platforms
// with a managed restart path activate automatically; others report activation
// as pending. Pass serverNames=nil to target every registered server.
//
// Servers are synced CONCURRENTLY (bounded by syncAgentParallelism) so a large
// fleet updates in parallel rather than one-at-a-time — but SYNCHRONOUSLY: the
// call waits for every server before returning, so there are no detached or
// orphaned updates and the caller always gets the complete result. The optional
// progress callback streams per-server status as each server starts/finishes;
// it is invoked serially (never concurrently), so callers need no locking.
func (a *App) SyncAgent(ctx context.Context, serverNames []string, progress func(SyncAgentProgress)) (FleetSyncAgentResult, error) {
	targets, err := a.updateTargets(serverNames)
	if err != nil {
		return FleetSyncAgentResult{}, err
	}

	result := FleetSyncAgentResult{
		ControllerVersion: version.Canonical(version.Version),
		Agents:            make([]SyncAgentResult, 0, len(targets)),
	}

	var (
		mu         sync.Mutex // guards result aggregation
		progressMu sync.Mutex // serializes progress callbacks
		wg         sync.WaitGroup
		sem        = make(chan struct{}, syncAgentParallelism)
	)
	emit := func(p SyncAgentProgress) {
		if progress == nil {
			return
		}
		progressMu.Lock()
		progress(p)
		progressMu.Unlock()
	}

	for _, server := range targets {
		wg.Add(1)
		go func(server ServerRecord) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			emit(SyncAgentProgress{Server: server.Name, State: "start"})
			r := a.syncAgentOne(ctx, server)

			mu.Lock()
			result.Agents = append(result.Agents, r)
			switch {
			case r.Error != "":
				result.Failed++
			case r.AlreadySynced:
				result.AlreadyUpToDate++
			default:
				result.Synced++
			}
			mu.Unlock()

			p := SyncAgentProgress{Server: server.Name, From: r.AgentVersion, To: r.WantedVersion}
			switch {
			case r.Error != "":
				p.State, p.Err = "error", r.Error
			case r.AlreadySynced:
				p.State = "uptodate"
			case r.ActivationPending:
				p.State = "pending-activation"
			default:
				p.State = "updated"
			}
			emit(p)
		}(server)
	}
	wg.Wait()
	// The goroutines append out of order; sort by server for deterministic output.
	sort.Slice(result.Agents, func(i, j int) bool { return result.Agents[i].Server < result.Agents[j].Server })
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
	base.Updated = applied.Applied
	base.RestartHandled = applied.RestartScheduled
	base.ActivationPending = applied.Applied && !applied.RestartScheduled
	if !base.ActivationPending {
		base.AgentVersion = applied.Version
	}
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
