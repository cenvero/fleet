// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/cenvero/fleet/internal/safetext"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/version"
	"golang.org/x/crypto/ssh"
)

// A Notice is something about the controller that needs the operator's
// attention but is not the outcome of the command being run: an update is
// available, a config migration is pending, a background task failed.
// Ordinary commands (fleet ssh, fleet exec, …) no longer print these while
// they run, where they got in the way of the output and of scripts; `fleet
// errors` lists them, and bare `fleet` and `fleet status` say how many there
// are.
//
// Notices are derived from state the controller already keeps (the update
// check cache, config.toml, the agent-sync state, the daemon lock), never
// stored, so one goes away by itself once its cause is fixed and none can be
// dismissed while its cause remains. A command's own failure is never a
// notice: it is still reported by that command.
type Notice struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // "info", "warning" or "error"
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	// Fix is the command that resolves it, when there is one.
	Fix string `json:"fix,omitempty"`
}

// Notice severities, most to least urgent.
const (
	NoticeError   = "error"
	NoticeWarning = "warning"
	NoticeInfo    = "info"
)

// CollectNotices returns what needs attention for the controller in
// configDir, most urgent first. It reads only local state, except that the
// update check may refresh its cached manifest (at most every 10 minutes, the
// same cache the update notice always used). It never fails: a source it
// cannot read contributes nothing.
func CollectNotices(configDir string) []Notice {
	if !IsInitialized(configDir) {
		return nil
	}
	cfg, err := LoadConfigShared(ConfigPath(configDir))
	if err != nil {
		return []Notice{{
			ID: "config-unreadable", Severity: NoticeError,
			Title:  "config.toml cannot be read",
			Detail: safetext.Bound(safetext.Terminal(err.Error(), false), 300),
			Fix:    "fleet config validate",
		}}
	}
	var notices []Notice

	if key := filepath.Join(configDir, "keys", cfg.Crypto.PrimaryKey); cfg.Crypto.PassphraseProtected && cfg.Crypto.PrimaryKey != "" && keyNeedsPassphrase(key) {
		notices = append(notices, Notice{
			ID: "passphrase-keys", Severity: NoticeError,
			Title:  "The controller key is passphrase-protected, which Fleet does not support yet",
			Detail: "Connections to servers fail until the key can be used without a passphrase. Removing it asks for the current passphrase.",
			Fix:    fmt.Sprintf("ssh-keygen -p -N '' -f %s", key),
		})
	}

	if latest := UpdateAvailable(configDir, cfg.ManifestURL, cfg.Updates.Channel, cfg.Updates.Policy); latest != "" {
		notices = append(notices, Notice{
			ID: "update-available", Severity: NoticeWarning,
			Title: fmt.Sprintf("Cenvero Fleet %s is available (you have %s)", latest, version.Canonical(version.Version)),
			Fix:   UpgradeCommand(),
		})
	}

	if hint := AdjustInitHint(cfg); hint != "" {
		notices = append(notices, Notice{
			ID: "adjust-init", Severity: NoticeWarning,
			Title:  "The controller config needs updating",
			Detail: hint,
			Fix:    "fleet adjust-init",
		})
	}

	if state := ReadAgentSyncState(configDir); state.LastFailed > 0 {
		names := make([]string, 0, len(state.LastFailedNames))
		for _, n := range state.LastFailedNames {
			names = append(names, safetext.Bound(safetext.Terminal(n, false), 64))
		}
		detail := fmt.Sprintf("%d server(s) could not be brought to this controller's agent version", state.LastFailed)
		if len(names) > 0 {
			detail += ": " + strings.Join(names, ", ")
		}
		notices = append(notices, Notice{
			ID: "agent-sync-failed", Severity: NoticeWarning,
			Title:  "The last automatic agent sync failed for some servers",
			Detail: detail + ". Log: " + AgentSyncLogPath(configDir),
			Fix:    "fleet sync-agent auto status",
		})
	}

	notices = append(notices, helperNotices(configDir)...)

	if n := reverseServerCount(configDir, cfg); n > 0 && !DaemonStatus(configDir, cfg.Runtime.ControlAddress).Running {
		notices = append(notices, Notice{
			ID: "daemon-needed", Severity: NoticeInfo,
			Title:  fmt.Sprintf("%d reverse-mode server(s) need the controller daemon, which is not running", n),
			Detail: "Reverse-mode agents connect to the daemon; until it runs, commands for them fail.",
			Fix:    "fleet start",
		})
	}

	return notices
}

// helperNotices reports the background helper's problems: a failed start
// that no successful start has followed, and tasks whose last run failed.
func helperNotices(configDir string) []Notice {
	status, err := ReadHelperStatus(configDir)
	if err != nil {
		return nil
	}
	clean := func(s string) string { return safetext.Bound(safetext.Terminal(s, false), 300) }
	var out []Notice
	if status.LastLaunchError != "" && status.LastLaunchErrorAt.After(status.StartedAt) && !HelperRunning(configDir) {
		out = append(out, Notice{
			ID: "helper-not-running", Severity: NoticeError,
			Title:  "The background helper could not start, so jobs are not tracked and agents are not synced",
			Detail: clean(status.LastLaunchError) + ". Log: " + HelperLogPath(configDir),
			Fix:    "fleet helper restart",
		})
	}
	names := make([]string, 0, len(status.Tasks))
	for name := range status.Tasks {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		task := status.Tasks[name]
		if task.LastResult != HelperResultError {
			continue
		}
		out = append(out, Notice{
			ID: "helper-task-" + safetext.Bound(safetext.Terminal(name, false), 40), Severity: NoticeWarning,
			Title:  fmt.Sprintf("The background %q task failed its last run", clean(name)),
			Detail: clean(task.LastError),
			Fix:    "fleet helper status",
		})
	}
	return out
}

// keyNeedsPassphrase reports whether the private key at path is encrypted.
// A key that is missing or unreadable is not reported here: connecting
// reports that itself.
func keyNeedsPassphrase(path string) bool {
	data, err := os.ReadFile(path) // #nosec G304 -- the controller's own key, inside its config dir
	if err != nil {
		return false
	}
	_, err = ssh.ParseRawPrivateKey(data)
	var missing *ssh.PassphraseMissingError
	return errors.As(err, &missing)
}

// reverseServerCount counts servers that reach the controller in reverse
// mode, reading the records directly (no App, no database), like shell
// completion does.
func reverseServerCount(configDir string, cfg Config) int {
	dir := filepath.Join(configDir, "servers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		var s ServerRecord
		if _, err := toml.DecodeFile(filepath.Join(dir, entry.Name()), &s); err != nil {
			continue
		}
		mode := s.Mode
		if mode == "" || mode == transport.ModePerNode {
			mode = cfg.DefaultMode
		}
		if mode == transport.ModeReverse {
			n++
		}
	}
	return n
}
